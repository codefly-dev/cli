package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runnablev0 "github.com/codefly-dev/core/generated/go/codefly/runnable/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

var (
	runnablesOutput string
	runnablesCheck  bool
)

// RunnablesCmd derives a Runnable package for every method a module's
// published contracts mark as an operation.
var RunnablesCmd = &cobra.Command{
	Use:   "runnables [module]",
	Short: "Derive SERVICE-facility Runnable packages, and so tools, from a module's served methods",
	Long: `Derive a Runnable package for every gRPC method a module's published contracts
serve behind the authority hop. Every Runnable is a tool.

A method that declares its authority policy (authority.hop.v1.policy) is an
operation with nothing added: a policy requiring only "read" derives a
read-only tool that is safe to re-run and carries no receipt; anything else
derives a mutation resolved by its effect receipt through the SDK's generic
lookup. The codefly.runnable.v0.operation option overrides those defaults, and
codefly.runnable.v0.exclude keeps a method from every agent. The message
descriptors say what the contract is, by their proto3 JSON. Nothing is written
twice.

The input is what ` + "`codefly generate contracts`" + ` already wrote — each endpoint's
served procedures in the API catalog and its serialized FileDescriptorSet.
Run that first. Only served methods are derived; a declared Lookup must be
served on the same endpoint. No flag names a method.

For each marked method this writes, under contracts/runnables:

  <service>/<endpoint>/<Method>/runnable-package.json   the canonical package
  <service>/<endpoint>/<Method>/operation.json          its execution policy and authority
  index.json                                            one row per operation

The tree is fully owned: a method that is no longer an operation loses its
directory. A module with no operation writes an empty index, not an error.

A method derived by default that cannot be one — a stream, a policy requiring
nothing, a payload proto3 JSON cannot carry — is listed and left a plain call.
A method carrying the option that core refuses is named and skipped, and the
command exits non-zero after reporting every one of them.

--check is the CI drift gate: it regenerates into a temporary directory and
prints a unified diff of what differs, without writing anything.

Examples:
  codefly generate runnables
  codefly generate runnables documents
  codefly generate runnables --check`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

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

		output := runnablesOutput
		if output == "" {
			output = filepath.Join(module.Dir(), filepath.FromSlash(runnablespkg.DerivedDir))
		} else if output, err = shared.SolvePath(output); err != nil {
			return fmt.Errorf("cannot solve output path: %w", err)
		}

		derived, err := deriveRunnables(ctx, workspace, module)
		if err != nil {
			return err
		}

		if runnablesCheck {
			return runRunnablesCheck(derived, output)
		}
		// A refused method fails the command, and a command that fails must
		// leave the tree exactly as it found it. Writing the methods that did
		// derive would delete the refused one's committed package — asserting
		// the module no longer publishes an operation whose payload the owner
		// merely broke — and the deletion survives the non-zero exit, ready to
		// be committed by anyone who does not read it.
		if refusal := derived.refusal(); refusal != nil {
			return refusal
		}
		if err = writeDerivedTree(ctx, derived.files, output); err != nil {
			return err
		}
		derived.report()
		cli.Header(1, "Derived %d runnable operation(s)", len(derived.index.Operations))
		return nil
	},
}

func init() {
	RunnablesCmd.Flags().StringVar(&runnablesOutput, "output", "", "output directory for derived runnables (default: <module dir>/contracts/runnables)")
	RunnablesCmd.Flags().BoolVar(&runnablesCheck, "check", false, "do not write; exit 1 with a unified diff if the on-disk tree differs from what would be generated")
}

// derivation is everything one walk produced: the files the tree should hold,
// the index that names them, and the methods that were refused.
type derivation struct {
	index *runnablespkg.Index
	files map[string][]byte
	// release is the module's own release version, empty for a module that
	// declares no package manifest.
	release string
	// workspaceDir is the root the derived directories are located relative
	// to. It is not part of the identity — a checkout-dependent path would
	// give one release a different digest per machine.
	workspaceDir string
	refusals     []string
	// notices are methods a default derivation left plain calls, and why.
	notices []string
	// origins says, per operation name, whether the owner declared it and
	// what its tool's effect is.
	origins map[string]derivedOperation
}

// refusal reports every method that could not be derived. They are collected
// rather than returned at the first one: an owner fixing a contract wants the
// whole list, not one method per run.
func (d *derivation) refusal() error {
	if len(d.refusals) == 0 {
		return nil
	}
	return fmt.Errorf("%d method(s) cannot be derived as runnable operations:\n  %s",
		len(d.refusals), strings.Join(d.refusals, "\n  "))
}

// derivedOperation is what the report says of one operation: where it came
// from and what it does.
type derivedOperation struct {
	explicit bool
	effect   string
}

