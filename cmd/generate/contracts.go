package generate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	resources "github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/spf13/cobra"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"
)

var (
	contractsOutput      string
	contractsCheck       bool
	contractsPermissions bool
	contractsFormat      string
)

// yamlStringTag is the yaml.v3 tag for a plain string scalar node.
const yamlStringTag = "!!str"

// ContractsCmd exports every endpoint a module's interface declares into
// contracts/api and writes the catalog the module package carries.
var ContractsCmd = &cobra.Command{
	Use:   "contracts [module]",
	Short: "Export a module's interface endpoints as API contracts into its package",
	Long: `Export the API contract of every endpoint a module's interface exposes into
contracts/api and write the catalog (contracts/api/catalog.codefly.json), so the
module package carries the contract.

gRPC and connect endpoints get a serialized FileDescriptorSet (contract.binpb)
plus a copy of the service's proto sources; REST endpoints get their OpenAPI
document (openapi.json). An HTTP endpoint gets the OpenAPI document its service
carries (openapi/api.swagger.json), when it carries one: a solution backend
publishes its routes there, each with its x-codefly-authority-policy. TCP and
MCP endpoints have no machine-readable contract and are skipped, as are an HTTP
endpoint whose service carries no document and a connect endpoint whose
service has no proto.

For restricted listeners, module contracts.codefly.yaml maps service names under
surfaces to service-relative JSON procedure inventories. This is CLI publication
configuration, separate from the agent's spec. Multiple exported protobuf
endpoints require an inventory; a full descriptor alone does not establish what
each listener serves. An inventory may include implemented interfaces from other
protobuf packages; every named procedure must exist in the descriptor. For such
multi-package endpoints, package is empty and services[].fullName identifies
each package. See docs/commands.md#generate-contracts.

Run it before module-package build; the package carries the result. --check
is the CI drift gate: it regenerates into a temporary directory and compares
against what's on disk, without writing anything.
`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		// This command loads service builders to read their endpoints
		// (generators.LoadServiceEndpoints -> instance.LoadBuilder), which
		// spawns an agent subprocess per service. Closing the cached
		// connections is what ends them: the agent's own Serve installs a
		// SIGTERM handler and shuts down inside the parent's window. Without
		// this, every `generate contracts` pass left its agents running — eight
		// live private go-grpc records accumulated across two regenerations of
		// one workspace. `generate client` already reaps the same way.
		defer services.ClearAgents()

		if contractsFormat != "text" && contractsFormat != "json" {
			return fmt.Errorf("--format must be text or json, got %q", contractsFormat)
		}

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}

		var module *resources.Module
		if len(args) == 1 {
			module, err = workspace.LoadModuleFromName(ctx, args[0])
		} else {
			module, err = common.LoadModule(ctx)
		}
		if err != nil {
			return err
		}

		output := contractsOutput
		if output == "" {
			output = filepath.Join(module.Dir(), "contracts", "api")
		} else if output, err = shared.SolvePath(output); err != nil {
			return fmt.Errorf("cannot solve output path: %w", err)
		}

		if contractsCheck {
			if err = runContractsCheck(ctx, workspace, module, output); err != nil {
				return err
			}
			if contractsPermissions {
				return generatePermissions(ctx, module, "", true)
			}
			return nil
		}

		catalog, err := generateContracts(ctx, workspace, module, generateOptions{
			canonicalOutput: output,
			writeDir:        output,
			updateManifest:  true,
		})
		if err != nil {
			return err
		}

		if contractsPermissions {
			if err = generatePermissions(ctx, module, "", false); err != nil {
				return err
			}
		}
		if contractsFormat == "json" {
			data, err := catalog.CanonicalBytes()
			if err != nil {
				return err
			}
			fmt.Println(string(data))
		}
		cli.Header(1, "Contracts generated successfully!")
		return nil
	},
}

func init() {
	ContractsCmd.Flags().BoolVar(&contractsPermissions, "permissions", false, "also generate typed permission constants; with --check, verify their freshness")
	ContractsCmd.Flags().StringVar(&contractsOutput, "output", "", "output directory for generated contracts (default: <module dir>/contracts/api)")
	ContractsCmd.Flags().BoolVar(&contractsCheck, "check", false, "do not write; exit 1 if the on-disk catalog differs from what would be generated")
	ContractsCmd.Flags().StringVar(&contractsFormat, "format", "text", "output format: text|json")
}

// generateOptions separates where generated bytes are canonically reported to
// live (canonicalOutput, used to compute each contract's module-relative Path)
// from where they are physically written (writeDir). --check stages bytes in a
// throwaway directory while still recording the Path a real run would produce,
// so the resulting catalog is diffable against the real output.
type generateOptions struct {
	canonicalOutput string
	writeDir        string
	updateManifest  bool
}

