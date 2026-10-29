package add

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// SolutionCmd scaffolds a solution in the one shape a solution has.
//
// It composes verbs the CLI already has — `add module`, then `add service`
// twice — and writes the documents core already specifies, by marshalling
// core's own types and reading each file back through core's own reader. It
// introduces no executor protocol and no new agent: core's Lifecycle.Create is
// declared by nothing and implemented by nothing, so `create` here is the CLI
// doing what an executor would have been asked to do (codefly-dev/cli#1215).
var SolutionCmd = &cobra.Command{
	Use:   "solution",
	Short: "Scaffold a solution: its backend, its page's shell, and the documents a host installs it from",
	Long: `Scaffold a solution in the one shape a solution has.

A solution is two Codefly services and a page:

  backend   The entry service. It presents the solution's identity, serves the
            API the page calls, and is the workload a host installs. It is
            what the solution is published as.
  web       The shell that serves the page where no host shell mounts it — the
            same ./Page a saas host federates, in a shell of its own, which an
            authority's edge serves as the browser's one origin. The package
            does not publish it: a host that federates the remote needs no
            shell.
  fe-remote The ./Page remote itself. It is an npm package, not a Codefly
            service, so this command does not scaffold it.

THE RULE THE BACKEND LIVES UNDER. A module call the authority holds to a
PERSON — one its artifact interface admits only in person, with no intervening
actor — is made by the PAGE, with that person's own Work Context. It is never
relayed through the backend: a backend reaches a module on a context minted
for the solution's own audience, so relaying such a call puts the solution
between the person and the module, and admission refuses it. Reads and work
that are the solution's own go through the backend as usual.

--agent names the agent that scaffolds the backend, and it must be a SOLUTION
agent (default: solution-go). The service agent of the same language is a
different thing: the go agent scaffolds a service — no declared operation, no
published policy, no page remote — and delivery refuses the result. Naming
one is refused here, with the solution agent to use instead.

What this writes, and what stays yours: the module, both services, the
solution manifest, the module contract, the package identity and the page
declaration are written and each is read back through core's own validator.
The COMPOSITION is not: which workspace composes this solution, which modules
it pins and at what commits, and the configuration groups those modules need,
are the composition's to say and are never guessed here.`,
	Example: `  # A solution with a Go backend that calls two modules.
  codefly add solution wiki --package-id obin/wiki \
      --consumes connect:documents/documents/grpc --consumes connect:robin/chat/connect

  # A solution agent other than the default.
  codefly add solution forecast --package-id obin/forecast --agent solution-python`,
	Args: cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		cli.Init()
		defer services.ClearAgents()

		ctx, done := common.NewContext()
		defer done()

		ctx, stop := common.SignalContext(ctx)
		defer stop()

		if err := addSolution(ctx, args[0]); err != nil {
			return fmt.Errorf("cannot add solution: %w", err)
		}
		return ctx.Err()
	},
}

var (
	solutionAgentInput     string
	solutionConsumes       []string
	solutionPackageID      string
	solutionExecutor       string
	solutionDescription    string
	solutionTitle          string
	solutionOrder          int64
	solutionAuthorityIface string
	solutionMinimumCodefly string
	solutionWithDefault    bool
)