// report says what the run made of the module, so an owner learns which of
// its methods became tools without reading the tree: each operation with its
// origin and effect, the methods left plain calls and why, and — when any
// operation was derived by default — how to keep a method from every agent.
//
// One method published on several endpoints (gRPC and Connect) is several
// operations but one method, so the summary counts and names methods, each
// once, and a notice is printed once per method.
func (d *derivation) report() {
	var defaults, mutations []string
	for i := range d.index.Operations {
		entry := &d.index.Operations[i]
		operation := d.origins[entry.Name]
		origin := "option"
		if !operation.explicit {
			origin = "default"
			defaults = appendOnce(defaults, entry.Method)
			if operation.effect == runnablev0.ToolExposure_EFFECT_MUTATION.String() {
				mutations = appendOnce(mutations, entry.Method)
			}
		}
		cli.Info("runnables: %s  %s  %s  %s  %s", entry.Name, entry.Method, origin, strings.TrimPrefix(operation.effect, "EFFECT_"), entry.Digest)
	}
	var notices []string
	for _, notice := range d.notices {
		notices = appendOnce(notices, notice)
	}
	for _, notice := range notices {
		cli.Info("runnables: not an operation: %s", notice)
	}
	if len(defaults) == 0 {
		return
	}
	cli.Info("runnables: %d method(s) derived by default from their authority policy, %d of them mutations: %s",
		len(defaults), len(mutations), strings.Join(mutations, ", "))
	cli.Info("runnables: every operation is a tool, offered only to callers whose grants cover its binding; " +
		"a method no agent may ever call declares `option (codefly.runnable.v0.exclude) = true;`")
}

// deriveRunnables walks every protobuf contract the module published and
// derives the operations its methods declare.
func deriveRunnables(ctx context.Context, workspace *resources.Workspace, module *resources.Module) (*derivation, error) {
	return deriveRunnablesIn(ctx, workspace.Name, workspace.Dir(), module)
}

// deriveRunnablesIn is deriveRunnables for a caller that holds the workspace's
// name and root rather than a loaded workspace — a release, which reads the
// repository's committed declarations only.
func deriveRunnablesIn(ctx context.Context, workspaceName, workspaceDir string, module *resources.Module) (*derivation, error) {
	catalog, err := loadPublishedContracts(module)
	if err != nil {
		return nil, err
	}

	release, err := moduleReleaseVersion(module)
	if err != nil {
		return nil, err
	}

	derived := &derivation{
		release:      release,
		workspaceDir: workspaceDir,
		index: &runnablespkg.Index{
			Schema:     runnablespkg.IndexSchema,
			Workspace:  workspaceName,
			Module:     module.Name,
			Operations: []runnablespkg.IndexEntry{},
		},
		files:   map[string][]byte{},
		origins: map[string]derivedOperation{},
	}

	var endpoints []composition.APIContractEndpoint
	if catalog != nil {
		endpoints = append(endpoints, catalog.Endpoints...)
	}
	sort.Slice(endpoints, func(i, j int) bool {
		if endpoints[i].Service != endpoints[j].Service {
			return endpoints[i].Service < endpoints[j].Service
		}
		return endpoints[i].Endpoint < endpoints[j].Endpoint
	})

	agents := map[string]*basev0.Agent{}
	for i := range endpoints {
		endpoint := &endpoints[i]
		if endpoint.Kind != composition.APIContractKindProtobuf {
			continue
		}
		agent, ok := agents[endpoint.Service]
		if !ok {
			if agent, err = ownerAgent(ctx, module, endpoint.Service); err != nil {
				return nil, err
			}
			agents[endpoint.Service] = agent
		}
		if err = deriveEndpoint(module, endpoint, agent, derived); err != nil {
			return nil, err
		}
	}

	indexBytes, err := marshalIndented(derived.index)
	if err != nil {
		return nil, err
	}
	derived.files[runnablespkg.IndexFileName] = indexBytes
	return derived, nil
}

// ownerAgent returns the service agent a derived package records. A method an
// owner already publishes is built by the owner's own builder, so the package
// names that agent rather than a runnable one.
func ownerAgent(ctx context.Context, module *resources.Module, serviceName string) (*basev0.Agent, error) {
	service, err := module.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("cannot load service %s: %w", serviceName, err)
	}
	if service.Agent == nil {
		return nil, fmt.Errorf("service %s/%s pins no agent; a derived package records the owner's own builder", module.Name, serviceName)
	}
	// core refuses a package whose agent is "latest", and core's own agent
	// parsing defaults a version-less agent to it, so an unpinned service
	// would fail deep inside package validation with no mention of the
	// service that has to be edited.
	if service.Agent.Version == "" || service.Agent.Version == "latest" {
		return nil, fmt.Errorf("service %s/%s pins agent %s at no version; a derived package is an immutable release and cannot record \"latest\"",
			module.Name, serviceName, service.Agent.Name)
	}
	agent, err := service.Agent.Proto()
	if err != nil {
		return nil, fmt.Errorf("service %s/%s agent: %w", module.Name, serviceName, err)
	}
	return agent, nil
}