// generateContracts implements the command's core behaviour: load the
// module's exported endpoints, load each owning service's Builder only when
// at least one of its exported endpoints needs one, write the contract files,
// and assemble + write the catalog (and, when requested, the package
// manifest's api-contracts).
func generateContracts(ctx context.Context, workspace *resources.Workspace, module *resources.Module, opts generateOptions) (*composition.APIContractCatalog, error) {
	endpoints, err := module.ExportedEndpointsForPackage(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w; add an interface: block to module.codefly.yaml to export API contracts", err)
	}

	serviceNames, byService := exportedEndpointsByService(endpoints)
	surfacePaths, err := loadContractSurfacePaths(module)
	if err != nil {
		return nil, err
	}

	if _, err = shared.CheckDirectoryOrCreate(ctx, opts.writeDir); err != nil {
		return nil, fmt.Errorf("cannot create output directory: %w", err)
	}

	var contractEndpoints []composition.APIContractEndpoint
	for _, serviceName := range serviceNames {
		exported := byService[serviceName]

		if !anyCarriesContract(exported) {
			for _, endpoint := range exported {
				cli.Info("endpoint %s/%s (api %s) has no contract; skipped", serviceName, endpoint.Name, endpoint.Api)
			}
			continue
		}

		service, loadErr := module.LoadServiceFromName(ctx, serviceName)
		if loadErr != nil {
			return nil, fmt.Errorf("cannot load service %s: %w", serviceName, loadErr)
		}

		// A connect endpoint is HTTP-shaped and only carries a protobuf
		// contract when its service is proto-backed; core does not guarantee it
		// is. A connect endpoint on a service with no proto therefore has no
		// machine-readable contract and is skipped like http/tcp, instead of
		// failing the whole export when the descriptor build has nothing to
		// compile. A rest endpoint is the same shape: an interface may export it
		// so composed modules can reach it (a gateway's REST surface, say)
		// without the service carrying an OpenAPI document, and such an export
		// is reachability, not a contract — skipped, never a failure. grpc
		// always carries a contract, so a missing proto there stays a hard
		// error (surfaced by writeProtobufContract).
		serviceHasProto, _ := shared.FileExists(ctx, filepath.Join(service.Dir(), "proto", "buf.yaml"))
		// A rest endpoint's contract is the OpenAPI document its service carries
		// where core loads it (standards.OpenAPIPath). The exported endpoint is
		// read from the module's interface and carries no API details, so it
		// cannot answer this; asking it skipped every REST face. An http
		// endpoint's is the same document, when its service carries one: a
		// solution backend serves plain HTTP and publishes its routes there.
		serviceHasOpenAPI, _ := shared.FileExists(ctx, filepath.Join(service.Dir(), standards.OpenAPIPath))

		carriers := contractCarriers(ctx, serviceName, exported, serviceHasProto, serviceHasOpenAPI)
		if len(carriers) == 0 {
			continue
		}
		surfaces, surfaceErr := loadContractSurfaces(service, carriers, surfacePaths[serviceName])
		if surfaceErr != nil {
			return nil, fmt.Errorf("module %s: %w", module.Name, surfaceErr)
		}

		// The agent reports each endpoint's API details at Load. An http
		// endpoint has none to report (its document is the service's file), so
		// a service whose carriers are all http is not loaded.
		var loadedByName map[string]*basev0.Endpoint
		for _, endpoint := range carriers {
			if endpoint.Api == standards.HTTP || loadedByName != nil {
				continue
			}
			loaded, loadErr := generators.LoadServiceEndpoints(ctx, workspace, module, service)
			if loadErr != nil {
				return nil, fmt.Errorf("cannot load endpoints for service %s: %w", serviceName, loadErr)
			}
			loadedByName = make(map[string]*basev0.Endpoint, len(loaded))
			for _, endpoint := range loaded {
				loadedByName[endpoint.Name] = endpoint
			}
		}

		for _, endpoint := range carriers {
			actual, ok := endpoint, true
			if endpoint.Api != standards.HTTP {
				actual, ok = loadedByName[endpoint.Name]
			}
			if !ok {
				return nil, fmt.Errorf("service %s does not report endpoint %q at Builder.Load", serviceName, endpoint.Name)
			}
			contractEndpoint, writeErr := writeEndpointContract(ctx, module, service, actual, surfaces[endpoint.Name], opts)
			if writeErr != nil {
				return nil, fmt.Errorf("cannot write contract for %s/%s: %w", serviceName, endpoint.Name, writeErr)
			}
			contractEndpoints = append(contractEndpoints, *contractEndpoint)
			printContractLine(contractEndpoint)
		}
	}

	return writeContractCatalog(ctx, module, contractEndpoints, opts)
}

