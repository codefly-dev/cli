package list

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
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
the ones it declares. With --tools or --env, each row also carries the tool its
owner exposed it as — the model-facing name and the explicit effect. An
operation with no exposure is not a tool and is shown without one: absent means
not exposed. --tools keeps only the exposed ones.

With --env, each derived operation also carries the configuration key its
authority is answered under (` + "`runnable-scope-selections`" + ` in
workspace.codefly.yaml) and what that environment has answered, judged by the
rules ` + "`generate runnable-bindings`" + ` judges it by:

  selected      its answer resolves every slot the owner declared, or it declares none
  declined      answered [] — installed for no resource, granted no authority
  needs-scopes  its owner asks for scope slots nobody has answered
  invalid       generation refuses this answer, and the row says why

An answer naming no operation at all is reported beside the rows rather than
dropped, because a mistyped key answers no question and leaves its operation
reading as unanswered.

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

// runnableRow is one listed runnable. The tool and the grant live here rather
// than on the shared projection because they are this listing's question: a
// field on runnablespkg.Identity would also appear in `show runnable`, whose
// report already states the operation's own exposure.
type runnableRow struct {
	runnablespkg.Identity
	// Tool is the exposure the owner declared, nil when the operation is no
	// tool. Absent means not exposed: that is the owner's fail-closed default
	// and this listing repeats it rather than inventing one.
	Tool *runnablespkg.Tool `json:"tool,omitempty"`
	// Grant is how the environment answers this operation, set only when a
	// listing was asked about one.
	Grant *runnablespkg.Grant `json:"grant,omitempty"`
}

// runnableListing is what a listing asked about an environment answers with:
// the rows, and the answers that match no operation. Without --env there is no
// environment to have answered anything, and the listing stays the array it
// has always been.
type runnableListing struct {
	Runnables []runnableRow `json:"runnables"`
	// UnansweredKeys are the runnable-scope-selections keys of this
	// environment that no listed operation binds. Generation refuses each by
	// name; a listing that only walked the catalogue would never show them.
	UnansweredKeys []string `json:"unanswered_keys,omitempty"`
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

	listing, err := runnableListingOf(ctx, modules, env)
	if err != nil {
		return err
	}

	if listRunnablesJSON {
		if env == nil {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(listing.Runnables)
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(listing)
	}
	if len(listing.Runnables) == 0 && len(listing.UnansweredKeys) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No runnables in workspace <%s>\n", workspace.Name)
		return nil
	}
	return writeRunnables(cmd, listing, env)
}

// runnableListingOf is every runnable the modules hold — the ones they declare
// and the operations they derive — and, when an environment was named, how it
// answers each derived operation and which of its answers match nothing.
func runnableListingOf(ctx context.Context, modules []*resources.Module, env *environments.Environment) (*runnableListing, error) {
	listing := &runnableListing{Runnables: make([]runnableRow, 0)}
	bound := map[string]bool{}
	for _, mod := range modules {
		runnables, err := mod.LoadRunnables(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot load runnables: %w", err)
		}
		for _, r := range runnables {
			listing.Runnables = append(listing.Runnables, runnableRow{Identity: runnablespkg.NewIdentity(r)})
		}
		// A module's derived operations are runnables it can be asked to
		// perform just as much as the ones it declares, so one listing
		// answers the question without the caller knowing which is which.
		derived, err := runnablespkg.LoadDerivedOperations(mod.Dir())
		if err != nil {
			return nil, fmt.Errorf("cannot load the derived runnables of module %s: %w", mod.Name, err)
		}
		for i := range derived {
			row := runnableRow{Identity: derived[i].Identity(), Tool: derived[i].ToolExposure()}
			if env != nil {
				// ScopeSelections is core's own ScopeSelection messages: the
				// environment decoded them, so the rules are applied to what
				// an installation would carry, not to a re-parse of the YAML.
				selections, answered := env.RunnableScopeSelections[runnablespkg.BindingKey(mod.Name, derived[i].Entry.Name)]
				row.Grant = runnablespkg.GrantOf(mod.Name, &derived[i], selections, answered)
				bound[row.Grant.Key] = true
			}
			listing.Runnables = append(listing.Runnables, row)
		}
	}
	if env != nil {
		answers := make([]string, 0, len(env.RunnableScopeSelections))
		for key := range env.RunnableScopeSelections {
			answers = append(answers, key)
		}
		sort.Strings(answers)
		listing.UnansweredKeys = runnablespkg.UnansweredKeys(answers, bound)
	}
	if !listRunnablesTools {
		return listing, nil
	}
	tools := make([]runnableRow, 0, len(listing.Runnables))
	for i := range listing.Runnables {
		if listing.Runnables[i].Tool != nil {
			tools = append(tools, listing.Runnables[i])
		}
	}
	listing.Runnables = tools
	return listing, nil
}

func writeRunnables(cmd *cobra.Command, listing *runnableListing, env *environments.Environment) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	// The tool columns appear only when a caller asked about tools or about an
	// environment: the default table is the one every existing reader parses.
	tools := listRunnablesTools || env != nil
	header := "MODULE\tNAME\tVERSION\tAGENT\tFACILITIES\tTIMEOUT\tSOURCE"
	if tools {
		header += "\tTOOL\tEFFECT"
	}
	if env != nil {
		header += "\tKEY\tGRANT"
	}
	fmt.Fprintln(w, header)
	for index := range listing.Runnables {
		e := &listing.Runnables[index]
		source := e.Source
		if source == "" {
			source = "declared"
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s",
			e.Module, e.Name, e.Version, e.Agent, strings.Join(e.Execution.Facilities, ","), e.Execution.Timeout, source)
		if tools {
			name, effect := "", ""
			if e.Tool != nil {
				name, effect = e.Tool.Name, e.Tool.Effect
			}
			row += "\t" + name + "\t" + effect
		}
		if env != nil {
			key, state := "", ""
			if e.Grant != nil {
				key, state = e.Grant.Key, e.Grant.State
			}
			row += "\t" + key + "\t" + state
		}
		fmt.Fprintln(w, row)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// A refused answer is printed under the table with its reason, because the
	// state alone ("invalid") does not say what to change.
	for index := range listing.Runnables {
		grant := listing.Runnables[index].Grant
		if grant != nil && grant.Reason != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s: %s", grant.Key, grant.Reason)
		}
	}
	if len(listing.UnansweredKeys) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "\n\nEnvironment %s answers keys no operation binds; generation refuses each by name:\n", env.Name)
		for _, key := range listing.UnansweredKeys {
			fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", key)
		}
	}
	return nil
}

func init() {
	RunnablesCmd.Flags().StringVar(&listRunnablesModule, "module", "", "Module to list runnables from")
	RunnablesCmd.Flags().BoolVar(&listRunnablesJSON, "json", false, "Emit machine-readable JSON")
	RunnablesCmd.Flags().StringVar(&listRunnablesEnv, "env", "", "Environment whose runnable-scope-selections to report as each operation's grant")
	RunnablesCmd.Flags().BoolVar(&listRunnablesTools, "tools", false, "List only the operations their owner exposes as a tool")
}
