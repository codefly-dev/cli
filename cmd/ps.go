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
	Short: "List dev servers running in codefly workspaces on this machine",
	Long: `List frontend dev servers (next dev / npm run dev / vite) running inside a
codefly workspace, machine-wide. STATUS is one of: orphaned (codefly's, escaped
its supervisor — reaped by 'codefly clear'), tracked (codefly's, still
supervised), or external (not codefly's — shown for visibility, never reaped).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		orphans, err := processgroup.ScanDevServerOrphans(ctx)
		if err != nil {
			return fmt.Errorf("scan dev servers: %w", err)
		}
		if psJSON {
			if orphans == nil {
				orphans = []processgroup.DevServerOrphan{}
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(orphans)
		}
		if len(orphans) == 0 {
			cmd.Println("no codefly dev servers running")
			return nil
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PID\tPGID\tSTATUS\tAGE\tCWD\tCOMMAND")
		for i := range orphans {
			o := &orphans[i]
			fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\n", o.PID, o.PGID, devServerStatus(o), age(o.Started), o.Cwd, o.Command)
		}
		return tw.Flush()
	},
}

// devServerStatus labels a scanned dev server: "external" when it is not
// codefly's (never reaped by clear), "orphaned" when it is codefly's but has
// been reparented away from its supervisor (a reap candidate), and "tracked"
// when it is codefly's and still supervised.
func devServerStatus(o *processgroup.DevServerOrphan) string {
	switch {
	case !o.Owned:
		return "external"
	case o.Orphaned:
		return "orphaned"
	default:
		return "tracked"
	}
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