// exportedEndpointsByService groups the exported endpoints by owning service,
// the services sorted.
func exportedEndpointsByService(endpoints []*basev0.Endpoint) ([]string, map[string][]*basev0.Endpoint) {
	var serviceNames []string
	byService := map[string][]*basev0.Endpoint{}
	for _, endpoint := range endpoints {
		if _, seen := byService[endpoint.Service]; !seen {
			serviceNames = append(serviceNames, endpoint.Service)
		}
		byService[endpoint.Service] = append(byService[endpoint.Service], endpoint)
	}
	sort.Strings(serviceNames)
	return serviceNames, byService
}

func anyCarriesContract(exported []*basev0.Endpoint) bool {
	for _, endpoint := range exported {
		if endpointCarriesContract(endpoint.Api) {
			return true
		}
	}
	return false
}

// contractCarriers keeps the exported endpoints that carry a machine-readable
// contract, saying why each of the others is skipped.
func contractCarriers(_ context.Context, serviceName string, exported []*basev0.Endpoint, serviceHasProto, serviceHasOpenAPI bool) []*basev0.Endpoint {
	var carriers []*basev0.Endpoint
	for _, endpoint := range exported {
		switch {
		case !endpointCarriesContract(endpoint.Api):
			cli.Info("endpoint %s/%s (api %s) has no contract; skipped", serviceName, endpoint.Name, endpoint.Api)
		case endpoint.Api == standards.CONNECT && !serviceHasProto:
			cli.Info("endpoint %s/%s (api connect) has no proto; skipped", serviceName, endpoint.Name)
		case endpoint.Api == standards.REST && !serviceHasOpenAPI:
			cli.Info("endpoint %s/%s (api rest) has no OpenAPI document; exported for reachability only, skipped", serviceName, endpoint.Name)
		case endpoint.Api == standards.HTTP && !serviceHasOpenAPI:
			cli.Info("endpoint %s/%s (api http) has no OpenAPI document; skipped", serviceName, endpoint.Name)
		default:
			carriers = append(carriers, endpoint)
		}
	}
	return carriers
}

// writeContractCatalog assembles the catalog, validates it, writes it and,
// when requested, the package manifest's api-contracts.
func writeContractCatalog(ctx context.Context, module *resources.Module, contractEndpoints []composition.APIContractEndpoint, opts generateOptions) (*composition.APIContractCatalog, error) {
	pkg, version, manifest, err := resolvePackageIdentity(module)
	if err != nil {
		return nil, err
	}

	catalog := &composition.APIContractCatalog{
		Schema:    composition.APIContractCatalogSchema,
		Package:   pkg,
		Version:   version,
		Endpoints: contractEndpoints,
	}
	if err = catalog.Validate(); err != nil {
		return nil, fmt.Errorf("generated catalog is invalid: %w", err)
	}
	if opts.updateManifest && manifest != nil {
		if err = requireContractArtifactRoots(module, manifest, contractEndpoints); err != nil {
			return nil, err
		}
	}

	catalogBytes, err := catalog.CanonicalBytes()
	if err != nil {
		return nil, err
	}
	if err := shared.WriteFileAtomic(ctx, filepath.Join(opts.writeDir, "catalog.codefly.json"), catalogBytes, 0o644); err != nil {
		return nil, err
	}

	if opts.updateManifest && manifest != nil {
		if err := updateManifestFile(ctx, module, manifest, contractEndpoints); err != nil {
			return nil, fmt.Errorf("cannot update %s: %w", composition.PackageManifestFileName, err)
		}
		if err := composition.ValidatePackageAPIContracts(module.Dir(), manifest, catalog); err != nil {
			return nil, err
		}
	}

	return catalog, nil
}

// requireContractArtifactRoots refuses, before the catalog or the package
// manifest is written, a manifest whose artifact-roots do not cover the
// contract files this run records in it: the package would not carry them,
// and composition.ValidatePackageAPIContracts would refuse the manifest only
// after it had been rewritten. The refusal names the manifest and the root to
// add — the top directory of the first uncovered contract path.
func requireContractArtifactRoots(module *resources.Module, manifest *composition.PackageManifest, contractEndpoints []composition.APIContractEndpoint) error {
	for i := range contractEndpoints {
		endpoint := &contractEndpoints[i]
		covered := false
		for _, root := range manifest.ArtifactRoots {
			if endpoint.Path == root || strings.HasPrefix(endpoint.Path, root+"/") {
				covered = true
				break
			}
		}
		if !covered {
			root, _, _ := strings.Cut(endpoint.Path, "/")
			return fmt.Errorf("%w: package manifest %s does not declare the artifact root %q, so the package cannot carry the %s/%s contract at %s; add %q under artifact-roots",
				composition.ErrContract, filepath.Join(module.Dir(), composition.PackageManifestFileName), root, endpoint.Service, endpoint.Endpoint, endpoint.Path, root)
		}
	}
	return nil
}

