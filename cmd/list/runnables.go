package list

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/codefly-dev/cli/cmd/common"
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

type runnableListEntry struct {
	Module     string   `json:"module"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Agent      string   `json:"agent"`
	Facilities []string `json:"facilities"`
	Timeout    string   `json:"timeout"`
}

func listRunnables(cmd *cobra.Command) error {
	ctx, done := common.NewContext()
	defer done()

	workspace, err := common.LoadWorkspace(ctx)
	if err != nil {
		return fmt.Errorf("cannot load workspace: %w", err)
	}

	var runnables []*resources.Runnable
	if listRunnablesModule != "" {
		var mod *resources.Module
		mod, err = workspace.LoadModuleFromName(ctx, listRunnablesModule)
		if err != nil {
			return fmt.Errorf("module not found: %w", err)
		}
		runnables, err = mod.LoadRunnables(ctx)
	} else {
		runnables, err = workspace.LoadAllRunnables(ctx)
	}
	if err != nil {
		return fmt.Errorf("cannot load runnables: %w", err)
	}

	entries := make([]runnableListEntry, 0, len(runnables))
	for _, r := range runnables {
		entry := runnableListEntry{
			Module:  r.Module(),
			Name:    r.Name,
			Version: r.Version,
			Agent:   r.Agent.Identifier(),
		}
		for _, facility := range r.Execution.Facilities {
			entry.Facilities = append(entry.Facilities, string(facility))
		}
		entry.Timeout = r.Execution.Timeout
		entries = append(entries, entry)
	}

	if listRunnablesJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(entries)
	}

	if len(entries) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No runnables in workspace <%s>\n", workspace.Name)
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODULE\tNAME\tVERSION\tAGENT\tFACILITIES\tTIMEOUT")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Module, e.Name, e.Version, e.Agent, strings.Join(e.Facilities, ","), e.Timeout)
	}
	return w.Flush()
}

func init() {
	RunnablesCmd.Flags().StringVar(&listRunnablesModule, "module", "", "Module to list runnables from")
	RunnablesCmd.Flags().BoolVar(&listRunnablesJSON, "json", false, "Emit machine-readable JSON")
}
