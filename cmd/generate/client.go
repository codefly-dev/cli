package generate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/generators"
	coreproto "github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/shared"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"

	"github.com/spf13/cobra"
)

var (
	clientFrom       string
	clientLanguages  []string
	clientServices   []string
	clientName       string
	clientModuleName string
	clientNoFacade   bool
	clientOutput     string
	clientForce      bool
	clientEndpoint   string
	clientGoModule   string
	clientNpmScope   string
)

// defaultFacadeModule is the cobra command name and the fallback facade
// entry-point name (proto.GenerateClient's own default when Module is
// empty), which happen to coincide.
const defaultFacadeModule = "client"

// ClientCmd generates a codefly library — generated bindings plus a generated
// facade — from an API contract, replacing the orphaned generate grpc/openapi
// pair with a real producer for the library system (add library,
// add library-dependency, sync library-dependencies, publish library).
var ClientCmd = &cobra.Command{
	Use:   defaultFacadeModule,
	Short: "Generate a codefly library (bindings + facade) from an API contract",
	Long: `Generate a codefly library — generated bindings plus a generated facade —
into libraries/<name>/<language>/, with a library.codefly.yaml recording the
contract it was generated from.

The contract can come from the local module (--from module/service[/endpoint]),
from a composed module package (--from package:<id>@<version>), or from a
contracts directory on disk (--from contracts:<dir>).
`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		defer services.ClearAgents()

		return runGenerateClient(ctx)
	},
}

func init() {
	ClientCmd.Flags().StringVar(&clientFrom, "from", "", "contract source: module/service[/endpoint], package:<id>@<version>, or contracts:<dir>")
	ClientCmd.Flags().StringSliceVar(&clientLanguages, "language", nil, "languages to generate: go,typescript,python")
	ClientCmd.Flags().StringSliceVar(&clientServices, "services", nil, "restrict the facade to these protobuf services (protobuf contracts only)")
	ClientCmd.Flags().StringVar(&clientName, "name", "", "library name (default: <module>-<service>-client)")
	ClientCmd.Flags().StringVar(&clientModuleName, "module-name", "", "facade entry-point name (default: derived from the contract)")
	ClientCmd.Flags().BoolVar(&clientNoFacade, "no-facade", false, "generate bindings only, no facade")
	ClientCmd.Flags().StringVar(&clientOutput, "output", "", "output directory (default: <workspace>/libraries/<name>)")
	ClientCmd.Flags().BoolVar(&clientForce, "force", false, "overwrite an existing library with a different contract digest")
	ClientCmd.Flags().StringVar(&clientEndpoint, "endpoint", "", "select an endpoint when --from resolves to more than one: service/endpoint")
	ClientCmd.Flags().StringVar(&clientGoModule, "go-module", "", "Go module path for the generated go/ library (default: github.com/codefly-dev/<name>-go)")
	ClientCmd.Flags().StringVar(&clientNpmScope, "npm-scope", "", "npm package name for the generated typescript/ library (default: @codefly-dev/<name>)")
}

