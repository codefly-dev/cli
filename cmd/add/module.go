package add

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/codefly-dev/cli/cmd/common"
	modulesync "github.com/codefly-dev/cli/cmd/sync"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionsmodule "github.com/codefly-dev/core/actions/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ModuleCmd represents the run command
var ModuleCmd = &cobra.Command{
	Use:   "module",
	Short: "Create a module to group related services and jobs",
	Args:  cobra.ExactArgs(1),

	RunE: func(cmd *cobra.Command, args []string) error {
		if interactive {
			return fmt.Errorf("interactive mode not implemented yet")
		}
		if moduleSource != "" {
			return addReferencedModule(args[0])
		}
		return addModule(args[0])
	},
}

var moduleAgentInput string
var moduleSource string
var moduleWithDefault bool

// addReferencedModule registers a module by reference — pointing at an
// out-of-repo directory — instead of vendoring a copy under modules/<name>/.
// This lets a solution repo be the composition root that references the host
// and runtime modules it does not own; `codefly run` boots them alongside
// local modules because resolution flows through the reference's path override.
func addReferencedModule(name string) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}
	if workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> already exists", name)
	}

	source, err := filepath.Abs(moduleSource)
	if err != nil {
		return fmt.Errorf("cannot resolve source path %q: %w", moduleSource, err)
	}
	mod, err := resources.LoadModuleFromDir(ctx, source)
	if err != nil {
		return fmt.Errorf("cannot load referenced module at %s: %w", source, err)
	}
	if mod.Name != name {
		return fmt.Errorf("referenced module at %s is named <%s>, not <%s>", source, mod.Name, name)
	}

	// Store a workspace-relative override when the source lives inside the
	// workspace so the reference stays portable; fall back to the absolute
	// path for the out-of-repo case, which is the whole point of a reference.
	stored := source
	if rel, relErr := filepath.Rel(workspace.Dir(), source); relErr == nil && filepath.IsLocal(rel) {
		stored = rel
	}

	if err := workspace.AddModuleReference(&resources.ModuleReference{Name: name, PathOverride: &stored}); err != nil {
		return fmt.Errorf("cannot add module reference: %w", err)
	}
	if err := workspace.Save(ctx); err != nil {
		return fmt.Errorf("cannot save workspace: %w", err)
	}

	cli.Header(2, "Referenced module <%s> added from %s.", name, source)
	return nil
}

func addModule(name string) (result error) {
	ctx, done := common.NewContext()
	defer done()

	w := wool.Get(ctx).In("cmd.add.module")

	// Non-interactive mode: skip all TUI prompts (for MCP, CI, scripts)
	cli.SetWithDefault(moduleWithDefault)

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	if workspace.ExistsModule(name) {
		return fmt.Errorf("module <%s> already exists", name)
	}

	// In non-interactive mode (--yes), skip confirmation
	if !moduleWithDefault {
		confirm := models.Confirm(ctx, fmt.Sprintf("Add a module in your workspace <%s>?", workspace.Name), true)
		if !confirm {
			cli.Header(2, "Received loud and clear!")
			return nil
		}
	}

	// Resolve module agent if specified
	var agent *resources.Agent
	if moduleAgentInput != "" {
		var err error
		agent, err = common.GetModuleAgent(ctx, moduleAgentInput)
		if err != nil {
			return fmt.Errorf("cannot resolve module agent: %w", err)
		}
		cli.Header(2, "Using module template: %s", agent.Identifier())
	}

	input := &actionsmodule.AddModule{
		Name: name,
	}
	if agent != nil {
		// Assign through input.Agent rather than a temporary: a `:=` here would
		// shadow the enclosing err.
		input.Agent, err = agent.Proto()
		if err != nil {
			return fmt.Errorf("cannot resolve agent %s: %w", agent.Identifier(), err)
		}
	}
	var preparedSource *modulesync.PreparedModuleSource
	if agent != nil {
		targetRoot := workspace.ModulePath(ctx, &resources.ModuleReference{Name: name})
		preparedSource, err = modulesync.PrepareModuleSource(ctx, targetRoot, agent)
		if err != nil {
			return fmt.Errorf("prepare module scaffold source: %w", err)
		}
		defer preparedSource.Close()
	}

	action, err := actionsmodule.NewActionAddModule(ctx, input)
	if err != nil {
		return fmt.Errorf("cannot create action: %w", err)
	}
	out, err := actions.Run(ctx, action, &actions.Space{Workspace: workspace})
	if err != nil {
		return fmt.Errorf("cannot add module: %w", err)
	}
	moduleAdded := true
	moduleDir := workspace.ModulePath(ctx, &resources.ModuleReference{Name: name})
	defer func() {
		if result == nil || !moduleAdded {
			return
		}
		if rollbackErr := workspace.DeleteModule(ctx, name); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back module reference: %w", rollbackErr))
			return
		}
		if rollbackErr := os.RemoveAll(moduleDir); rollbackErr != nil {
			result = errors.Join(result, fmt.Errorf("roll back module directory: %w", rollbackErr))
		}
	}()

	mod, err := actions.As[resources.Module](out)
	if err != nil {
		return fmt.Errorf("cannot read added module: %w", err)
	}

	// If a module agent was specified, execute it to scaffold services and templates
	if agent != nil {
		binPath, err := agent.Path(ctx)
		if err != nil {
			return fmt.Errorf("cannot resolve module agent binary path: %w", err)
		}
		w.Info("executing module agent", wool.Field("binary", binPath), wool.Field("dir", mod.Dir()))

		cmd := exec.CommandContext(ctx, binPath, mod.Dir(), name)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("module agent failed: %w", err)
		}
		if err := preparedSource.Pin(mod); err != nil {
			return fmt.Errorf("pin module scaffold source: %w", err)
		}
		cli.Header(2, "Module agent scaffolded services for <%s>", name)
	}

	moduleAdded = false
	cli.Header(2, "Module <%s> added.", mod.Name)
	return nil
}

func init() {
	ModuleCmd.PersistentFlags().BoolVarP(&interactive, "interactive", "i", false, "interactive mode")
	ModuleCmd.Flags().StringVar(&moduleAgentInput, "agent", "", "Module template agent (e.g. user-management, rag)")
	ModuleCmd.Flags().StringVar(&moduleSource, "source", "", "Reference an existing out-of-repo module by path instead of vendoring a copy")
	ModuleCmd.MarkFlagsMutuallyExclusive("agent", "source")
	ModuleCmd.Flags().BoolVar(&moduleWithDefault, "yes", false, "Skip confirmation prompts (non-interactive/MCP mode)")
}
