package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	solutionSDKLanguages []string
	solutionSDKCheck     bool
	solutionSDKApply     bool

	// skipGenerateForTests short-circuits the actual codegen call
	// (generators.GenerateClientLibrary), so a test can exercise resolution,
	// validation, and --apply-dependencies without Docker. Never set outside
	// tests.
	skipGenerateForTests bool
)

// SolutionSDKCmd aggregates one client library per solution from the module-
// bound entries of solution.codefly.yaml's api.consumes, and keeps the
// runtime service-dependencies declaration in step with it.
var SolutionSDKCmd = &cobra.Command{
	Use:   "solution-sdk",
	Short: "Aggregate one client library per solution from api.consumes",
	Long: `Reads api.consumes in solution.codefly.yaml and produces one aggregated
client library per solution, carrying only the declared services, generated
from the contracts of the module packages the workspace composes. Also keeps
the runtime service-dependencies in step with the declaration.
`,
	Args: cobra.NoArgs,
	RunE: func(_ *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		return runSyncSolutionSDK(ctx)
	},
}

func init() {
	SolutionSDKCmd.Flags().StringSliceVar(&solutionSDKLanguages, "language", nil, "languages to generate: go,typescript,python")
	SolutionSDKCmd.Flags().BoolVar(&solutionSDKCheck, "check", false, "do not write; exit 1 if the library or service-dependencies are out of date")
	SolutionSDKCmd.Flags().BoolVar(&solutionSDKApply, "apply-dependencies", false, "add missing service-dependencies entries for every bound consume entry")
}

func runSyncSolutionSDK(ctx context.Context) error {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	warnLegacySolutionSDKFile(workspace.Dir())

	manifestPath := filepath.Join(workspace.Dir(), manifest.FileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", manifest.FileName, err)
	}
	solutionManifest, err := manifest.Load(data)
	if err != nil {
		return fmt.Errorf("cannot load %s: %w", manifest.FileName, err)
	}

	entries, err := resolveConsumedContracts(ctx, workspace, solutionManifest)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		cli.Info("nothing to sync: api.consumes declares no module-bound APIs")
		return nil
	}

	entryModule, entryService, err := resolveSolutionEntryService(ctx, workspace)
	if err != nil {
		return err
	}

	missing, err := missingServiceDependencies(entryService, entries)
	if err != nil {
		return err
	}

	name := entryModule.Name + "-sdk"
	outputDir := filepath.Join(workspace.Dir(), "libraries", name)

	if solutionSDKCheck {
		return runSolutionSDKCheck(ctx, name, outputDir, entries, missing)
	}

	if solutionSDKApply {
		if err := applyServiceDependencies(entryService, missing); err != nil {
			return err
		}
	} else {
		for _, m := range missing {
			cli.Warning("service-dependencies missing %s/%s (endpoint %s); run with --apply-dependencies to add it", m.Module, m.Service, m.Endpoint)
		}
	}

	if err := generateSolutionSDK(ctx, name, outputDir, entries); err != nil {
		return err
	}

	printResolvedEntries(entries)
	cli.Header(1, "Library %s generated at %s", name, outputDir)
	cli.Info("next: codefly sync library-dependencies --service %s/%s", entryModule.Name, entryService.Name)
	return nil
}