func runGenerateClient(ctx context.Context) error {
	if strings.TrimSpace(clientFrom) == "" {
		return fmt.Errorf("--from is required")
	}
	if len(clientLanguages) == 0 {
		return fmt.Errorf("--language is required")
	}
	var langs []languages.Language
	for _, l := range clientLanguages {
		lang := languages.FromString(strings.TrimSpace(l))
		if lang == languages.NotSupported {
			return fmt.Errorf("language %q is not supported", l)
		}
		langs = append(langs, lang)
	}

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return err
	}

	source, err := resolveClientSource(ctx, workspace, clientFrom, clientEndpoint)
	if err != nil {
		return err
	}

	if len(clientServices) > 0 && source.endpoint.Kind != composition.APIContractKindProtobuf {
		return fmt.Errorf("--services only applies to protobuf contracts")
	}
	if source.endpoint.Kind == composition.APIContractKindOpenAPI {
		for _, lang := range langs {
			if lang == languages.PYTHON {
				return fmt.Errorf("python openapi clients are not supported; generate go or typescript")
			}
		}
	}

	name := strings.TrimSpace(clientName)
	if name == "" {
		name = defaultLibraryName(source)
	}

	outputDir := clientOutput
	if outputDir == "" {
		outputDir = filepath.Join(workspace.Dir(), "libraries", name)
	}
	outputDir, err = solveOutputDirectory(ctx, outputDir)
	if err != nil {
		return fmt.Errorf("cannot resolve output path: %w", err)
	}

	existingDigest, hasExisting, err := readExistingLibraryDigest(outputDir)
	if err != nil {
		return err
	}
	if hasExisting && existingDigest != source.endpoint.Digest && !clientForce {
		return fmt.Errorf("library %s already exists with a different contract (digest %s, generating %s); use --force to overwrite", name, existingDigest, source.endpoint.Digest)
	}

	moduleName := strings.TrimSpace(clientModuleName)
	if moduleName == "" {
		moduleName = source.facadeModuleDefault
	}
	if moduleName == "" {
		moduleName = defaultFacadeModule
	}

	contractDir := filepath.Join(outputDir, "contract")
	if err := writeContractFiles(ctx, contractDir, &source.endpoint, source.contractBytes, source.packageID, source.packageVersion); err != nil {
		return fmt.Errorf("cannot write contract: %w", err)
	}

	goModule := strings.TrimSpace(clientGoModule)
	if goModule == "" {
		goModule = "github.com/codefly-dev/" + name + "-go"
	}
	npmPackage := strings.TrimSpace(clientNpmScope)
	if npmPackage == "" {
		npmPackage = "@codefly-dev/" + name
	}
	pyPackage := strings.ReplaceAll(name, "-", "_")

	var langExports []generatedLanguageExport
	for _, lang := range langs {
		export, genErr := generateLanguage(ctx, lang, outputDir, moduleName, clientServices, !clientNoFacade, source, goModule, npmPackage, pyPackage)
		if genErr != nil {
			return fmt.Errorf("cannot generate %s client: %w", lang, genErr)
		}
		langExports = append(langExports, export)
		cli.Info("generated %s client at %s", lang, filepath.Join(outputDir, string(lang)))
	}

	if err := writeLibraryManifest(ctx, outputDir, name, source, langExports, !clientNoFacade); err != nil {
		return fmt.Errorf("cannot write %s: %w", resources.LibraryConfigurationName, err)
	}

	cli.Header(1, "Library %s generated at %s", name, outputDir)
	cli.Info("next: codefly publish library %s", name)
	return nil
}

// runLegacyClientAlias backs the hidden `generate grpc`/`generate openapi`
// aliases: it prints a deprecation notice, then delegates to `generate
// client --from <module>/<service> --language <lang> --no-facade --output
// <destination>`. It resets every generate-client flag var itself, so a
// leftover value from another invocation in the same process (as happens
// when tests drive RunE directly) never leaks into the delegated run.
func runLegacyClientAlias(_ *cobra.Command, alias string) error {
	ctx, done := common.NewContext()
	defer done()
	ctx, stop := common.SignalContext(ctx)
	defer stop()
	defer services.ClearAgents()

	cli.Warning("`codefly generate %s` is deprecated; use `codefly generate client --from <module>/<service> --language %s --no-facade --output <destination>` instead", alias, languageInput)

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return err
	}
	service, module, err := workspace.FindUniqueModuleServiceByName(ctx, serviceInput)
	if err != nil {
		return fmt.Errorf("cannot find service from input: %w", err)
	}
	dest, err := solveOutputDirectory(ctx, destination)
	if err != nil {
		return fmt.Errorf("cannot solve destination path: %w", err)
	}

	clientFrom = module.Name + "/" + service.Name
	clientLanguages = []string{languageInput}
	clientServices = nil
	clientName = ""
	clientModuleName = ""
	clientNoFacade = true
	clientOutput = dest
	clientForce = true
	clientEndpoint = ""
	clientGoModule = ""
	clientNpmScope = ""

	return runGenerateClient(ctx)
}

// resolvedContractSource is what any of the three --from kinds resolve to: a
// single catalog entry, the raw contract bytes it points to, and enough
// identity to fill library.codefly.yaml's source: block.
type resolvedContractSource struct {
	endpoint       composition.APIContractEndpoint
	contractBytes  []byte
	packageID      string
	packageVersion string
	moduleName     string

	facadeModuleDefault string

	// protoSources, when set (the local module/service source only), carries
	// the service's own .proto files. proto.GenerateClient's DescriptorSet
	// path mishandles a --services subset once more than one file is present
	// (core bug: a facade plugin invoked over a multi-file descriptor set —
	// unavoidable once the module's proto imports anything, e.g. a REST
	// gateway's google/api/annotations.proto — reports the target service as
	// undeclared, even though the same content generates fine as Sources).
	// The local source has the original files on hand, so it is generated
	// from Sources instead, sidestepping the bug; contracts:/package:
	// sources only ever have the persisted descriptor bytes and remain
	// exposed to it (see docs/commands.md).
	protoSources []coreproto.Source
}