// addSolution scaffolds the solution. Everything that can fail on the author's
// input — the consumed bindings, a document the schema refuses — is rendered
// and validated BEFORE the first write, so a mistyped flag leaves no
// half-built module behind.
func addSolution(ctx context.Context, name string) (result error) {
	w := wool.Get(ctx).In("cmd.add.solution")

	documents, err := solutionDocuments(ctx, name)
	if err != nil {
		return err
	}

	// Resolved with the other input, BEFORE the first write: naming the
	// service agent instead of the solution agent is an ordinary mistake and
	// must cost nothing, not half a module.
	entryAgent, err := solutionAgentFor(solutionAgentInput)
	if err != nil {
		return err
	}

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot load workspace")
	}
	if workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> already exists", name)
	}

	// 1. The module, through `add module`'s own path — including its rollback
	// of a module whose creation then fails.
	previousModuleDefault := moduleWithDefault
	moduleWithDefault = solutionWithDefault
	defer func() { moduleWithDefault = previousModuleDefault }()
	if moduleErr := addModule(name); moduleErr != nil {
		return moduleErr
	}
	// `add module` rolls back only what fails inside its own call. Everything
	// below can fail with the module already written — an agent that cannot
	// scaffold the service is the ordinary case, not a hypothetical: the
	// generic `python` agent runs a Python service but defines no project
	// template, so `--agent python` always fails here. Without this the
	// author is left a module and a half-built service to delete by hand
	// before they can retry.
	defer func() {
		if result == nil {
			return
		}
		// Re-read, for the same reason step 3 does: the handle loaded above
		// predates the module, so deleting through it reports that the module
		// <name> does not exist and leaves it on disk.
		current, loadErr := common.LoadWorkspace(ctx)
		if loadErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back: cannot re-load the workspace: %w", loadErr))
			return
		}
		moduleDir := current.ModulePath(ctx, &resources.ModuleReference{Name: name})
		if rollbackErr := current.DeleteModule(ctx, name); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back module reference: %w", rollbackErr))
			return
		}
		if rollbackErr := os.RemoveAll(moduleDir); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back module directory: %w", rollbackErr))
		}
	}()

	// 2. Both services, through the agents' own Create flows — the same call
	// `add service` makes.
	previousWithDefault := withDefault
	withDefault = solutionWithDefault
	defer func() { withDefault = previousWithDefault }()
	for _, scaffolded := range []struct{ service, agent string }{
		{solutionEntryService, entryAgent},
		{solutionPageService, solutionPageAgent},
	} {
		target := fmt.Sprintf("%s/%s", name, scaffolded.service)
		if serviceErr := addService(ctx, target, scaffolded.agent); serviceErr != nil {
			return fmt.Errorf("cannot add the solution's %s service: %w", scaffolded.service, serviceErr)
		}
	}

	// Re-read the workspace: the handle above was loaded before `add module`
	// wrote the module into it, so it still lists the modules that existed
	// when this command started. `add service` re-reads for the same reason,
	// which is why the two services above resolved and this did not.
	workspace, err = common.LoadWorkspace(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot re-load the workspace after scaffolding")
	}
	mod, err := workspace.LoadModuleFromName(ctx, name)
	if err != nil {
		return w.Wrapf(err, "cannot load the scaffolded module")
	}

	// 3. The module's own declarations, written after both services exist so
	// service-entry and the interface never name a service that is not there.
	mod.ServiceEntry = solutionEntryService
	if solutionDescription != "" {
		mod.Description = solutionDescription
	}
	mod.Interface = solutionModuleInterface()
	if err := mod.Save(ctx); err != nil {
		return w.Wrapf(err, "cannot save the module manifest")
	}

	// 4. The shape each service carries, on the manifest its agent just wrote.
	if err := applyEntryServiceShape(ctx, mod); err != nil {
		return err
	}
	if err := applyPageServiceShape(ctx, mod); err != nil {
		return err
	}

	// 5. The page shell, over the generic app the nextjs agent scaffolded.
	// It runs here because it is the only step after BOTH agents: the nextjs
	// agent would overwrite anything the solution agent wrote into its
	// service, and what it leaves on its own is a standalone web app with
	// placeholder login routes and "Your app is running" — not a solution.
	if err := applySolutionShell(ctx, mod, name+"-fe-remote"); err != nil {
		return err
	}

	// 6. The documents. The three at the module root, and the page
	// declaration beside the entry service.
	for _, document := range documents {
		path := filepath.Join(mod.Dir(), filepath.FromSlash(document.name))
		if err := shared.WriteFileAtomic(ctx, path, document.content, 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %w", document.name, err)
		}
		cli.Info("wrote %s", document.name)
	}
	// Read the contract back through core's own loader: the bytes on disk are
	// what every reader of this solution will see.
	if _, err := modulecontract.Load(mod.Dir()); err != nil {
		return fmt.Errorf("the generated %s is not readable by core: %w", modulecontract.FileName, err)
	}
	if _, err := solutionrun.DeclaredFrontend(ctx, mod); err != nil {
		return fmt.Errorf("the generated %s is not readable by core: %w", solutionrun.FrontendFileName, err)
	}

	// 7. The backend's published contract, and its entry in the package.
	//
	// The authority derives the method policy of every route from it, so a
	// solution whose package carries none admits no call to any route it
	// serves — and says so only as a warning in the authority's log, during
	// a run. Generated here so what this command leaves behind is a solution
	// that conforms, rather than one whose first `codefly doctor solution`
	// fails on a step the author had no reason to know about.
	if err := generate.PublishModuleContracts(ctx, workspace, mod); err != nil {
		return fmt.Errorf("cannot publish the backend's contract: %w", err)
	}
	cli.Info("wrote contracts/api and the package's api-contracts entry")

	cli.Header(1, "Solution <%s> scaffolded: %s (entry) and %s (the page's shell).", name, solutionEntryService, solutionPageService)
	reportNextSteps(name)
	return nil
}

// scaffoldedDocument is one file the scaffold writes, rendered and validated
// before anything is created. name is slash-separated, relative to the module.
type scaffoldedDocument struct {
	name    string
	content []byte
}