// resolveConsumedContracts resolves every module-bound api.consumes entry in
// solutionManifest to a ContractEntry: the composed module's package and API
// contract catalog, version-checked and services-validated.
func resolveConsumedContracts(ctx context.Context, workspace *resources.Workspace, solutionManifest *manifest.Manifest) ([]generators.ContractEntry, error) {
	var entries []generators.ContractEntry
	for _, consume := range solutionManifest.API.Consumes {
		if consume.Module == "" {
			continue
		}

		var ref *resources.ModuleReference
		for _, r := range workspace.Modules {
			if r.Name == consume.Module {
				ref = r
				break
			}
		}
		if ref == nil {
			return nil, fmt.Errorf("solution consumes %s/%s but the workspace does not compose module %q; run codefly add module", consume.Module, consume.Service, consume.Module)
		}

		dir, err := clicomposition.ResolveComposedModuleDir(ctx, workspace, ref)
		if err != nil {
			return nil, fmt.Errorf("cannot resolve module %q: %w", consume.Module, err)
		}

		pkgManifest, err := composition.LoadPackageManifest(dir)
		if err != nil {
			return nil, fmt.Errorf("module %s ships no API contracts; the producer must run codefly generate contracts", consume.Module)
		}
		catalog, err := composition.LoadAPIContractCatalog(dir)
		if err != nil {
			return nil, fmt.Errorf("module %s (package %s@%s) ships no API contracts; the producer must run codefly generate contracts", consume.Module, pkgManifest.ID, pkgManifest.Version)
		}

		if strings.TrimSpace(consume.Version) != "" {
			constraint, err := semver.NewConstraint(consume.Version)
			if err != nil {
				return nil, fmt.Errorf("consume %s/%s: invalid version constraint %q: %w", consume.Module, consume.Service, consume.Version, err)
			}
			composedVersion, err := semver.NewVersion(pkgManifest.Version)
			if err != nil {
				return nil, fmt.Errorf("module %s package version %q is invalid: %w", consume.Module, pkgManifest.Version, err)
			}
			if !constraint.Check(composedVersion) {
				return nil, fmt.Errorf("composed %s@%s does not satisfy %s", pkgManifest.ID, pkgManifest.Version, consume.Version)
			}
		}

		var endpointEntry *composition.APIContractEndpoint
		for i := range catalog.Endpoints {
			if catalog.Endpoints[i].Service == consume.Service && catalog.Endpoints[i].Endpoint == consume.Endpoint {
				endpointEntry = &catalog.Endpoints[i]
				break
			}
		}
		if endpointEntry == nil {
			return nil, fmt.Errorf("module %s catalog has no endpoint %s/%s", consume.Module, consume.Service, consume.Endpoint)
		}

		if len(consume.Services) > 0 {
			known := make(map[string]bool, len(endpointEntry.Services))
			for _, s := range endpointEntry.Services {
				known[s.Name] = true
			}
			var unknown []string
			for _, s := range consume.Services {
				if !known[s] {
					unknown = append(unknown, s)
				}
			}
			if len(unknown) > 0 {
				return nil, fmt.Errorf("consume %s/%s/%s: unknown service(s) %s", consume.Module, consume.Service, consume.Endpoint, strings.Join(unknown, ", "))
			}
		}

		contractBytes, err := generators.ReadCatalogContractBytes(dir, endpointEntry)
		if err != nil {
			return nil, err
		}

		entries = append(entries, generators.ContractEntry{
			Endpoint:            *endpointEntry,
			ContractBytes:       contractBytes,
			PackageID:           pkgManifest.ID,
			PackageVersion:      pkgManifest.Version,
			ModuleName:          consume.Module,
			FacadeModuleDefault: generators.DefaultFacadeModuleName(endpointEntry),
			Services:            consume.Services,
			As:                  consume.As,
		})
	}
	return entries, nil
}

// resolveSolutionEntryService finds the solution root — the workspace's own
// module (the `path: .` self module, or, failing an explicit path, the
// module whose name matches the workspace) — and its service-entry.
// Composed dependency modules may declare their own service-entry, but those
// are dependencies, not the solution root.
func resolveSolutionEntryService(ctx context.Context, workspace *resources.Workspace) (*resources.Module, *resources.Service, error) {
	ref := solutionRootModuleRef(workspace)
	if ref == nil {
		return nil, nil, fmt.Errorf("no solution root in workspace <%s>: no module is referenced by `path: .` or named after the workspace", workspace.Name)
	}
	mod, err := workspace.LoadModuleFromReference(ctx, ref)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load solution root module <%s>: %w", ref.Name, err)
	}
	if mod.ServiceEntry == "" {
		return nil, nil, fmt.Errorf("solution root module <%s> declares no service-entry", mod.Name)
	}
	service, err := mod.LoadServiceFromName(ctx, mod.ServiceEntry)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot load module service entry %q: %w", mod.ServiceEntry, err)
	}
	return mod, service, nil
}