func resolveClientSource(ctx context.Context, workspace *resources.Workspace, from, endpointHint string) (*resolvedContractSource, error) {
	switch {
	case strings.HasPrefix(from, "package:"):
		return resolvePackageSource(ctx, workspace, strings.TrimPrefix(from, "package:"), endpointHint)
	case strings.HasPrefix(from, "contracts:"):
		return resolveContractsDirSource(ctx, strings.TrimPrefix(from, "contracts:"), endpointHint)
	default:
		return resolveLocalSource(ctx, workspace, from)
	}
}

// resolveLocalSource handles --from module/service[/endpoint]: it builds a
// one-entry catalog in memory from the live service, exactly as
// `generate contracts` builds its on-disk catalog.
func resolveLocalSource(ctx context.Context, workspace *resources.Workspace, from string) (*resolvedContractSource, error) {
	parts := strings.Split(from, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fmt.Errorf("--from %q must be module/service or module/service/endpoint", from)
	}
	moduleName, serviceName := parts[0], parts[1]
	var endpointHint string
	if len(parts) == 3 {
		endpointHint = parts[2]
	}

	module, err := workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("cannot load module %q: %w", moduleName, err)
	}
	service, err := module.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("cannot load service %q: %w", serviceName, err)
	}
	endpoints, err := generators.LoadServiceEndpoints(ctx, workspace, module, service)
	if err != nil {
		return nil, fmt.Errorf("cannot load endpoints for %s/%s: %w", moduleName, serviceName, err)
	}

	chosen, err := pickContractEndpoint(ctx, endpoints, endpointHint, moduleName, serviceName)
	if err != nil {
		return nil, err
	}

	descriptorBytes, contractEndpoint, protoSources, err := buildLocalContractEndpoint(ctx, service, chosen)
	if err != nil {
		return nil, fmt.Errorf("cannot build contract for %s/%s/%s: %w", moduleName, serviceName, chosen.Name, err)
	}

	version := service.Version
	if strings.TrimSpace(version) == "" {
		version = "0.0.1"
	}

	return &resolvedContractSource{
		endpoint:            *contractEndpoint,
		contractBytes:       descriptorBytes,
		packageID:           "local",
		packageVersion:      version,
		moduleName:          module.Name,
		facadeModuleDefault: defaultFacadeModuleName(contractEndpoint),
		protoSources:        protoSources,
	}, nil
}

// pickContractEndpoint selects the endpoint named by hint, or the single
// grpc/rest endpoint among endpoints when hint is empty. More than one
// candidate with no hint is an error listing them.
func pickContractEndpoint(ctx context.Context, endpoints []*basev0.Endpoint, hint, moduleName, serviceName string) (*basev0.Endpoint, error) {
	var candidates []*basev0.Endpoint
	for _, e := range endpoints {
		if resources.IsGRPC(ctx, e) != nil || resources.IsRest(ctx, e) != nil {
			candidates = append(candidates, e)
		}
	}
	if hint != "" {
		for _, e := range candidates {
			if e.Name == hint {
				return e, nil
			}
		}
		return nil, fmt.Errorf("service %s/%s has no grpc or rest endpoint named %q", moduleName, serviceName, hint)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("service %s/%s has no grpc or rest endpoint", moduleName, serviceName)
	}
	if len(candidates) > 1 {
		var names []string
		for _, e := range candidates {
			names = append(names, e.Name)
		}
		return nil, fmt.Errorf("service %s/%s has multiple grpc/rest endpoints (%s); specify one with --from %s/%s/<endpoint>", moduleName, serviceName, strings.Join(names, ", "), moduleName, serviceName)
	}
	return candidates[0], nil
}