// resolvePackageIdentity returns the catalog's Package/Version and, when the
// module has one, its package manifest. A module without a manifest is not
// packageable yet, so it falls back to a synthetic identity and a warning
// instead of failing the whole export.
func resolvePackageIdentity(module *resources.Module) (string, string, *composition.PackageManifest, error) {
	manifest, err := composition.LoadPackageManifest(module.Dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cli.Warning("module %s has no %s; it is not packageable yet", module.Name, composition.PackageManifestFileName)
			return "codefly/" + module.Name, "0.0.0", nil, nil
		}
		return "", "", nil, err
	}
	return manifest.ID, manifest.Version, manifest, nil
}

// endpointCarriesContract reports whether an endpoint's API can be exported as
// a machine-readable contract: grpc and connect resolve to a protobuf
// FileDescriptorSet, rest and http to an OpenAPI document (http only when its
// service carries one, which contractCarriers decides). tcp and mcp have none.
func endpointCarriesContract(api string) bool {
	return api == standards.GRPC || api == standards.CONNECT || api == standards.REST || api == standards.HTTP
}

// writeEndpointContract writes one endpoint's contract files and returns its
// catalog entry.
func writeEndpointContract(ctx context.Context, module *resources.Module, service *resources.Service, endpoint *basev0.Endpoint, surface []composition.APIContractService, opts generateOptions) (*composition.APIContractEndpoint, error) {
	physicalDir := filepath.Join(opts.writeDir, service.Name, endpoint.Name)
	canonicalDir := filepath.Join(opts.canonicalOutput, service.Name, endpoint.Name)
	if err := os.RemoveAll(physicalDir); err != nil {
		return nil, fmt.Errorf("cannot clear %s: %w", physicalDir, err)
	}
	if err := os.MkdirAll(physicalDir, 0o755); err != nil {
		return nil, err
	}

	if endpoint.Api == standards.CONNECT || resources.IsGRPC(ctx, endpoint) != nil {
		return writeProtobufContract(ctx, module, service, endpoint, surface, physicalDir, canonicalDir)
	}
	if rest := resources.IsRest(ctx, endpoint); rest != nil {
		return writeRestContract(ctx, module, endpoint, rest, physicalDir, canonicalDir)
	}
	if endpoint.Api == standards.HTTP {
		// An http endpoint reports no API details at Load, so its document is
		// read where the service carries it, by core's own REST loader: the
		// same bytes and routes a rest endpoint's agent reports.
		document := filepath.Join(service.Dir(), standards.OpenAPIPath)
		rest, err := resources.LoadRestAPI(ctx, &document)
		if err != nil {
			return nil, fmt.Errorf("endpoint %s: %s: %w", endpoint.Name, standards.OpenAPIPath, err)
		}
		return writeRestContract(ctx, module, endpoint, rest, physicalDir, canonicalDir)
	}
	return nil, fmt.Errorf("endpoint %s reports neither a grpc/connect nor a rest contract", endpoint.Name)
}

// writeProtobufContract builds and writes the FileDescriptorSet contract for a
// protobuf-backed endpoint. It serves both grpc and connect endpoints: connect
// shares the service's proto with grpc, but core models a connect endpoint as
// an HTTP-shaped endpoint that carries no GrpcAPI, so the package and services
// are derived from the built descriptor set rather than from endpoint API
// details.
func writeProtobufContract(ctx context.Context, module *resources.Module, service *resources.Service, endpoint *basev0.Endpoint, surface []composition.APIContractService, physicalDir, canonicalDir string) (*composition.APIContractEndpoint, error) {
	protoDir := filepath.Join(service.Dir(), "proto")
	if ok, _ := shared.FileExists(ctx, filepath.Join(protoDir, "buf.yaml")); !ok {
		return nil, fmt.Errorf("service %s has no proto/buf.yaml; cannot build a descriptor set for endpoint %s", service.Name, endpoint.Name)
	}

	descriptorSet, err := buildDescriptorSet(ctx, protoDir)
	if err != nil {
		return nil, fmt.Errorf("cannot build descriptor set: %w", err)
	}
	return writeBuiltProtobufContract(ctx, module, service, endpoint, surface, physicalDir, canonicalDir, descriptorSet)
}