func solutionRootModuleRef(workspace *resources.Workspace) *resources.ModuleReference {
	for _, ref := range workspace.Modules {
		if ref.PathOverride != nil && *ref.PathOverride == "." {
			return ref
		}
	}
	for _, ref := range workspace.Modules {
		if ref.Name == workspace.Name {
			return ref
		}
	}
	return nil
}

// missingDependency is one api.consumes entry not yet reflected in the entry
// service's service-dependencies.
type missingDependency struct {
	Module, Service, Endpoint string
}

// missingServiceDependencies reports which consumed entries have no matching
// service-dependencies item, matched by (name, module) — the same identity
// granularity core's own Application.HasServiceDependency uses.
func missingServiceDependencies(service *resources.Service, entries []generators.ContractEntry) ([]missingDependency, error) {
	existing := make(map[[2]string]bool, len(service.ServiceDependencies))
	for _, dep := range service.ServiceDependencies {
		existing[[2]string{dep.Name, dep.Module}] = true
	}
	var missing []missingDependency
	for _, entry := range entries {
		if existing[[2]string{entry.Endpoint.Service, entry.ModuleName}] {
			continue
		}
		missing = append(missing, missingDependency{Module: entry.ModuleName, Service: entry.Endpoint.Service, Endpoint: entry.Endpoint.Endpoint})
	}
	return missing, nil
}

// applyServiceDependencies node-preserving-edits the entry service's
// service.codefly.yaml, appending one service-dependencies item per missing
// entry. Existing items are never touched or removed.
func applyServiceDependencies(service *resources.Service, missing []missingDependency) error {
	if len(missing) == 0 {
		return nil
	}
	path := filepath.Join(service.Dir(), resources.ServiceConfigurationName)
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", resources.ServiceConfigurationName, err)
	}
	_, root, err := yamledit.Document(original)
	if err != nil {
		return fmt.Errorf("cannot parse %s: %w", resources.ServiceConfigurationName, err)
	}

	items := make([]*yaml.Node, 0, len(missing))
	for _, m := range missing {
		item, err := yamledit.Encode(struct {
			Name      string              `yaml:"name"`
			Module    string              `yaml:"module"`
			Endpoints []map[string]string `yaml:"endpoints"`
		}{
			Name:      m.Service,
			Module:    m.Module,
			Endpoints: []map[string]string{{"name": m.Endpoint}},
		})
		if err != nil {
			return err
		}
		items = append(items, item)
	}

	updated, err := yamledit.AppendSequenceItems(original, root, "service-dependencies", items)
	if err != nil {
		return err
	}
	if err := shared.WriteFileAtomic(context.Background(), path, updated, 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", resources.ServiceConfigurationName, err)
	}
	for _, m := range missing {
		cli.Info("added service-dependency %s/%s (endpoint %s)", m.Module, m.Service, m.Endpoint)
	}
	return nil
}

func generateSolutionSDK(ctx context.Context, name, outputDir string, entries []generators.ContractEntry) error {
	if skipGenerateForTests {
		return nil
	}
	return generateSolutionSDKInto(ctx, name, outputDir, entries)
}