// buildLocalContractEndpoint builds the raw contract bytes and catalog entry
// for a single live endpoint, reusing the same descriptor-building and
// OpenAPI-title-extraction helpers `generate contracts` uses, but anchored at
// a flat "contract.binpb"/"openapi.json" path (a generated library owns its
// own contract/ directory, not a module's committed contracts tree).
func buildLocalContractEndpoint(ctx context.Context, service *resources.Service, endpoint *basev0.Endpoint) ([]byte, *composition.APIContractEndpoint, []coreproto.Source, error) {
	if grpc := resources.IsGRPC(ctx, endpoint); grpc != nil {
		protoDir := filepath.Join(service.Dir(), "proto")
		if ok, _ := shared.FileExists(ctx, filepath.Join(protoDir, "buf.yaml")); !ok {
			return nil, nil, nil, fmt.Errorf("service %s has no proto/buf.yaml; cannot build a descriptor set for endpoint %s", service.Name, endpoint.Name)
		}
		descriptorSet, err := buildDescriptorSet(ctx, protoDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot build descriptor set: %w", err)
		}
		var set descriptorpb.FileDescriptorSet
		if err = googleproto.Unmarshal(descriptorSet, &set); err != nil {
			return nil, nil, nil, fmt.Errorf("cannot parse generated descriptor set: %w", err)
		}
		protoSources, err := collectProtoSources(protoDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot read proto sources: %w", err)
		}
		return descriptorSet, &composition.APIContractEndpoint{
			Service:  service.Name,
			Endpoint: endpoint.Name,
			API:      endpoint.Api,
			Kind:     composition.APIContractKindProtobuf,
			Package:  grpc.Package,
			Path:     "contract.binpb",
			Digest:   composition.APIContractDigest(descriptorSet),
			Services: composition.ProtobufServices(&set, grpc.Package),
		}, protoSources, nil
	}
	if rest := resources.IsRest(ctx, endpoint); rest != nil {
		if len(rest.Openapi) == 0 {
			return nil, nil, nil, fmt.Errorf("endpoint %s has no OpenAPI document", endpoint.Name)
		}
		title, err := openAPITitle(rest.Openapi)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot read OpenAPI info.title: %w", err)
		}
		var routes []composition.APIContractRoute
		for _, group := range rest.Groups {
			for _, route := range group.Routes {
				routes = append(routes, composition.APIContractRoute{Method: route.Method.String(), Path: filepath.ToSlash(filepath.Join(group.Path, route.Path))})
			}
		}
		return rest.Openapi, &composition.APIContractEndpoint{
			Service:  service.Name,
			Endpoint: endpoint.Name,
			API:      endpoint.Api,
			Kind:     composition.APIContractKindOpenAPI,
			Package:  title,
			Path:     "openapi.json",
			Digest:   composition.APIContractDigest(rest.Openapi),
			Routes:   routes,
		}, nil, nil
	}
	return nil, nil, nil, fmt.Errorf("endpoint %s reports neither a grpc nor a rest contract", endpoint.Name)
}

