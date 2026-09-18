package list

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var (
	listRunnablesModule string
	listRunnablesJSON   bool
)

// RunnablesCmd lists the runnables declared in the current workspace.
var RunnablesCmd = &cobra.Command{
	Use:   "runnables",
	Short: "List runnables across the workspace or within one module",
	Long: `List the runnables (typed finite operations) declared in the workspace.

A runnable is identified by module/name @ version: a version is an immutable
release and several versions of one name coexist.

Examples:
  codefly list runnables
  codefly list runnables --module=backend
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

	entries := make([]runnablespkg.Identity, 0)
	for _, mod := range modules {
		runnables, loadErr := mod.LoadRunnables(ctx)
		if loadErr != nil {
			return fmt.Errorf("cannot load runnables: %w", loadErr)
		}
		for _, r := range runnables {
			entries = append(entries, runnablespkg.NewIdentity(r))
		}
		// A module's derived operations are runnables it can be asked to
		// perform just as much as the ones it declares, so one listing
		// answers the question without the caller knowing which is which.
		derived, derivedErr := runnablespkg.LoadDerivedOperations(mod.Dir())
		if derivedErr != nil {
			return fmt.Errorf("cannot load the derived runnables of module %s: %w", mod.Name, derivedErr)
		}
		for i := range derived {
			entries = append(entries, derived[i].Identity())
		}
	}

	if listRunnablesJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
	}

	if len(entries) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No runnables in workspace <%s>\n", workspace.Name)
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODULE\tNAME\tVERSION\tAGENT\tFACILITIES\tTIMEOUT\tSOURCE")
	for _, e := range entries {
		source := e.Source
		if source == "" {
			source = "declared"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Module, e.Name, e.Version, e.Agent, strings.Join(e.Execution.Facilities, ","), e.Execution.Timeout, source)
	}
	return w.Flush()
}

func init() {
	RunnablesCmd.Flags().StringVar(&listRunnablesModule, "module", "", "Module to list runnables from")
	RunnablesCmd.Flags().BoolVar(&listRunnablesJSON, "json", false, "Emit machine-readable JSON")
}