// runSolutionSDKCheck exits 1 when the library is missing entirely (no Docker
// needed to detect that), when a service-dependency is missing, or when
// regenerating into a scratch directory differs from what is on disk.
func runSolutionSDKCheck(ctx context.Context, name, outputDir string, entries []generators.ContractEntry, missing []missingDependency) error {
	problems := 0
	for _, m := range missing {
		cli.Warning("service-dependencies missing %s/%s (endpoint %s); run with --apply-dependencies to add it", m.Module, m.Service, m.Endpoint)
		problems++
	}

	if _, err := os.Stat(filepath.Join(outputDir, resources.LibraryConfigurationName)); err != nil {
		if os.IsNotExist(err) {
			cli.Warning("library %s is absent; run `codefly sync solution-sdk` to generate it", name)
			return fmt.Errorf("solution SDK %s is out of date (%d problem(s))", name, problems+1)
		}
		return err
	}

	if !skipGenerateForTests {
		tempDir, err := os.MkdirTemp("", "codefly-solution-sdk-check")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(tempDir) }()

		if err := generateSolutionSDKInto(ctx, name, tempDir, entries); err != nil {
			return err
		}
		diffs, err := diffTrees(tempDir, outputDir)
		if err != nil {
			return err
		}
		if len(diffs) > 0 {
			cli.Warning("solution SDK %s is out of date:", name)
			for _, d := range diffs {
				cli.Info("  %s", d)
			}
			problems += len(diffs)
		}
	}

	if problems > 0 {
		return fmt.Errorf("solution SDK %s is out of date (%d problem(s)); run `codefly sync solution-sdk` to update", name, problems)
	}
	cli.Header(1, "Solution SDK %s is up to date", name)
	return nil
}

func generateSolutionSDKInto(ctx context.Context, name, outputDir string, entries []generators.ContractEntry) error {
	var langs []languages.Language
	for _, l := range solutionSDKLanguages {
		lang := languages.FromString(strings.TrimSpace(l))
		if lang == languages.NotSupported {
			return fmt.Errorf("language %q is not supported", l)
		}
		langs = append(langs, lang)
	}
	return generators.GenerateClientLibrary(ctx, &generators.ClientLibraryRequest{
		Name:       name,
		Output:     outputDir,
		Languages:  langs,
		Entries:    entries,
		GoModule:   "github.com/codefly-dev/" + name + "-go",
		NpmPackage: "@codefly-dev/" + name,
	})
}

// diffTrees returns the slash-separated, sorted set of relative paths whose
// content (or presence) differs between the two directory trees.
func diffTrees(generated, existing string) ([]string, error) {
	generatedFiles, err := listRelativeFiles(generated)
	if err != nil {
		return nil, err
	}
	existingFiles, err := listRelativeFiles(existing)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	for f := range generatedFiles {
		seen[f] = struct{}{}
	}
	for f := range existingFiles {
		seen[f] = struct{}{}
	}
	var diffs []string
	for f := range seen {
		a, okA := generatedFiles[f]
		b, okB := existingFiles[f]
		if okA != okB || string(a) != string(b) {
			diffs = append(diffs, f)
		}
	}
	sort.Strings(diffs)
	return diffs, nil
}

func listRelativeFiles(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // p is walked from a directory this command owns or was pointed at
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func printResolvedEntries(entries []generators.ContractEntry) {
	for _, entry := range entries {
		services := "all"
		if len(entry.Services) > 0 {
			services = strings.Join(entry.Services, ", ")
		}
		cli.Info("%s ← %s/%s/%s @%s services: %s", entry.Endpoint.Service, entry.ModuleName, entry.Endpoint.Service, entry.Endpoint.Endpoint, entry.PackageVersion, services)
	}
}

// warnLegacySolutionSDKFile prints the migration note when a solution-sdk.yaml
// (the pre-api.consumes declaration this command replaces) sits next to the
// manifest. It is not auto-migrated: source.repo/ref there has no equivalent —
// the workspace composition pin replaces it.
func warnLegacySolutionSDKFile(workspaceDir string) {
	if _, err := os.Stat(filepath.Join(workspaceDir, "solution-sdk.yaml")); err == nil {
		cli.Warning("found solution-sdk.yaml; migrate its dependencies into solution.codefly.yaml api.consumes (module/service/endpoint/services) and delete the file; the vendored _sdk/ is replaced by libraries/<name>-sdk/")
	}
}