// deriveEndpoint derives every marked method one endpoint publishes.
func deriveEndpoint(module *resources.Module, endpoint *composition.APIContractEndpoint, agent *basev0.Agent, derived *derivation) error {
	contract := filepath.Join(module.Dir(), filepath.FromSlash(endpoint.Path))
	data, err := os.ReadFile(contract) //nolint:gosec // the path comes from the module's own contract catalog
	if err != nil {
		return fmt.Errorf("cannot read the contract of %s/%s: %w", endpoint.Service, endpoint.Endpoint, err)
	}
	var set descriptorpb.FileDescriptorSet
	if err = googleproto.Unmarshal(data, &set); err != nil {
		return fmt.Errorf("cannot parse %s: %w", contract, err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return fmt.Errorf("cannot resolve the descriptors of %s/%s: %w", endpoint.Service, endpoint.Endpoint, err)
	}

	owner := corerunnable.ServiceOwner{
		Module:   module.Name,
		Service:  endpoint.Service,
		Endpoint: endpoint.Endpoint,
		Agent:    agent,
	}
	methods, err := endpointProcedures(&set, endpoint)
	if err != nil {
		return err
	}
	for _, method := range methods {
		if err = deriveMethod(module, endpoint, owner, files, method, methods, derived); err != nil {
			return err
		}
	}
	return nil
}

// deriveMethod derives one method. A method that carries no option is not an
// operation, which is an answer rather than a failure; anything else the
// derivation refuses is recorded and the walk continues.
func deriveMethod(module *resources.Module, endpoint *composition.APIContractEndpoint, owner corerunnable.ServiceOwner, files *protoregistry.Files, fullMethod string, served []string, derived *derivation) error {
	name := methodName(fullMethod)
	relativeDir, err := derivedRelativeDir(derived.workspaceDir, module, endpoint, name)
	if err != nil {
		return err
	}
	location := &resources.RunnableLocation{
		Identity: &resources.RunnableIdentity{
			Name:      derivedName(endpoint.Service, endpoint.Endpoint, name),
			Module:    module.Name,
			Workspace: derived.index.Workspace,
			Version:   derivedVersion(derived.release, endpoint),
		},
		WorkspacePath:       derived.workspaceDir,
		RelativeToWorkspace: relativeDir,
	}

	pkg, spec, err := corerunnable.PackageFromMethod(files, location, owner, fullMethod)
	if err != nil {
		if errors.Is(err, corerunnable.ErrNotAnOperation) {
			return nil
		}
		if errors.Is(err, corerunnable.ErrNotDerivable) {
			derived.notices = append(derived.notices, fmt.Sprintf("%s: %v", fullMethod, err))
			return nil
		}
		derived.refusals = append(derived.refusals, fmt.Sprintf("%s: %v", fullMethod, err))
		return nil
	}
	if spec.LookupMethod != "" && !slices.Contains(served, spec.LookupMethod) {
		derived.refusals = append(derived.refusals, fmt.Sprintf("%s: endpoint %s/%s does not serve lookup method %s", fullMethod, endpoint.Service, endpoint.Endpoint, spec.LookupMethod))
		return nil
	}

	packageBytes, err := runnablespkg.CanonicalJSON(pkg)
	if err != nil {
		return fmt.Errorf("%s: cannot encode the derived package: %w", fullMethod, err)
	}
	operationBytes, err := marshalIndented(newOperationDocument(spec))
	if err != nil {
		return fmt.Errorf("%s: cannot encode the derived operation: %w", fullMethod, err)
	}

	dir := filepath.Join(endpoint.Service, endpoint.Endpoint, name)
	derived.files[filepath.ToSlash(filepath.Join(dir, runnablespkg.PackageFileName))] = append(packageBytes, '\n')
	derived.files[filepath.ToSlash(filepath.Join(dir, runnablespkg.OperationFileName))] = operationBytes

	operation := pkg.GetServiceOperations()[0]
	derived.origins[pkg.GetIdentity().GetName()] = derivedOperation{explicit: spec.Declared, effect: spec.Tool.GetEffect().String()}
	derived.index.Operations = append(derived.index.Operations, runnablespkg.IndexEntry{
		Name:          pkg.GetIdentity().GetName(),
		Version:       pkg.GetIdentity().GetVersion(),
		Digest:        pkg.GetDigest(),
		Service:       endpoint.Service,
		Endpoint:      endpoint.Endpoint,
		Method:        operation.GetOperation(),
		InputMessage:  operation.GetInputMessage(),
		OutputMessage: operation.GetOutputMessage(),
		Path:          filepath.ToSlash(filepath.Join(runnablespkg.DerivedDir, dir)),
	})
	return nil
}

// derivedRelativeDir locates one operation's directory relative to the
// workspace root, which is what a runnable location records.
func derivedRelativeDir(workspaceDir string, module *resources.Module, endpoint *composition.APIContractEndpoint, method string) (string, error) {
	dir := filepath.Join(module.Dir(), filepath.FromSlash(runnablespkg.DerivedDir), endpoint.Service, endpoint.Endpoint, method)
	return filepath.Rel(workspaceDir, dir)
}

// loadPublishedContracts returns the contracts a module published, or nil when
// it exports none.
//
// A module with no interface block exports no endpoint, so it has no method to
// derive from and no catalog to read — zero operations, which is an empty
// index rather than an error, exactly as a module whose methods carry no
// option produces one. A module that DOES declare an interface but has no
// catalog has simply not run the prerequisite, and saying so is more useful
// than an empty index that hides it.
func loadPublishedContracts(module *resources.Module) (*composition.APIContractCatalog, error) {
	catalog, err := composition.LoadAPIContractCatalog(module.Dir())
	if err == nil {
		return catalog, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if module.Interface == nil {
		return nil, nil
	}
	return nil, fmt.Errorf("%w; run `codefly generate contracts` first, so the methods to derive from are published", err)
}

// moduleReleaseVersion is the version a packageable module is released at. A
// module with no package manifest is not packageable yet, which is reported as
// an empty version rather than an error: `generate contracts` exports such a
// module too, and the operations derived from those contracts fall back to a
// digest-derived identity.
func moduleReleaseVersion(module *resources.Module) (string, error) {
	manifest, err := composition.LoadPackageManifest(module.Dir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cli.Warning("module %s has no %s; derived operations carry a contract-digest identity instead of a release version", module.Name, composition.PackageManifestFileName)
			return "", nil
		}
		return "", err
	}
	return manifest.Version, nil
}

// derivedVersion is the release identity a derived package carries.
//
// A module that declares a package manifest is released at its version, and
// that is the version its contracts are published under, so the operations
// derived from them carry it too. A module with no manifest has no release
// version to carry, and the contract's own digest stands in: it is a valid
// semantic version and it changes exactly when the contract does.
//
// Either way the version's only job is to be a valid identity. A binding pins
// the package digest, not the version, because the digest is what a consumer
// actually agreed to — two releases of one module that leave a method's
// contract and policy untouched derive byte-identical packages.
func derivedVersion(release string, endpoint *composition.APIContractEndpoint) string {
	if release != "" {
		return release
	}
	digest := strings.TrimPrefix(endpoint.Digest, "sha256:")
	if len(digest) > 12 {
		digest = digest[:12]
	}
	// The digest is prefixed rather than used bare because a prerelease
	// identifier made only of digits may not carry a leading zero: a contract
	// whose digest begins "0123456789ab" would spell an invalid version, and
	// every method in that module would then be refused for a reason naming
	// the method instead of the version. "contract-" makes the identifier
	// alphanumeric, which exempts it from that rule whatever the digest is.
	return "0.0.0-contract-" + digest
}

// derivedName names the release a method derives. The endpoint is part of it
// because one proto service may be published on both a grpc and a connect
// endpoint: those are two operations reached two ways, and a name that dropped
// the endpoint would give them one identity and two digests.
//
// A release name is at most maxDerivedName characters. A longer one is
// shortened deterministically — a stable prefix and a short digest of the
// whole name — so a long method name never fails generation and two long
// names never collapse into one.
func derivedName(service, endpoint, method string) string {
	return corerunnable.BoundedName(strings.Join([]string{service, endpoint, kebab(method)}, "-"), maxDerivedName, "-")
}

// appendOnce appends value unless the list already holds it.
func appendOnce(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

// maxDerivedName is the longest release name a RunnableIdentity admits.
const maxDerivedName = 50

// kebab spells a method name the way a resource name is spelled. An acronym
// stays one word: ApplyHTTPText is apply-http-text, not apply-h-t-t-p-text.
func kebab(name string) string {
	var out []rune
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			previous := runes[i-1]
			if !unicode.IsUpper(previous) || (i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
				out = append(out, '-')
			}
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

func methodName(fullMethod string) string {
	return fullMethod[strings.LastIndex(fullMethod, "/")+1:]
}