// writeBuiltProtobufContract keeps the export boundary independently testable
// with real descriptor bytes, without a provider toolchain or released agent.
func writeBuiltProtobufContract(ctx context.Context, module *resources.Module, service *resources.Service, endpoint *basev0.Endpoint, surface []composition.APIContractService, physicalDir, canonicalDir string, descriptorSet []byte) (*composition.APIContractEndpoint, error) {
	protoDir := filepath.Join(service.Dir(), "proto")
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(descriptorSet, &set); err != nil {
		return nil, fmt.Errorf("cannot parse generated descriptor set: %w", err)
	}

	pkg, err := serviceContractPackage(&set, protoDir, surface)
	if err != nil {
		return nil, fmt.Errorf("service %s endpoint %s: %w", service.Name, endpoint.Name, err)
	}
	entry := &composition.APIContractEndpoint{
		Service: service.Name, Endpoint: endpoint.Name, API: endpoint.Api,
		Kind: composition.APIContractKindProtobuf, Package: pkg,
		Digest:   composition.APIContractDigest(descriptorSet),
		Services: surface,
	}
	if entry.Services == nil {
		entry.Services = composition.ProtobufServices(&set, pkg)
	}
	if _, err = endpointProcedures(&set, entry); err != nil {
		return nil, err
	}

	contractPath := filepath.Join(physicalDir, "contract.binpb")
	if err = shared.WriteFileAtomic(ctx, contractPath, descriptorSet, 0o644); err != nil {
		return nil, err
	}
	if err = copyTree(protoDir, filepath.Join(physicalDir, "proto")); err != nil {
		return nil, fmt.Errorf("cannot copy proto sources: %w", err)
	}

	relPath, err := relativeContractPath(module, filepath.Join(canonicalDir, "contract.binpb"))
	if err != nil {
		return nil, err
	}

	entry.Path = relPath
	return entry, nil
}

