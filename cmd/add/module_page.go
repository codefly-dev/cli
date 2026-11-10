package add

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/generate"
	clisync "github.com/codefly-dev/cli/cmd/sync"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/standards"
	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"
	"gopkg.in/yaml.v3"
)

// ModulePageCmd scaffolds a module with a page.
//
// It is its own verb, beside `add solution`, for the reason that one is: a
// module page is a shape (a module, an agent-scaffolded entry service, a page
// remote and the documents a host reads), not a mode of `add module`, whose
// flags compose an existing module (--source, --worktree) or run a template
// (--agent) and exclude each other. Like `add solution` it composes verbs the
// CLI already has — `add module`, `add service`, `sync service`, `generate
// contracts`, `generate permissions`, `generate client` — and reads each
// document it writes back through core's own reader.
var ModulePageCmd = &cobra.Command{
	Use:   "module-page",
	Short: "Scaffold a module with a page: its Connect entry service, the page remote, and least-privilege roles",
	Long: `Scaffold a module that people reach through a page a host mounts.

A module page is a module that ships NO solution.codefly.yaml and declares its
page beside its entry service (solution.frontend.codefly.yaml). Unlike a
solution, it is installed by composition in every organization that composes
it, holds no standing authority (installation_role: none), and nobody can use
it until an administrator grants one of its roles ("Set up access" in Admin).

What this writes:

  services/<name>    The entry: a go-grpc service serving Connect (which also
                     carries gRPC and gRPC-Web). Its proto marks every method
                     with what a call must hold (authority.hop.v1.policy).
  services/fe-remote The ./Page remote. It calls the module's own procedures
                     on the host's module edge — <moduleApiBase>/<package.Service>/<Method>,
                     the /_browser door — through the generated client, never
                     on apiBase, the host's relay for a solution runtime.
  module.contract.codefly.yaml
                     installer, editor and viewer, each granting exactly what
                     the operations its holders call require, with the
                     operation named beside each scope; installation_role: none.

Then it regenerates the entry's code from the proto, publishes its contract,
generates the typed permissions and the page's TypeScript client, and the
result passes "codefly doctor solution --dir modules/<name>" with no warning.

The scaffolded methods answer Unimplemented until you implement them.`,
	Example: `  codefly add module-page notes --package-id acme/notes
  codefly add module-page notes --package-id acme/notes --title "Team notes" --order 40`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		cli.Init()
		defer services.ClearAgents()

		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		if err := addModulePage(ctx, args[0]); err != nil {
			return fmt.Errorf("cannot add module page: %w", err)
		}
		return ctx.Err()
	},
}

var (
	modulePagePackageID      string
	modulePageTitle          string
	modulePageOrder          int64
	modulePageMinimumCodefly string
	modulePageWithDefault    bool
)

// modulePageEntryAgent scaffolds the entry: the agent whose Connect face the
// host's module edge forwards to, and whose proto the policies mark.
const modulePageEntryAgent = "go-grpc"

// modulePageEntryAnswers shape the entry at creation: Connect, no REST
// gateway. The edge reaches a module only by procedure.
var modulePageEntryAnswers = map[string]bool{"rest-endpoint": false, "connect-endpoint": true}

