package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Masterminds/semver"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/cli/pkg/librarystore"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

var (
	publishClientsLanguages []string
	publishClientsDryRun    bool
	publishClientsCheck     bool
	publishClientsOutput    string
)

var clientsCmd = &cobra.Command{
	Use:   "clients [module]",
	Short: "Generate and publish a client library for every API contract a module exports",
	Long: `Publish a client library for each endpoint in the module's interface: block
that exports an API contract (codefly generate contracts), in every language
the contract kind supports — go, typescript and python for protobuf, go and
typescript for OpenAPI.

Each endpoint becomes one codefly library named <module>-<service>-client,
generated exactly as ` + "`codefly generate client`" + ` would (bindings plus facade) and
published at the module package version through the stores configured under
the workspace's libraries.publish block. Versions are immutable.

An endpoint restricts or opts out of client publishing in module.codefly.yaml:

  interface:
    endpoints:
      - service: accounts
        endpoint: connect
        clients:
          languages: [go, typescript]                # default: every supported language
          services: [AuditService, WebhookService]   # facade subset; protobuf only
          publish: false                             # opt out

contracts/clients.codefly.json records what was published (library, language,
import path, digest) for the current package version. A contract whose digest
moved without a package version bump is refused: bump module.package.codefly.yaml
first. Re-running after a partial failure publishes only what is still missing.

Examples:
  codefly publish clients saas-starter --dry-run     # plan + identities, no toolchain, no network
  codefly publish clients saas-starter --check       # CI gate: every client of this version is recorded as published
  codefly publish clients saas-starter --language go
  codefly publish clients saas-starter --output ./libraries   # keep the generated libraries`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		moduleName := ""
		if len(args) == 1 {
			moduleName = args[0]
		}
		return publishClients(cmd, moduleName)
	},
}

func init() {
	clientsCmd.Flags().StringSliceVar(&publishClientsLanguages, "language", nil, "Restrict publishing to these languages (default: every language each endpoint declares or supports)")
	clientsCmd.Flags().BoolVar(&publishClientsDryRun, "dry-run", false, "Show what would be generated and published without touching a toolchain or a store")
	clientsCmd.Flags().BoolVar(&publishClientsCheck, "check", false, "Exit 1 unless contracts/clients.codefly.json records every client of the current package version; publishes nothing")
	clientsCmd.Flags().StringVar(&publishClientsOutput, "output", "", "Directory to generate the libraries into (default: a temporary directory removed afterwards)")
	Cmd.AddCommand(clientsCmd)
}

// ClientsManifestSchema versions contracts/clients.codefly.json.
const ClientsManifestSchema = "codefly/module-clients/v1"

// ClientsManifestFileName is the manifest's path relative to the module's
// contracts/ directory — beside contracts/api, never inside it, which
// `generate contracts --check` diffs as a whole tree.
const ClientsManifestFileName = "clients.codefly.json"

// ClientsManifest records the client libraries published for one package
// version. It holds the current version only: the stores keep the history,
// and a version bump starts it over.
type ClientsManifest struct {
	Schema  string            `json:"schema"`
	Package string            `json:"package"`
	Version string            `json:"version"`
	Clients []PublishedClient `json:"clients"`
}

// PublishedClient is one library (one contract endpoint) and the language
// exports of it that are published.
type PublishedClient struct {
	Library        string                  `json:"library"`
	Service        string                  `json:"service"`
	Endpoint       string                  `json:"endpoint"`
	ContractDigest string                  `json:"contractDigest"`
	Services       []string                `json:"services,omitempty"`
	Exports        []PublishedClientExport `json:"exports"`
}

// PublishedClientExport is the durable handle one language export resolved to.
type PublishedClientExport struct {
	Language    string `json:"language"`
	ImportPath  string `json:"importPath"`
	Ref         string `json:"ref,omitempty"`
	Digest      string `json:"digest,omitempty"`
	InstallHint string `json:"installHint"`
}

// clientAction is what reconcileClientExport decides for one (library,
// language) pair against the manifest.
type clientAction int

const (
	actionPublish clientAction = iota
	actionSkip
)

// errContractMoved is returned when the manifest already records the pair at
// this package version but for a different contract digest.
var errContractMoved = errors.New("contract changed since this package version published clients")