// serviceContractPackage preserves the single-package default. An explicit
// listener inventory may span packages; then the package field is empty and
// each service's fully qualified name supplies its package.
func serviceContractPackage(set *descriptorpb.FileDescriptorSet, protoDir string, surface []composition.APIContractService) (string, error) {
	if packages := surfacePackages(surface); len(packages) > 0 {
		names := make([]string, 0, len(packages))
		for pkg := range packages {
			names = append(names, pkg)
		}
		sort.Strings(names)
		for _, pkg := range names {
			if len(composition.ProtobufServices(set, pkg)) == 0 {
				return "", fmt.Errorf("inventory package %s declares no services in the descriptor", pkg)
			}
		}
		if len(names) == 1 {
			return names[0], nil
		}
		return "", nil
	}
	own := map[string]struct{}{}
	walkErr := filepath.WalkDir(protoDir, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(p) != ".proto" {
			return nil
		}
		rel, relErr := filepath.Rel(protoDir, p)
		if relErr != nil {
			return relErr
		}
		own[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}

	matchedOwn := false
	seen := map[string]struct{}{}
	var packages []string
	for _, file := range set.GetFile() {
		if _, ok := own[file.GetName()]; !ok {
			continue
		}
		matchedOwn = true
		if len(file.GetService()) == 0 {
			continue
		}
		pkg := file.GetPackage()
		if _, dup := seen[pkg]; dup {
			continue
		}
		seen[pkg] = struct{}{}
		packages = append(packages, pkg)
	}
	sort.Strings(packages)

	switch len(packages) {
	case 0:
		// buf always includes the service's own files in the descriptor set,
		// so matching none of them means buf named them differently than their
		// path under protoDir — a buf module `path:` or v1beta1 `roots:` entry
		// that strips a prefix. Report that cause rather than the misleading
		// "no services", which would send the reader hunting for services that
		// are in fact declared.
		if len(own) > 0 && !matchedOwn {
			return "", fmt.Errorf("none of the service's own proto files under %s appear in the built descriptor set under the same path; a buf module path or roots entry may be remapping file names", protoDir)
		}
		return "", fmt.Errorf("proto declares no services; nothing to export")
	case 1:
		return packages[0], nil
	default:
		return "", fmt.Errorf("proto declares services in multiple packages %v; a contract endpoint must be single-package", packages)
	}
}

// surfacePackages is the set of proto packages a declared surface names, taken
// from each service's fully-qualified name. A name carrying no package
// qualifier contributes nothing rather than an empty-string package.
func surfacePackages(surface []composition.APIContractService) map[string]struct{} {
	packages := make(map[string]struct{}, len(surface))
	for _, service := range surface {
		dot := strings.LastIndex(service.FullName, ".")
		if dot <= 0 {
			continue
		}
		packages[service.FullName[:dot]] = struct{}{}
	}
	return packages
}

func writeRestContract(ctx context.Context, module *resources.Module, endpoint *basev0.Endpoint, rest *basev0.RestAPI, physicalDir, canonicalDir string) (*composition.APIContractEndpoint, error) {
	if len(rest.Openapi) == 0 {
		return nil, fmt.Errorf("endpoint %s has no OpenAPI document", endpoint.Name)
	}
	contractPath := filepath.Join(physicalDir, "openapi.json")
	if err := shared.WriteFileAtomic(ctx, contractPath, rest.Openapi, 0o644); err != nil {
		return nil, err
	}

	title, err := openAPITitle(rest.Openapi)
	if err != nil {
		return nil, fmt.Errorf("cannot read OpenAPI info.title: %w", err)
	}

	routes := contractRoutes(rest.Groups)

	relPath, err := relativeContractPath(module, filepath.Join(canonicalDir, "openapi.json"))
	if err != nil {
		return nil, err
	}

	return &composition.APIContractEndpoint{
		Service:  endpoint.Service,
		Endpoint: endpoint.Name,
		API:      endpoint.Api,
		Kind:     composition.APIContractKindOpenAPI,
		Package:  title,
		Path:     relPath,
		Digest:   composition.APIContractDigest(rest.Openapi),
		Routes:   routes,
	}, nil
}

func openAPITitle(data []byte) (string, error) {
	var document struct {
		Info struct {
			Title string `json:"title"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return "", err
	}
	if document.Info.Title == "" {
		return "", fmt.Errorf("missing info.title")
	}
	return document.Info.Title, nil
}

// relativeContractPath returns path relative to the module directory,
// slash-separated: the form the catalog and package manifest record.
func relativeContractPath(module *resources.Module, path string) (string, error) {
	rel, err := filepath.Rel(module.Dir(), path)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func printContractLine(endpoint *composition.APIContractEndpoint) {
	switch endpoint.Kind {
	case composition.APIContractKindProtobuf:
		cli.Info("contracts: %s/%s  %s  %s  %d service(s)  %s", endpoint.Service, endpoint.Endpoint, endpoint.Kind, endpoint.Package, len(endpoint.Services), endpoint.Digest)
	case composition.APIContractKindOpenAPI:
		cli.Info("contracts: %s/%s  %s  %s  %d route(s)  %s", endpoint.Service, endpoint.Endpoint, endpoint.Kind, endpoint.Package, len(endpoint.Routes), endpoint.Digest)
	}
}

// buildDescriptorSet runs buf inside the proto companion to compile protoDir
// into a serialized descriptorpb.FileDescriptorSet, transitive imports
// included. It works from a temporary copy of protoDir so buf's dependency
// resolution never mutates the service's committed buf.lock.
func buildDescriptorSet(ctx context.Context, protoDir string) ([]byte, error) {
	descriptor, _, err := compileProtoDir(ctx, protoDir, false)
	return descriptor, err
}

// ListProtoSources names protoDir's source set as buf compiles it: the one
// source-set mechanism, shared by `generate client` and `publish clients`
// (over a published contract's proto tree, which carries its buf.yaml).
func ListProtoSources(ctx context.Context, protoDir string) ([]proto.Source, error) {
	_, sources, err := compileProtoDir(ctx, protoDir, true)
	return sources, err
}

// compileProtoDir has buf, in the pinned proto companion, build protoDir's
// descriptor set and, when listSources is set, name the source set it built
// it from (`buf ls-files --format json`): every file buf compiles, read from
// where buf found it and named by its import path. buf.yaml's modules,
// includes, excludes and symlinks are buf's to interpret, never this CLI's.
func compileProtoDir(ctx context.Context, protoDir string, listSources bool) ([]byte, []proto.Source, error) {
	if !runners.DockerEngineRunning(ctx) {
		return nil, nil, fmt.Errorf("docker is not running; please start Docker first")
	}
	image, err := proto.CompanionImage(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot get companion image: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "codefly-contract-proto")
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err = copyTree(protoDir, tmpDir); err != nil {
		return nil, nil, fmt.Errorf("cannot copy proto sources: %w", err)
	}

	projectContainerRecovery(ctx)

	name := fmt.Sprintf("contracts-%d", time.Now().UnixMilli())
	runner, err := runners.NewDockerEnvironment(ctx, image, tmpDir, name)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create docker runner: %w", err)
	}
	// A descriptor-set container holds no state worth preserving across runs: it
	// is created, driven once and shut down in the defer below. Marking it
	// ephemeral is what makes a leaked one recoverable at all — the exact-scope
	// sweep compares a hash that includes the naming scope, which a later run may
	// set differently (--naming-scope, a non-local --env, --temporary-ports),
	// while the disposable sweep is keyed on the naming-scope-independent
	// namespace. Only a container whose owning process is gone is ever reaped, so
	// a concurrent generate is never disturbed.
	runner.WithEphemeral()
	runner.WithMount(tmpDir, "/workspace")
	runner.WithWorkDir("/workspace")
	// Run buf as the host user so the descriptor and buf.lock it writes into the
	// bind-mounted tmp dir are owned by this process, not root. Without this the
	// container writes root-owned files that a non-root CI runner cannot read
	// back or clean up ("permission denied"); the proto companion sets HOME=/tmp
	// precisely so it runs as an arbitrary UID. Mirrors core's runBuf.
	runner.WithUser(fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	runner.WithPause()
	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		_ = runner.Shutdown(cleanupCtx)
	}()

	if err = runner.Init(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot init runner: %w", err)
	}

	depProc, err := runner.NewProcess("buf", "dep", "update")
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create process: %w", err)
	}
	if err = depProc.Run(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot update buf dependencies: %w", err)
	}

	const descriptorFile = ".codefly-contract.binpb"
	buildProc, err := runner.NewProcess("buf", "build", "--as-file-descriptor-set", "-o", "/workspace/"+descriptorFile)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create process: %w", err)
	}
	if err = buildProc.Run(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot build descriptor set: %w", err)
	}

	descriptor, err := os.ReadFile(filepath.Join(tmpDir, descriptorFile))
	if err != nil || !listSources {
		return descriptor, nil, err
	}
	lsProc, err := runner.NewProcess("buf", "ls-files", "--format", "json")
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create process: %w", err)
	}
	stdout, err := lsProc.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read buf ls-files: %w", err)
	}
	if err = lsProc.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot list proto sources: %w", err)
	}
	listing, readErr := io.ReadAll(stdout)
	if err = lsProc.Wait(ctx); err != nil {
		return nil, nil, fmt.Errorf("cannot list proto sources: %w", err)
	}
	if readErr != nil {
		return nil, nil, fmt.Errorf("cannot read buf ls-files: %w", readErr)
	}
	sources, err := protoSourcesFromListing(tmpDir, listing)
	return descriptor, sources, err
}

// bufListedFile is one line of `buf ls-files --format json`.
type bufListedFile struct {
	Path       string `json:"path"`
	ImportPath string `json:"import_path"`
	IsImport   bool   `json:"is_import"`
}

// protoSourcesFromListing reads every file buf listed, from its path under
// root, as a Source named by its import path — the name every importer spells
// and the descriptor set records. Files buf lists only as imports are not
// sources.
func protoSourcesFromListing(root string, listing []byte) ([]proto.Source, error) {
	var sources []proto.Source
	decoder := json.NewDecoder(bytes.NewReader(listing))
	for decoder.More() {
		var file bufListedFile
		if err := decoder.Decode(&file); err != nil {
			return nil, fmt.Errorf("cannot parse buf ls-files: %w", err)
		}
		if file.IsImport {
			continue
		}
		if file.Path == "" || file.ImportPath == "" || !filepath.IsLocal(filepath.FromSlash(file.Path)) {
			return nil, fmt.Errorf("buf ls-files listed an unusable file %q (%q)", file.Path, file.ImportPath)
		}
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path))) //nolint:gosec // a local path buf listed under root
		if err != nil {
			return nil, err
		}
		sources = append(sources, proto.Source{Path: file.ImportPath, Content: content})
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("buf lists no .proto files to compile")
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

// copyTree copies the file tree rooted at src into dst, creating dst as
// needed. A symlink is copied as what it points at, so a symlinked module
// folder reaches buf in the container, where the link's target is not mounted,
// and the published contract's proto tree is self-contained. A linked folder
// may live outside src (a module shared between services is the reason to link
// one), but a link to a folder that holds it, or to one already being copied,
// is a loop and is refused by name.
func copyTree(src, dst string) error {
	root, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return copyResolvedTree(root, dst, []string{root})
}

// copyResolvedTree copies root, a directory with no symlink in its path, into
// dst. copying holds every resolved folder whose copy is in progress, outermost
// first and root last.
func copyResolvedTree(root, dst string, copying []string) error {
	return filepath.WalkDir(root, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.Type()&os.ModeSymlink != 0 {
			info, statErr := os.Stat(p)
			if statErr != nil {
				return statErr
			}
			if info.IsDir() {
				resolved, linkErr := filepath.EvalSymlinks(p)
				if linkErr != nil {
					return linkErr
				}
				// WalkDir follows no link, so p's folder is already resolved.
				for _, held := range append([]string{filepath.Dir(p)}, copying...) {
					if folderHolds(resolved, held) {
						return fmt.Errorf("symlink %s points at %s, which holds it: a looping link cannot be copied", p, resolved)
					}
				}
				return copyResolvedTree(resolved, target, append(copying[:len(copying):len(copying)], resolved))
			}
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p) //nolint:gosec // p is walked from src, a service's own committed proto directory
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644) //nolint:gosec
	})
}

// folderHolds reports whether dir is folder or lies beneath it.
func folderHolds(folder, dir string) bool {
	rel, err := filepath.Rel(folder, dir)
	return err == nil && filepath.IsLocal(rel)
}

// runContractsCheck regenerates contracts into a throwaway directory and
// diffs them against output, without writing anything real.
func runContractsCheck(ctx context.Context, workspace *resources.Workspace, module *resources.Module, output string) error {
	tempDir, err := os.MkdirTemp("", "codefly-contracts-check")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	if _, err = generateContracts(ctx, workspace, module, generateOptions{
		canonicalOutput: output,
		writeDir:        tempDir,
		updateManifest:  false,
	}); err != nil {
		return err
	}

	diffs, err := generators.DiffTrees(tempDir, output)
	if err != nil {
		return err
	}
	if len(diffs) == 0 {
		cli.Header(1, "Contracts are up to date")
		return nil
	}
	cli.Warning("Contracts are out of date:")
	for _, d := range diffs {
		cli.Info("  %s", d)
	}
	return fmt.Errorf("generated contracts differ from %s (%d file(s)); run `codefly generate contracts` to update", output, len(diffs))
}

// updateManifestFile rewrites module.package.codefly.yaml's
// services[*].api-contracts for the services present in contractEndpoints,
// preserving every other byte of the file (comments, key order, unrelated
// content) via a yaml.Node round-trip instead of a struct re-marshal.
func updateManifestFile(ctx context.Context, module *resources.Module, manifest *composition.PackageManifest, contractEndpoints []composition.APIContractEndpoint) error {
	entries := map[string][]composition.ProvidedAPIContract{}
	for i := range contractEndpoints {
		endpoint := &contractEndpoints[i]
		entries[endpoint.Service] = append(entries[endpoint.Service], composition.ProvidedAPIContract{
			Endpoint: endpoint.Endpoint,
			Kind:     endpoint.Kind,
			Package:  endpoint.Package,
			Path:     endpoint.Path,
			Digest:   endpoint.Digest,
		})
	}
	if len(entries) == 0 {
		return nil
	}

	file := filepath.Join(module.Dir(), composition.PackageManifestFileName)
	content, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	updated, err := updatePackageManifestAPIContracts(content, entries)
	if err != nil {
		return err
	}
	if err := shared.WriteFileAtomic(ctx, file, updated, 0o644); err != nil {
		return err
	}
	manifest.Services = mergeAPIContracts(manifest.Services, entries)
	return nil
}

func mergeAPIContracts(services []composition.ProvidedService, entries map[string][]composition.ProvidedAPIContract) []composition.ProvidedService {
	for i := range services {
		if contracts, ok := entries[services[i].Name]; ok {
			services[i].APIContracts = contracts
		}
	}
	return services
}

// updatePackageManifestAPIContracts sets services[name].api-contracts for
// each service in entries to its given list, replacing whatever was there.
// Every other node in the document — comments, key order, unrelated fields —
// is left untouched: the document is parsed into a yaml.Node tree, mutated in
// place, and re-encoded, never reconstructed from a Go struct.
func updatePackageManifestAPIContracts(content []byte, entries map[string][]composition.ProvidedAPIContract) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse package manifest: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty package manifest")
	}
	root := doc.Content[0]
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("package manifest has no services list")
	}

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		var target *yaml.Node
		for _, item := range services.Content {
			if nameNode := mappingValue(item, "name"); nameNode != nil && nameNode.Value == name {
				target = item
				break
			}
		}
		if target == nil {
			return nil, fmt.Errorf("package manifest has no service %q", name)
		}
		setMappingValue(target, "api-contracts", buildAPIContractsNode(entries[name]))
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(node *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			node.Content[i+1] = value
			return
		}
	}
	node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStringTag, Value: key}, value)
}

func buildAPIContractsNode(contracts []composition.ProvidedAPIContract) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, contract := range contracts {
		item := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendScalarField(item, "endpoint", contract.Endpoint)
		appendScalarField(item, "kind", contract.Kind)
		appendScalarField(item, "package", contract.Package)
		appendScalarField(item, "path", contract.Path)
		appendScalarField(item, "digest", contract.Digest)
		seq.Content = append(seq.Content, item)
	}
	return seq
}

func appendScalarField(node *yaml.Node, key, value string) {
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStringTag, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: yamlStringTag, Value: value},
	)
}

// contractRoutes lists a REST endpoint's routes for its contract entry. A
// route's path is already the full path (core's loader holds every route to
// start with its group's path), so the group's path is never prepended: doing
// so wrote "/v1/x/y/v1/x/y" into every catalog.
func contractRoutes(groups []*basev0.RestRouteGroup) []composition.APIContractRoute {
	var routes []composition.APIContractRoute
	for _, group := range groups {
		for _, route := range group.Routes {
			routes = append(routes, composition.APIContractRoute{Method: route.Method.String(), Path: route.Path})
		}
	}
	return routes
}