// addModulePage scaffolds the module page. Everything that can fail on the
// author's input is rendered and validated before the first write, and a
// failure after it removes the module again.
func addModulePage(ctx context.Context, name string) (result error) {
	// The entry is named after the module, so its proto service — and every
	// procedure the host's edge dispatches on — is unique to it.
	entry := name
	documents, err := modulePageDocuments(name, entry)
	if err != nil {
		return err
	}
	hopVersion, err := hopLibraryVersion()
	if err != nil {
		return err
	}
	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	if workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> already exists", name)
	}

	// 1. The module and its entry, through `add module` and `add service`.
	previousModuleDefault, previousDefault := moduleWithDefault, withDefault
	moduleWithDefault, withDefault = modulePageWithDefault, modulePageWithDefault
	defer func() { moduleWithDefault, withDefault = previousModuleDefault, previousDefault }()
	if moduleErr := addModule(name); moduleErr != nil {
		return moduleErr
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, rollBackModule(ctx, name))
		}
	}()
	if serviceErr := addServiceAnswering(ctx, name+"/"+entry, modulePageEntryAgent, modulePageEntryAnswers); serviceErr != nil {
		return fmt.Errorf("cannot add the module's %s service: %w", entry, serviceErr)
	}

	workspace, err = common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot re-load the workspace after scaffolding: %w", err)
	}
	mod, err := workspace.LoadModuleFromName(ctx, name)
	if err != nil {
		return fmt.Errorf("cannot load the scaffolded module: %w", err)
	}

	// 2. The module's declarations: the entry, and its Connect endpoint as the
	// one the host's gateway may reach.
	mod.ServiceEntry = entry
	mod.Interface = modulePageInterface(entry)
	if saveErr := mod.Save(ctx); saveErr != nil {
		return fmt.Errorf("cannot save the module manifest: %w", saveErr)
	}
	if err = applyModulePageEntryShape(ctx, mod, entry); err != nil {
		return err
	}

	// 3. The entry's API, every method marked with its policy.
	serviceDir := filepath.Join(mod.ServicesDir(), entry)
	goModule, err := goModulePath(filepath.Join(serviceDir, "code", "go.mod"))
	if err != nil {
		return err
	}
	protoFiles := map[string][]byte{
		"proto/api.proto":                          []byte(modulePageProto(name, goModule, pageProtoService(name))),
		filepath.Join("proto", hopPolicyProtoPath): hopPolicyProto,
		"proto/buf.yaml":                           []byte(modulePageBufYAML),
		"proto/buf.gen.yaml":                       []byte(modulePageBufGen()),
	}
	for relative, content := range protoFiles {
		if err = shared.WriteFileAtomic(ctx, filepath.Join(serviceDir, relative), content, 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", relative, err)
		}
	}
	// The template's lock and REST document describe the API the proto above
	// replaced; the next buf run writes the lock again.
	for _, stale := range []string{"proto/buf.lock", "openapi"} {
		if err = os.RemoveAll(filepath.Join(serviceDir, stale)); err != nil {
			return fmt.Errorf("cannot remove the template's %s: %w", stale, err)
		}
	}

	// 4. The documents a host reads.
	for _, document := range documents {
		if err = shared.WriteFileAtomic(ctx, filepath.Join(mod.Dir(), filepath.FromSlash(document.name)), document.content, 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", document.name, err)
		}
		cli.Info("wrote %s", document.name)
	}

	// 5. The entry's code, from that proto, by its agent.
	service, err := mod.LoadServiceFromName(ctx, entry)
	if err != nil {
		return fmt.Errorf("cannot load the %s service: %w", entry, err)
	}
	// The agent `add service` loaded acknowledged that call's run; a sync is a
	// run of its own, so it loads the agent afresh.
	services.ClearAgents()
	if err = clisync.Service(ctx, workspace, mod, service, true); err != nil {
		return fmt.Errorf("cannot regenerate the %s service from its proto: %w", entry, err)
	}
	if err = requireGoModule(ctx, filepath.Join(serviceDir, "code"), hopLibraryModule, hopVersion); err != nil {
		return err
	}

	// 6. The published contract the policies are read from, the typed
	// permissions, and the page's client.
	if err = generate.PublishModuleContracts(ctx, workspace, mod); err != nil {
		return fmt.Errorf("cannot publish the %s contract: %w", entry, err)
	}
	if err = generate.ModulePermissions(ctx, mod); err != nil {
		return fmt.Errorf("cannot generate the permissions: %w", err)
	}
	client := name + "-client"
	if err = generate.TypeScriptClient(ctx, name+"/"+entry+"/"+standards.CONNECT, client, client); err != nil {
		return fmt.Errorf("cannot generate the page's client: %w", err)
	}

	// 7. The page remote, on that client.
	remoteDir := filepath.Join(mod.ServicesDir(), "fe-remote")
	clientDir, err := filepath.Rel(remoteDir, filepath.Join(workspace.Dir(), "libraries", client, "typescript"))
	if err != nil {
		return err
	}
	for relative, content := range modulePageRemote(name, modulePageTitleOrDefault(name), client, clientDir) {
		if err = shared.WriteFileAtomic(ctx, filepath.Join(remoteDir, relative), []byte(content), 0o644); err != nil {
			return fmt.Errorf("cannot write services/fe-remote/%s: %w", relative, err)
		}
	}

	// 8. Read back what every reader of the module will see.
	if _, err = modulecontract.Load(mod.Dir()); err != nil {
		return fmt.Errorf("the generated %s is not readable by core: %w", modulecontract.FileName, err)
	}
	if _, err = solutionrun.DeclaredFrontend(ctx, mod); err != nil {
		return fmt.Errorf("the generated %s is not readable by core: %w", solutionrun.FrontendFileName, err)
	}

	cli.Header(1, "Module page <%s> scaffolded: %s (entry, Connect) and fe-remote (./Page).", name, entry)
	reportModulePageNextSteps(name, entry)
	return nil
}