// LoadClientsManifest reads contracts/clients.codefly.json; a missing file is
// an empty manifest.
func LoadClientsManifest(moduleDir string) (*ClientsManifest, error) {
	path := filepath.Join(moduleDir, "contracts", ClientsManifestFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &ClientsManifest{Schema: ClientsManifestSchema}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var manifest ClientsManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if manifest.Schema != ClientsManifestSchema {
		return nil, fmt.Errorf("%s: unsupported schema %q (want %s)", path, manifest.Schema, ClientsManifestSchema)
	}
	return &manifest, nil
}

// forVersion returns the manifest's entries when it describes pkg@version,
// or an empty manifest for that version otherwise — a bump drops the previous
// version's records.
func (m *ClientsManifest) forVersion(pkg, version string) *ClientsManifest {
	if m.Package == pkg && m.Version == version {
		return m
	}
	return &ClientsManifest{Schema: ClientsManifestSchema, Package: pkg, Version: version}
}

func (m *ClientsManifest) client(library string) *PublishedClient {
	for i := range m.Clients {
		if m.Clients[i].Library == library {
			return &m.Clients[i]
		}
	}
	return nil
}

func (c *PublishedClient) export(language string) *PublishedClientExport {
	for i := range c.Exports {
		if c.Exports[i].Language == language {
			return &c.Exports[i]
		}
	}
	return nil
}

// reconcileClientExport decides whether the (plan, language) export still
// needs publishing at the manifest's version: recorded at the same contract
// digest → skip (the store version is immutable anyway); recorded at another
// digest → errContractMoved, the package version must be bumped; not
// recorded → publish.
func reconcileClientExport(manifest *ClientsManifest, plan *generate.ModuleClientPlan, language languages.Language) (clientAction, error) {
	client := manifest.client(plan.Name)
	if client == nil {
		return actionPublish, nil
	}
	if client.ContractDigest != plan.Endpoint.Digest {
		return actionPublish, fmt.Errorf("%w: %s (%s/%s) was published at %s for contract %s, the exported contract is now %s; bump the version in %s",
			errContractMoved, plan.Name, plan.Endpoint.Service, plan.Endpoint.Endpoint, manifest.Version, client.ContractDigest, plan.Endpoint.Digest, composition.PackageManifestFileName)
	}
	if client.export(string(language)) != nil {
		return actionSkip, nil
	}
	return actionPublish, nil
}

// record adds or replaces the export of plan in the manifest.
func (m *ClientsManifest) record(plan *generate.ModuleClientPlan, export *PublishedClientExport) {
	client := m.client(plan.Name)
	if client == nil {
		m.Clients = append(m.Clients, PublishedClient{
			Library:        plan.Name,
			Service:        plan.Endpoint.Service,
			Endpoint:       plan.Endpoint.Endpoint,
			ContractDigest: plan.Endpoint.Digest,
			Services:       append([]string(nil), plan.Services...),
		})
		client = &m.Clients[len(m.Clients)-1]
	}
	if existing := client.export(export.Language); existing != nil {
		*existing = *export
	} else {
		client.Exports = append(client.Exports, *export)
	}
	sort.Slice(client.Exports, func(i, j int) bool { return client.Exports[i].Language < client.Exports[j].Language })
	sort.Slice(m.Clients, func(i, j int) bool { return m.Clients[i].Library < m.Clients[j].Library })
}

// Save writes the manifest canonically (sorted, indented) so a publish run
// produces a reviewable one-line-per-fact diff.
func (m *ClientsManifest) Save(ctx context.Context, moduleDir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(moduleDir, "contracts", ClientsManifestFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, path, append(data, '\n'), 0o644)
}

// clientsRun is everything publishClients resolves from the workspace before
// it decides what to do: the module, its exported catalog, the client plans
// and the manifest scoped to the catalog's version.
type clientsRun struct {
	workspace *resources.Workspace
	module    *resources.Module
	catalog   *composition.APIContractCatalog
	version   *semver.Version
	plans     []generate.ModuleClientPlan
	manifest  *ClientsManifest
}

// clientWork is one library still owed at least one language export, with
// the store identity previewed for every language of its plan.
type clientWork struct {
	plan       *generate.ModuleClientPlan
	languages  []languages.Language
	importPath map[languages.Language]string
}

func parseRequestedLanguages(raw []string) ([]languages.Language, error) {
	var requested []languages.Language
	for _, name := range raw {
		lang := languages.FromString(strings.TrimSpace(name))
		if lang == languages.NotSupported {
			return nil, fmt.Errorf("language %q is not supported", name)
		}
		requested = append(requested, lang)
	}
	return requested, nil
}

func loadClientsRun(ctx context.Context, moduleName string, requested []languages.Language) (*clientsRun, error) {
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot load workspace: %w", err)
	}
	var module *resources.Module
	if moduleName != "" {
		module, err = workspace.LoadModuleFromName(ctx, moduleName)
	} else {
		module, err = common.LoadModule(ctx)
	}
	if err != nil {
		return nil, err
	}
	catalog, err := composition.LoadAPIContractCatalog(module.Dir())
	if err != nil {
		return nil, fmt.Errorf("cannot load the exported API contracts of %s (run `codefly generate contracts %s` first): %w", module.Name, module.Name, err)
	}
	version, err := semver.NewVersion(strings.TrimPrefix(catalog.Version, "v"))
	if err != nil {
		return nil, fmt.Errorf("package version %q in the contract catalog is not a strict semantic version: %w", catalog.Version, err)
	}
	config, err := generate.LoadModuleClientsConfig(module.Dir())
	if err != nil {
		return nil, err
	}
	plans, err := generate.PlanModuleClients(module, catalog, config, requested)
	if err != nil {
		return nil, err
	}
	if len(plans) == 0 {
		return nil, fmt.Errorf("module %s exports no API contract with clients to publish", module.Name)
	}
	stored, err := LoadClientsManifest(module.Dir())
	if err != nil {
		return nil, err
	}
	return &clientsRun{
		workspace: workspace,
		module:    module,
		catalog:   catalog,
		version:   version,
		plans:     plans,
		manifest:  stored.forVersion(catalog.Package, version.String()),
	}, nil
}

