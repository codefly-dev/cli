package cmd

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/spf13/cobra"
)

var psJSON bool

// PsCmd lists codefly-spawned dev servers running anywhere on the machine,
// independent of the current working directory. Unlike `list jobs`, it does not
// need a workspace — its purpose is to surface leaks (orphaned `next dev` /
// `npm run dev` servers) that `codefly run` lost track of.
var PsCmd = &cobra.Command{
	Use:   "ps",
	Short: "List codefly dev servers running on this machine",
	Long: `List frontend dev servers (next dev / npm run dev / vite) running inside a
codefly workspace, machine-wide. Servers marked ORPHANED have been reparented
away from codefly and can be reaped with 'codefly clear'.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		orphans, err := processgroup.ScanDevServerOrphans(ctx)
		if err != nil {
			return fmt.Errorf("scan dev servers: %w", err)
		}
		if psJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(orphans)
		}
		if len(orphans) == 0 {
			cmd.Println("no codefly dev servers running")
			return nil
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PID\tPGID\tSTATUS\tAGE\tCWD\tCOMMAND")
		for _, o := range orphans {
			status := "tracked"
			if o.Orphaned {
				status = "orphaned"
			}
			fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n", o.PID, o.PGID, status, age(o.Started), o.Cwd, o.Command)
		}
		return tw.Flush()
	},
}

func age(started time.Time) string {
	if started.IsZero() {
		return "?"
	}
	return time.Since(started).Round(time.Second).String()
}

func init() {
	PsCmd.Flags().BoolVar(&psJSON, "json", false, "Print the dev servers as JSON")
}