// modulePageDocuments renders the documents at the module root and the page
// declaration beside the entry, each held to core's validator.
func modulePageDocuments(name, entry string) ([]scaffoldedDocument, error) {
	if modulePagePackageID == "" {
		return nil, fmt.Errorf("--package-id <owner>/<name> is required: it is the identity the module is PUBLISHED under, which nothing here can derive — without it the render declares no presence and no host lists the page")
	}
	contract := modulePageContract(name, entry)
	if err := contract.Validate(); err != nil {
		return nil, fmt.Errorf("the generated %s is invalid: %w", modulecontract.FileName, err)
	}
	contractContent, err := marshalModulePageContract(name, contract)
	if err != nil {
		return nil, err
	}

	toolVersion, err := cli.GetCurrentVersion()
	if err != nil {
		return nil, fmt.Errorf("cannot read the running Codefly version: %w", err)
	}
	packaged, err := modulePagePackage(modulePagePackageID, toolVersion, modulePageMinimumCodefly, entry)
	if err != nil {
		return nil, err
	}
	if validateErr := packaged.Validate(); validateErr != nil {
		return nil, fmt.Errorf("the generated %s is invalid: %w", composition.PackageManifestFileName, validateErr)
	}
	packageContent, err := yaml.Marshal(packaged)
	if err != nil {
		return nil, err
	}
	if packageContent, err = dropTopLevelKeys(packageContent, func(_ string, value *yaml.Node) bool { return isNullNode(value) }); err != nil {
		return nil, err
	}

	title := modulePageTitleOrDefault(name)
	frontend, err := marshalSolutionFrontend(solutionFrontend(title, modulePageOrder))
	if err != nil {
		return nil, err
	}
	return []scaffoldedDocument{
		{name: modulecontract.FileName, content: contractContent},
		{name: composition.PackageManifestFileName, content: packageContent},
		{name: "services/" + entry + "/" + solutionrun.FrontendFileName, content: frontend},
		{name: "README.md", content: []byte(modulePageReadme(name, title, entry, name+"-client"))},
	}, nil
}

func modulePageTitleOrDefault(name string) string {
	if modulePageTitle != "" {
		return modulePageTitle
	}
	return titleFromName(name)
}

// applyModulePageEntryShape leaves the entry one endpoint, Connect: the one
// the edge forwards to. One protobuf endpoint is also one published contract,
// so `generate contracts` needs no surface inventory to say which listener
// serves which procedure.
func applyModulePageEntryShape(ctx context.Context, mod *resources.Module, entry string) error {
	return editServiceManifest(ctx, mod, entry, func(root *yaml.Node) error {
		if spec := yamledit.MapValue(root, "spec"); spec == nil || yamledit.MapValue(spec, "connect-endpoint") == nil || yamledit.MapValue(spec, "connect-endpoint").Value != "true" {
			return fmt.Errorf("the %s agent did not scaffold a Connect endpoint for %s (spec.connect-endpoint is not true), and the host's module edge reaches a module only through one", modulePageEntryAgent, entry)
		}
		endpoint := modulePageConnectEndpoint()
		return setEndpoints(root, entry, &endpoint)
	})
}