// solutionDocuments renders every document the solution ships, holding each to
// the validator core owns for it, before the first write.
func solutionDocuments(ctx context.Context, name string) ([]scaffoldedDocument, error) {
	if solutionPackageID == "" {
		return nil, fmt.Errorf("--package-id <owner>/<name> is required: it is the identity the solution is PUBLISHED under, which nothing here can derive — without it the render declares no presence, the host never lists the solution, and nobody can install it")
	}

	var consumes []manifest.APIDeclaration
	for _, value := range solutionConsumes {
		declaration, err := parseConsumedAPI(value)
		if err != nil {
			return nil, err
		}
		consumes = append(consumes, declaration)
	}

	var executor *resources.Agent
	if solutionExecutor != "" {
		parsed, err := resources.ParseAgent(ctx, resources.SolutionAgent, solutionExecutor)
		if err != nil {
			return nil, fmt.Errorf("cannot read --executor %q: %w", solutionExecutor, err)
		}
		executor = parsed
	}

	solution, err := marshalSolutionManifest(solutionManifest(consumes, executor))
	if err != nil {
		return nil, err
	}

	contract, err := yaml.Marshal(solutionContract(name))
	if err != nil {
		return nil, err
	}

	toolVersion, err := cli.GetCurrentVersion()
	if err != nil {
		return nil, fmt.Errorf("cannot read the running Codefly version: %w", err)
	}
	packaged, err := solutionPackage(solutionPackageID, toolVersion, solutionMinimumCodefly)
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
	// A nil optional (required-qualifications) renders as an explicit null,
	// which decodes the same as absent and reads as a decision nobody made.
	if packageContent, err = dropTopLevelKeys(packageContent, func(_ string, value *yaml.Node) bool {
		return isNullNode(value)
	}); err != nil {
		return nil, err
	}

	title := solutionTitle
	if title == "" {
		title = titleFromName(name)
	}
	frontend, err := marshalSolutionFrontend(solutionFrontend(title, solutionOrder))
	if err != nil {
		return nil, err
	}

	return []scaffoldedDocument{
		{name: manifest.FileName, content: solution},
		{name: modulecontract.FileName, content: []byte(contractPreamble + string(contract))},
		{name: composition.PackageManifestFileName, content: []byte(packagePreamble + string(packageContent))},
		{name: "services/" + solutionEntryService + "/" + solutionrun.FrontendFileName, content: frontend},
	}, nil
}