// collectProtoSources reads every .proto file directly (and doubly) under
// protoDir into proto.GenerateClient's Sources form, path relative to
// protoDir — the same files buildDescriptorSet compiled, so generation from
// Sources produces the same code the persisted descriptor set describes.
func collectProtoSources(protoDir string) ([]coreproto.Source, error) {
	var sources []coreproto.Source
	err := filepath.WalkDir(protoDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".proto") {
			return err
		}
		rel, err := filepath.Rel(protoDir, p)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(p) //nolint:gosec // p is walked from a service's own committed proto directory
		if err != nil {
			return err
		}
		sources = append(sources, coreproto.Source{Path: filepath.ToSlash(rel), Content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no .proto files under %s", protoDir)
	}
	return sources, nil
}

// resolvePackageSource handles --from package:<id>@<version>: it finds the
// composed module whose package manifest ID matches id, requires it to be
// pinned at exactly version, and reads its committed API contract catalog.
func resolvePackageSource(ctx context.Context, workspace *resources.Workspace, spec, endpointHint string) (*resolvedContractSource, error) {
	idx := strings.LastIndex(spec, "@")
	if idx <= 0 || idx == len(spec)-1 {
		return nil, fmt.Errorf("package source %q must be package:<id>@<version>", spec)
	}
	id, version := spec[:idx], spec[idx+1:]

	for _, ref := range workspace.Modules {
		dir, resolveErr := resolveComposedModuleDir(ctx, workspace, ref)
		if resolveErr != nil {
			continue
		}
		manifest, manifestErr := composition.LoadPackageManifest(dir)
		if manifestErr != nil || manifest.ID != id {
			continue
		}
		if manifest.Version != version {
			return nil, fmt.Errorf("workspace composes %s@%s, not @%s; update the pin first", id, manifest.Version, version)
		}
		catalog, catalogErr := composition.LoadAPIContractCatalog(dir)
		if catalogErr != nil {
			return nil, fmt.Errorf("cannot load API contract catalog for %s: %w", id, catalogErr)
		}
		entry, err := pickCatalogEndpoint(catalog, endpointHint)
		if err != nil {
			return nil, err
		}
		contractBytes, err := readCatalogContractBytes(dir, entry)
		if err != nil {
			return nil, err
		}
		moduleName := ""
		if mod, loadErr := resources.LoadModuleFromDir(ctx, dir); loadErr == nil {
			moduleName = mod.Name
		}
		return &resolvedContractSource{
			endpoint:            *entry,
			contractBytes:       contractBytes,
			packageID:           manifest.ID,
			packageVersion:      manifest.Version,
			moduleName:          moduleName,
			facadeModuleDefault: defaultFacadeModuleName(entry),
		}, nil
	}
	return nil, fmt.Errorf("workspace does not compose package %q", id)
}

// resolveComposedModuleDir resolves a composed module reference to its local
// directory, materializing a pinned reference through the verified module
// package (the same path `codefly run` uses) rather than requiring it
// already be checked out.
func resolveComposedModuleDir(ctx context.Context, workspace *resources.Workspace, ref *resources.ModuleReference) (string, error) {
	resolution, err := workspace.ResolveModule(ctx, ref)
	if err != nil {
		return "", err
	}
	if resolution.Kind == resources.ResolutionPinned {
		pinned, pinnedErr := clicomposition.ResolvePinnedModule(ctx, workspace.Dir(), ref)
		if pinnedErr != nil {
			return "", pinnedErr
		}
		return pinned.Dir, nil
	}
	return resolution.Dir, nil
}

// resolveContractsDirSource handles --from contracts:<dir>: dir may be either
// a module directory or its contracts/api subdirectory directly.
func resolveContractsDirSource(ctx context.Context, dir, endpointHint string) (*resolvedContractSource, error) {
	dir, err := shared.SolvePath(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve %s: %w", dir, err)
	}
	moduleDir := dir
	if filepath.Base(moduleDir) == "api" && filepath.Base(filepath.Dir(moduleDir)) == "contracts" {
		moduleDir = filepath.Dir(filepath.Dir(moduleDir))
	}
	catalog, err := composition.LoadAPIContractCatalog(moduleDir)
	if err != nil {
		return nil, fmt.Errorf("cannot load API contract catalog: %w", err)
	}
	entry, err := pickCatalogEndpoint(catalog, endpointHint)
	if err != nil {
		return nil, err
	}
	contractBytes, err := readCatalogContractBytes(moduleDir, entry)
	if err != nil {
		return nil, err
	}
	moduleName := ""
	if mod, loadErr := resources.LoadModuleFromDir(ctx, moduleDir); loadErr == nil {
		moduleName = mod.Name
	}
	return &resolvedContractSource{
		endpoint:            *entry,
		contractBytes:       contractBytes,
		packageID:           catalog.Package,
		packageVersion:      catalog.Version,
		moduleName:          moduleName,
		facadeModuleDefault: defaultFacadeModuleName(entry),
	}, nil
}

// pickCatalogEndpoint selects the catalog entry named by hint
// ("service/endpoint", an optional leading module segment ignored), or the
// single entry when hint is empty and the catalog carries exactly one.
func pickCatalogEndpoint(catalog *composition.APIContractCatalog, hint string) (*composition.APIContractEndpoint, error) {
	if hint != "" {
		segments := strings.Split(hint, "/")
		if len(segments) < 2 {
			return nil, fmt.Errorf("--endpoint %q must be service/endpoint", hint)
		}
		service, endpoint := segments[len(segments)-2], segments[len(segments)-1]
		for i := range catalog.Endpoints {
			if catalog.Endpoints[i].Service == service && catalog.Endpoints[i].Endpoint == endpoint {
				return &catalog.Endpoints[i], nil
			}
		}
		return nil, fmt.Errorf("catalog has no endpoint %s/%s", service, endpoint)
	}
	if len(catalog.Endpoints) == 0 {
		return nil, fmt.Errorf("catalog has no endpoints")
	}
	if len(catalog.Endpoints) > 1 {
		names := make([]string, len(catalog.Endpoints))
		for i := range catalog.Endpoints {
			names[i] = catalog.Endpoints[i].Service + "/" + catalog.Endpoints[i].Endpoint
		}
		return nil, fmt.Errorf("catalog has multiple endpoints (%s); specify one with --endpoint", strings.Join(names, ", "))
	}
	return &catalog.Endpoints[0], nil
}

// readCatalogContractBytes reads an entry's contract file off disk and
// verifies it still matches the catalog's recorded digest, so a corrupted or
// stale contract file never silently gets baked into a generated library.
func readCatalogContractBytes(moduleDir string, entry *composition.APIContractEndpoint) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(moduleDir, filepath.FromSlash(entry.Path)))
	if err != nil {
		return nil, fmt.Errorf("cannot read contract file %s: %w", entry.Path, err)
	}
	if digest := composition.APIContractDigest(data); digest != entry.Digest {
		return nil, fmt.Errorf("contract file %s digest %s does not match catalog digest %s", entry.Path, digest, entry.Digest)
	}
	return data, nil
}