// planClientWork reconciles every plan against the manifest and previews
// every store identity before anything is generated or published: a
// language with no configured store fails the whole run up front instead of
// after a sibling export has already shipped.
func planClientWork(run *clientsRun, cfg librarystore.StoreConfig) ([]clientWork, error) {
	var work []clientWork
	for i := range run.plans {
		plan := &run.plans[i]
		item := clientWork{plan: plan, importPath: map[languages.Language]string{}}
		for _, lang := range plan.Languages {
			action, err := reconcileClientExport(run.manifest, plan, lang)
			if err != nil {
				return nil, err
			}
			importPath, _, err := librarystore.PreviewIdentity(librarystore.Language(lang), cfg, plan.Name, run.version.String())
			if err != nil {
				return nil, err
			}
			item.importPath[lang] = importPath
			if action == actionSkip {
				cli.Info("%s %s@%s is already published (%s); skipped", plan.Name, lang, run.version, importPath)
				continue
			}
			item.languages = append(item.languages, lang)
		}
		if len(item.languages) > 0 {
			work = append(work, item)
		}
	}
	return work, nil
}

func printClientsDryRun(out io.Writer, work []clientWork) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LIBRARY\tENDPOINT\tLANGUAGE\tIMPORT PATH\tCONTRACT DIGEST\tFACADE")
	for i := range work {
		item := &work[i]
		facade := "whole contract"
		if len(item.plan.Services) > 0 {
			facade = strings.Join(item.plan.Services, ",")
		}
		for _, lang := range item.languages {
			fmt.Fprintf(w, "%s\t%s/%s\t%s\t%s\t%s\t%s\n", item.plan.Name, item.plan.Endpoint.Service, item.plan.Endpoint.Endpoint, lang, item.importPath[lang], item.plan.Endpoint.Digest, facade)
		}
	}
	return w.Flush()
}

func resolveClientsOutputRoot(flag string) (root string, cleanup func(), err error) {
	if flag == "" {
		root, err = os.MkdirTemp("", "codefly-publish-clients-")
		if err != nil {
			return "", nil, err
		}
		return root, func() { _ = os.RemoveAll(root) }, nil
	}
	root, err = shared.SolvePath(flag)
	if err != nil {
		return "", nil, fmt.Errorf("cannot resolve --output: %w", err)
	}
	return root, func() {}, nil
}