// goModulePath is the module path a go.mod declares.
func goModulePath(goMod string) (string, error) {
	content, err := os.ReadFile(goMod)
	if err != nil {
		return "", fmt.Errorf("cannot read the entry's go.mod: %w", err)
	}
	path := modfile.ModulePath(content)
	if path == "" {
		return "", fmt.Errorf("%s declares no module path", goMod)
	}
	return path, nil
}

// hopLibraryVersion is the version of the authority's hop library this CLI is
// built against: the one whose policy option the vendored proto copies, so
// the entry registers the same descriptor its proto imports.
func hopLibraryVersion() (string, error) {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == hopLibraryModule {
				if dependency.Replace != nil && dependency.Replace.Version != "" {
					return dependency.Replace.Version, nil
				}
				return dependency.Version, nil
			}
		}
	}
	return "", fmt.Errorf("this Codefly does not record the version of %s it was built against, so the entry cannot require the library that registers its policy option", hopLibraryModule)
}

// requireGoModule adds module@version to the Go module at dir, with its
// checksums: the generated code imports the policy option's package from it.
func requireGoModule(ctx context.Context, dir, module, version string) error {
	// module is a constant of this command and version the one its own build
	// records, so nothing an author typed reaches the arguments.
	command := exec.CommandContext(ctx, "go", "get", module+"@"+version) //nolint:gosec // see above
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		// The library is private: the public proxy and checksum database
		// cannot see it, and GOPRIVATE is the host's to set, never this
		// command's.
		owner, _, _ := strings.Cut(strings.TrimPrefix(module, "github.com/"), "/")
		return fmt.Errorf("cannot require %s@%s in %s: %w: %s — the generated code imports its policy option's package from it. If the module proxy or checksum database cannot see it, set GOPRIVATE=github.com/%s/* and run the scaffold again",
			module, version, dir, err, strings.TrimSpace(string(output)), owner)
	}
	return nil
}

// reportModulePageNextSteps names what the scaffold left to the author.
func reportModulePageNextSteps(name, entry string) {
	cli.Info("Next:")
	cli.Info("  1. Implement the methods in services/%s/code: they answer Unimplemented until you do.", entry)
	cli.Info("     Change what one requires in proto/api.proto, then its role in %s, together.", modulecontract.FileName)
	cli.Info("  2. Build the page: cd services/fe-remote && npm run client && npm install && npm run build")
	cli.Info("  3. Check the roles: codefly doctor solution --dir modules/%s --roles", name)
	cli.Info("  Access: the module is installed by composition in every organization; nobody can use the")
	cli.Info("  page until an administrator grants a role (\"Set up access\" in Admin: %s-installer,", name)
	cli.Info("  %s-editor, %s-viewer).", name, name)
}

func init() {
	ModulePageCmd.Flags().StringVar(&modulePagePackageID, "package-id", "", "The <owner>/<name> identity the module is published under. Required; nothing here can derive who publishes it")
	ModulePageCmd.Flags().StringVar(&modulePageTitle, "title", "", "The page's title in the host's shell (default: the module name, capitalised)")
	ModulePageCmd.Flags().Int64Var(&modulePageOrder, "order", 50, "The page's order in the host's shell")
	ModulePageCmd.Flags().StringVar(&modulePageMinimumCodefly, "minimum-codefly-version", "", "The Codefly range the package declares (default: the running release, open to the end of its major). Required when scaffolding with a build from source, which reports no release version")
	ModulePageCmd.Flags().BoolVar(&modulePageWithDefault, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