// defaultFacadeModuleName replicates the facade plugins' own default (the
// last non-version segment of the proto package), so the CLI knows the exact
// facade file name it must locate after generation without forcing the user
// to pass --module-name. Empty for an OpenAPI contract, which has no facade.
func defaultFacadeModuleName(entry *composition.APIContractEndpoint) string {
	if entry.Kind != composition.APIContractKindProtobuf {
		return ""
	}
	segments := strings.Split(entry.Package, ".")
	for len(segments) > 1 && isProtoVersionSegment(segments[len(segments)-1]) {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return ""
	}
	return segments[len(segments)-1]
}

func isProtoVersionSegment(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func defaultLibraryName(source *resolvedContractSource) string {
	module := source.moduleName
	if module == "" {
		module = lastPathSegment(source.packageID)
	}
	return fmt.Sprintf("%s-%s-client", module, source.endpoint.Service)
}

func lastPathSegment(s string) string {
	parts := strings.Split(s, "/")
	return parts[len(parts)-1]
}

// readExistingLibraryDigest reports the source.contract-digest recorded in an
// existing libraries/<name>/library.codefly.yaml, if any. It reads the
// document generically (rather than through resources.Library, which does
// not carry this field) since the file may not exist yet, may have been
// hand-authored, or may carry the extended schema this command writes.
func readExistingLibraryDigest(libraryDir string) (digest string, exists bool, err error) {
	data, err := os.ReadFile(filepath.Join(libraryDir, resources.LibraryConfigurationName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var doc struct {
		Source struct {
			ContractDigest string `yaml:"contract-digest"`
		} `yaml:"source"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", true, fmt.Errorf("cannot parse existing %s: %w", resources.LibraryConfigurationName, err)
	}
	return doc.Source.ContractDigest, true, nil
}

// writeContractFiles writes a generated library's contract/ directory: the
// raw contract bytes (contract.binpb or openapi.json) plus the one-entry
// catalog.codefly.json describing them.
func writeContractFiles(ctx context.Context, contractDir string, endpoint *composition.APIContractEndpoint, contractBytes []byte, packageID, packageVersion string) error {
	if err := os.RemoveAll(contractDir); err != nil {
		return err
	}
	if err := os.MkdirAll(contractDir, 0o755); err != nil {
		return err
	}
	filename := "contract.binpb"
	if endpoint.Kind == composition.APIContractKindOpenAPI {
		filename = "openapi.json"
	}
	if err := shared.WriteFileAtomic(ctx, filepath.Join(contractDir, filename), contractBytes, 0o644); err != nil {
		return err
	}

	entry := *endpoint
	entry.Path = filename
	catalog := &composition.APIContractCatalog{
		Schema:    composition.APIContractCatalogSchema,
		Package:   packageID,
		Version:   packageVersion,
		Endpoints: []composition.APIContractEndpoint{entry},
	}
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("generated catalog is invalid: %w", err)
	}
	data, err := catalog.CanonicalBytes()
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, filepath.Join(contractDir, "catalog.codefly.json"), data, 0o644)
}