// titleFromName is the page title a solution gets when --title names none: the
// module name, hyphens read as spaces, each word capitalised. It is the
// product's name, so it is the author's to give — this is only a legible
// placeholder rather than the raw slug in the host's shell.
func titleFromName(name string) string {
	words := strings.Split(name, "-")
	for index, word := range words {
		if word == "" {
			continue
		}
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// applyEntryServiceShape states the solution shape on the backend manifest the
// service agent just wrote, editing the nodes in place so the agent's own
// comments and every key it set that the shape does not speak to survive.
func applyEntryServiceShape(ctx context.Context, mod *resources.Module) error {
	return editServiceManifest(ctx, mod, solutionEntryService, func(root *yaml.Node) error {
		entry := solutionEntryEndpoint()
		if setErr := setEndpoints(root, solutionEntryService, &entry); setErr != nil {
			return setErr
		}
		// The backend presents the solution's identity: the authority takes
		// its projected workload credential as the actor of every Work Context
		// it issues the solution, so the authority document the CLI renders
		// from the module contract approves this service's build — and the CLI
		// refuses an authority request with no such service.
		identity, err := yamledit.Encode(true)
		if err != nil {
			return err
		}
		yamledit.SetMapValue(root, "module-identity", identity)

		dependencies, err := yamledit.Encode(solutionEntryDependencies(solutionAuthorityIface))
		if err != nil {
			return err
		}
		yamledit.SetMapValue(root, "service-dependencies", dependencies)

		// The authority interface's own address. A binding added to the module
		// contract later adds its own groups beside this one.
		groups, err := yamledit.Encode([]string{"authority"})
		if err != nil {
			return err
		}
		yamledit.SetMapValue(root, "workspace-configuration-dependencies", groups)
		return nil
	})
}

// applyPageServiceShape states the shape on the shell's manifest. The shell
// declares no dependency and no identity: it serves a page, and every call
// that page makes is the browser's, not the shell's.
func applyPageServiceShape(ctx context.Context, mod *resources.Module) error {
	return editServiceManifest(ctx, mod, solutionPageService, func(root *yaml.Node) error {
		page := solutionPageEndpoint()
		return setEndpoints(root, solutionPageService, &page)
	})
}

// setEndpoints replaces the service's endpoints with the one the shape
// declares. It SETS rather than merges: a solution's service serves one
// endpoint, and a port an agent template happened to scaffold is not part of
// the solution and would be one more surface the cell has to account for. The
// replacement is reported rather than silent.
func setEndpoints(root *yaml.Node, service string, endpoint *resources.Endpoint) error {
	if existing := yamledit.MapValue(root, "endpoints"); existing != nil && len(existing.Content) > 0 {
		cli.Info("%s: replacing the %d endpoint(s) the agent scaffolded with the solution's one %s endpoint", service, len(existing.Content), endpoint.Name)
	}
	encoded, err := yamledit.Encode([]resources.Endpoint{*endpoint})
	if err != nil {
		return err
	}
	yamledit.SetMapValue(root, "endpoints", encoded)
	return nil
}

// editServiceManifest applies edit to a scaffolded service's manifest,
// preserving the agent's own comments and unmodeled keys.
func editServiceManifest(ctx context.Context, mod *resources.Module, service string, edit func(root *yaml.Node) error) error {
	path := filepath.Join(mod.ServicesDir(), service, resources.ServiceConfigurationName)
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cannot read the %s service manifest: %w", service, err)
	}
	document, root, err := yamledit.Document(original)
	if err != nil {
		return fmt.Errorf("cannot parse the %s service manifest: %w", service, err)
	}
	if editErr := edit(root); editErr != nil {
		return editErr
	}
	updated, err := yamledit.Marshal(document)
	if err != nil {
		return err
	}
	if err := shared.WriteFileAtomic(ctx, path, updated, 0o644); err != nil {
		return fmt.Errorf("cannot write the %s service manifest: %w", service, err)
	}
	return nil
}

// reportNextSteps names what the scaffold deliberately did not decide, so the
// author meets each one here rather than as a failed run.
func reportNextSteps(name string) {
	cli.Info("Next, and none of it derivable from here:")
	cli.Info("  1. Author services/fe-remote — the ./Page remote the shell compiles and a host")
	cli.Info("     federates. It is an npm package, not a Codefly service.")
	cli.Info("  2. Compose the modules this solution calls, and COPY EACH CONFIGURATION GROUP WHOLE.")
	cli.Info("     A group is the set its consumers declare, not the modules you call: trimming one")
	cli.Info("     gets `module contract slot cannot be resolved`, `slot resolves ambiguously`, or")
	cli.Info("     `it is not a module this workspace composes`. Disable behaviour by the module's own flag.")
	cli.Info("  3. Pin every composed module at a DEFAULT-BRANCH commit. A PR head dies on squash-merge.")
	cli.Info("  4. Add a binding to %s per module call made ON A PERSON'S BEHALF. A call the", modulecontract.FileName)
	cli.Info("     authority holds to a person IN PERSON takes no binding: the page makes it, as the person.")
	cli.Info("  Running it locally: do not pass --temporary-ports — it breaks ${endpoint:...} for")
	cli.Info("  anything resolved before init. Exclude any converter service whose local run needs an")
	cli.Info("  image the CLI does not pass (codefly-dev/cli#932).")
	cli.Info("  The solution is <%s> to the composition, and %s to whoever publishes it.", name, solutionPackageID)
}

func init() {
	SolutionCmd.Flags().StringVar(&solutionPackageID, "package-id", "", "The <owner>/<name> identity the solution is published under. Required; nothing here can derive who publishes it")
	SolutionCmd.Flags().StringVar(&solutionAgentInput, "agent", "", "Agent that scaffolds the backend (default: "+solutionEntryAgent+"). Must be a SOLUTION agent; naming the service agent of a language is refused, with the one to use instead")
	SolutionCmd.Flags().StringArrayVar(&solutionConsumes, "consumes", nil, "A module this solution calls, as [<id>=]<protocol>:<module>/<service>/<endpoint>. Repeatable. Route names only — authority is declared once, in the module contract")
	SolutionCmd.Flags().StringVar(&solutionExecutor, "executor", "", "A codefly:solution executor to name in the solution manifest. Omitted by default: core specifies no executor protocol and none is published (codefly-dev/cli#1215)")
	SolutionCmd.Flags().StringVar(&solutionDescription, "description", "", "Short description written to module.codefly.yaml")
	SolutionCmd.Flags().StringVar(&solutionTitle, "title", "", "The page's title in the host's shell (default: the module name, capitalised)")
	SolutionCmd.Flags().Int64Var(&solutionOrder, "order", 50, "The page's order in the host's shell")
	SolutionCmd.Flags().StringVar(&solutionAuthorityIface, "authority-interface", authorityInterfaceRange, "The authority interface the backend requires, as <publisher>/<name>@<range>")
	SolutionCmd.Flags().StringVar(&solutionMinimumCodefly, "minimum-codefly-version", "", "The Codefly range the package declares (default: the running release, open to the end of its major). Required when scaffolding with a build from source, which reports no release version")
	SolutionCmd.Flags().BoolVar(&solutionWithDefault, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