// publishClientWork generates one library into outputRoot and publishes its
// owed language exports, recording each one in the manifest as it ships.
// Whatever shipped is immutable, so a partial failure is recorded before it
// is returned and a re-run publishes only the languages still missing.
func publishClientWork(ctx context.Context, run *clientsRun, item *clientWork, cfg librarystore.StoreConfig, outputRoot string, w io.Writer) error {
	entry, err := generate.BuildModuleClientEntry(ctx, run.workspace, run.module, run.catalog, item.plan)
	if err != nil {
		return err
	}
	outputDir := filepath.Join(outputRoot, item.plan.Name)
	if err = generators.GenerateClientLibrary(ctx, &generators.ClientLibraryRequest{
		Name:       item.plan.Name,
		Output:     outputDir,
		Languages:  item.languages,
		Entries:    []generators.ContractEntry{*entry},
		Force:      true,
		GoModule:   item.importPath[languages.GO],
		NpmPackage: item.importPath[languages.TYPESCRIPT],
	}); err != nil {
		return fmt.Errorf("generate %s: %w", item.plan.Name, err)
	}
	lib, err := resources.LoadLibraryFromDir(ctx, outputDir)
	if err != nil {
		return fmt.Errorf("cannot load generated library %s: %w", item.plan.Name, err)
	}
	names := make([]string, 0, len(item.languages))
	for _, lang := range item.languages {
		names = append(names, string(lang))
	}
	exports, err := preflightLibraryExports(lib, run.version, names, cfg)
	if err != nil {
		return err
	}
	published, publishErr := publishLibraryExports(ctx, lib, run.version, exports, cfg)
	for i := range published {
		p := &published[i]
		run.manifest.record(item.plan, &PublishedClientExport{
			Language: string(p.Language), ImportPath: p.ImportPath, Ref: p.Ref, Digest: p.Digest, InstallHint: p.InstallHint,
		})
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", item.plan.Name, p.Language, p.Version, p.ImportPath, p.Digest, p.InstallHint)
	}
	if len(published) > 0 {
		if saveErr := run.manifest.Save(ctx, run.module.Dir()); saveErr != nil {
			if publishErr != nil {
				return fmt.Errorf("%w; additionally cannot record it in contracts/%s: %v", publishErr, ClientsManifestFileName, saveErr)
			}
			return fmt.Errorf("cannot record published clients in contracts/%s: %w", ClientsManifestFileName, saveErr)
		}
	}
	return publishErr
}

func publishClients(cmd *cobra.Command, moduleName string) error {
	ctx, done := common.NewContext()
	defer done()
	ctx, stop := common.SignalContext(ctx)
	defer stop()

	if publishClientsDryRun && publishClientsCheck {
		return fmt.Errorf("--dry-run and --check are mutually exclusive")
	}
	requested, err := parseRequestedLanguages(publishClientsLanguages)
	if err != nil {
		return err
	}
	run, err := loadClientsRun(ctx, moduleName, requested)
	if err != nil {
		return err
	}
	if publishClientsCheck {
		return checkClients(cmd.OutOrStdout(), run.manifest, run.plans)
	}
	cfg, err := librarystore.LoadStoreConfig(run.workspace.Dir())
	if err != nil {
		return err
	}
	work, err := planClientWork(run, cfg)
	if err != nil {
		return err
	}
	if publishClientsDryRun {
		return printClientsDryRun(cmd.OutOrStdout(), work)
	}
	if len(work) == 0 {
		cli.Header(1, "Every client of %s@%s is already published", run.catalog.Package, run.version)
		return nil
	}
	outputRoot, cleanup, err := resolveClientsOutputRoot(publishClientsOutput)
	if err != nil {
		return err
	}
	defer cleanup()

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LIBRARY\tLANGUAGE\tVERSION\tIMPORT PATH\tREF/DIGEST\tINSTALL HINT")
	for i := range work {
		if err = publishClientWork(ctx, run, &work[i], cfg, outputRoot, w); err != nil {
			_ = w.Flush()
			return err
		}
	}
	if err = w.Flush(); err != nil {
		return err
	}
	cli.Header(1, "Published clients of %s@%s; commit contracts/%s", run.catalog.Package, run.version, ClientsManifestFileName)
	return nil
}

// checkClients is the offline CI gate: every planned (library, language)
// pair must be recorded in the manifest at this package version, for the
// contract digest the catalog currently exports.
func checkClients(out io.Writer, manifest *ClientsManifest, plans []generate.ModuleClientPlan) error {
	var problems []string
	for i := range plans {
		plan := &plans[i]
		for _, lang := range plan.Languages {
			action, err := reconcileClientExport(manifest, plan, lang)
			if err != nil {
				problems = append(problems, err.Error())
				break
			}
			if action == actionPublish {
				problems = append(problems, fmt.Sprintf("%s %s is not published at %s", plan.Name, lang, manifest.Version))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("contracts/%s is not complete for %s@%s:\n  %s\nrun `codefly publish clients` and commit the manifest",
			ClientsManifestFileName, manifest.Package, manifest.Version, strings.Join(problems, "\n  "))
	}
	fmt.Fprintf(out, "every client of %s@%s is published and recorded\n", manifest.Package, manifest.Version)
	return nil
}
