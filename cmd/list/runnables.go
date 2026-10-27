package list

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/environments"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	listRunnablesModule string
	listRunnablesJSON   bool
	listRunnablesEnv    string
	listRunnablesTools  bool
)

// RunnablesCmd lists the runnables declared in the current workspace.
var RunnablesCmd = &cobra.Command{
	Use:   "runnables",
	Short: "List runnables across the workspace or within one module",
	Long: `List the runnables (typed finite operations) declared in the workspace.

A runnable is identified by module/name @ version: a version is an immutable
release and several versions of one name coexist.

Every Runnable a module derives from its published contracts is listed beside
the ones it declares, with the tool it is offered as when its owner exposed one
— the model-facing name and the explicit effect. An operation with no exposure
is not a tool and is shown without one: absent means not exposed.

With --env, each derived operation also carries the configuration key its
authority is answered under (` + "`runnable-scope-selections`" + ` in
workspace.codefly.yaml) and what that environment has answered:

  selected      a binding is prepared: no slot to answer, or an answer resolves it
  declined      answered [] — installed for no resource, granted no authority
  needs-scopes  its owner asks for scope slots nobody has answered yet

That last state is how a module version offering a new tool becomes visible:
the operation appears, nothing answers it, and generation refuses it by name
rather than preparing a binding with the question still open.

Examples:
  codefly list runnables
  codefly list runnables --module=backend
  codefly list runnables --tools --env=staging
  codefly list runnables --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return listRunnables(cmd)
	},
}

func listRunnables(cmd *cobra.Command) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	var modules []*resources.Module
	if listRunnablesModule != "" {
		var mod *resources.Module
		mod, err = workspace.LoadModuleFromName(ctx, listRunnablesModule)
		if err != nil {
			return fmt.Errorf("module not found: %w", err)
		}
		modules = []*resources.Module{mod}
	} else if modules, err = workspace.LoadModules(ctx); err != nil {
		return fmt.Errorf("cannot load modules: %w", err)
	}

	// The environment is resolved once, before any module is read: a listing
	// asked about an environment the workspace does not declare answers with
	// that, rather than with rows whose grant column is silently absent.
	var env *environments.Environment
	if listRunnablesEnv != "" {
		if env, err = environments.Select(workspace, listRunnablesEnv); err != nil {
			return fmt.Errorf("cannot list the grants of environment %q: %w", listRunnablesEnv, err)
		}
	}

	entries, err := runnableEntries(ctx, modules, env)
	if err != nil {
		return err
	}

	if listRunnablesJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
	}
	if len(entries) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No runnables in workspace <%s>\n", workspace.Name)
		return nil
	}
	return writeRunnables(cmd, entries, env != nil)
}

// runnableEntries is every runnable the modules hold: the ones they declare,
// and the operations they derive. With env, each derived row carries how that
// environment answers it.
func runnableEntries(ctx context.Context, modules []*resources.Module, env *environments.Environment) ([]runnablespkg.Identity, error) {
	entries := make([]runnablespkg.Identity, 0)
	for _, mod := range modules {
		runnables, err := mod.LoadRunnables(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot load runnables: %w", err)
		}
		for _, r := range runnables {
			entries = append(entries, runnablespkg.NewIdentity(r))
		}
		// A module's derived operations are runnables it can be asked to
		// perform just as much as the ones it declares, so one listing
		// answers the question without the caller knowing which is which.
		derived, err := runnablespkg.LoadDerivedOperations(mod.Dir())
		if err != nil {
			return nil, fmt.Errorf("cannot load the derived runnables of module %s: %w", mod.Name, err)
		}
		for i := range derived {
			identity := derived[i].Identity()
			if env != nil {
				identity.Grant = grantOf(env, mod.Name, &derived[i])
			}
			entries = append(entries, identity)
		}
	}
	if !listRunnablesTools {
		return entries, nil
	}
	tools := make([]runnablespkg.Identity, 0, len(entries))
	for i := range entries {
		if entries[i].Tool != nil {
			tools = append(tools, entries[i])
		}
	}
	return tools, nil
}

// grantOf is how env answers one derived operation, judged the way
// `generate runnable-bindings` judges it: an explicitly empty list declines,
// an owner's unanswered scope slot is refused rather than skipped, and
// everything else prepares a binding.
func grantOf(env *environments.Environment, module string, operation *runnablespkg.Derived) *runnablespkg.Grant {
	key := generate.RunnableBindingKey(module, operation.Entry.Name)
	grant := &runnablespkg.Grant{Key: key, State: runnablespkg.GrantSelected, Slots: operation.Slots()}
	switch {
	case env.DeclinesRunnableBinding(key):
		grant.State = runnablespkg.GrantDeclined
	case len(grant.Slots) > 0 && len(env.RunnableScopeSelections[key]) == 0:
		grant.State = runnablespkg.GrantNeedsScopes
	}
	return grant
}

func writeRunnables(cmd *cobra.Command, entries []runnablespkg.Identity, grants bool) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	header := "MODULE\tNAME\tVERSION\tAGENT\tFACILITIES\tTIMEOUT\tSOURCE\tTOOL\tEFFECT"
	if grants {
		header += "\tKEY\tGRANT"
	}
	fmt.Fprintln(w, header)
	for index := range entries {
		e := &entries[index]
		source := e.Source
		if source == "" {
			source = "declared"
		}
		tool, effect := "", ""
		if e.Tool != nil {
			tool, effect = e.Tool.Name, e.Tool.Effect
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			e.Module, e.Name, e.Version, e.Agent, strings.Join(e.Execution.Facilities, ","), e.Execution.Timeout, source, tool, effect)
		if grants {
			key, state := "", ""
			if e.Grant != nil {
				key, state = e.Grant.Key, e.Grant.State
			}
			row += "\t" + key + "\t" + state
		}
		fmt.Fprintln(w, row)
	}
	return w.Flush()
}

func init() {
	RunnablesCmd.Flags().StringVar(&listRunnablesModule, "module", "", "Module to list runnables from")
	RunnablesCmd.Flags().BoolVar(&listRunnablesJSON, "json", false, "Emit machine-readable JSON")
	RunnablesCmd.Flags().StringVar(&listRunnablesEnv, "env", "", "Environment whose runnable-scope-selections to report as each operation's grant")
	RunnablesCmd.Flags().BoolVar(&listRunnablesTools, "tools", false, "List only the operations their owner exposes as a tool")
}
