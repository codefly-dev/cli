package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/spf13/cobra"
)

var (
	psJSON          bool
	psAllWorkspaces bool
)

// psProcess is one row of `codefly ps`: a process a codefly run is keeping
// alive, whichever kind it is. The kinds are found by different signatures —
// a dev server by its command line, a native service by its executable — but
// they are one list to the person asking what their run is still holding, so
// they share one shape here rather than leaking the discovery path into the
// output.
type psProcess struct {
	// Kind is "dev-server" or "native": what the scan matched on.
	Kind    string `json:"kind"`
	PID     int    `json:"pid"`
	PGID    int    `json:"pgid"`
	Parent  int    `json:"parent"`
	Status  string `json:"status"`
	Command string `json:"command"`
	Cwd     string `json:"cwd"`
	// Workspace is the run this process belongs to, which for a composed
	// module's service is not the directory it runs in.
	Workspace string    `json:"workspace"`
	Started   time.Time `json:"started"`
}

// PsCmd lists what a codefly run is still holding: its frontend dev servers and
// its native-mode service processes — the compiled service binaries and the
// stateful stores it started.
//
// It lists a run COMPLETELY, which it did not. Two gaps, one cause: it scanned
// only dev servers, so a run's compiled services and stores never appeared at
// all; and a dev server was only found when its working directory sat inside a
// workspace, which is never true of a composed module's frontend — that runs
// from the module's own checkout, outside the workspace that composed it. A
// composed run therefore showed a fraction of itself, and `stop` then left
// exactly the processes `ps` could not name still holding their ports.
//
// It is scoped to the current workspace, so it answers "what is my run still
// holding" — the same set `codefly stop` acts on. `--all` widens it to every
// workspace on the machine, which is how to find a leak belonging to a checkout
// you are not standing in.
var PsCmd = &cobra.Command{
	Use:   "ps",
	Short: "List the processes this workspace's run is still holding",
	Long: `List the processes a codefly run is keeping alive: frontend dev servers
(next dev / npm run dev / vite) and native-mode services (compiled service
binaries and the stateful stores a run started), including those belonging to
composed modules checked out outside the workspace.

Scoped to the current workspace — the same set 'codefly stop' acts on. Use
--all for every workspace on this machine. Outside a workspace the listing is
machine-wide, since there is nothing to scope to.

STATUS is one of: orphaned (codefly's, escaped its supervisor — reaped by
'codefly clear'), tracked (codefly's, still supervised), or external (not
codefly's — shown for visibility, never reaped).`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		processes, err := scanRunProcesses(ctx, workspaceScope(psAllWorkspaces))
		if err != nil {
			return err
		}
		if psJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(processes)
		}
		if len(processes) == 0 {
			if psAllWorkspaces {
				cmd.Println("no codefly processes running")
			} else {
				cmd.Println("no codefly processes running in this workspace (--all for every workspace)")
			}
			return nil
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PID\tPGID\tKIND\tSTATUS\tAGE\tWORKSPACE\tCWD\tCOMMAND")
		for i := range processes {
			p := &processes[i]
			fmt.Fprintf(tw, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				p.PID, p.PGID, p.Kind, p.Status, age(p.Started), p.Workspace, p.Cwd, p.Command)
		}
		return tw.Flush()
	},
}

// scanRunProcesses gathers both scans into one list, keeps what the scope
// admits, and orders it by pid so repeated runs of the command are comparable.
// The result is never nil, so --json prints an empty array rather than "null".
func scanRunProcesses(ctx context.Context, scope processgroup.Scope) ([]psProcess, error) {
	devServers, err := processgroup.ScanDevServerOrphans(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan dev servers: %w", err)
	}
	natives, err := processgroup.ScanNativeServiceOrphans(ctx)
	if err != nil {
		return nil, fmt.Errorf("scan native services: %w", err)
	}
	processes := make([]psProcess, 0, len(devServers)+len(natives))
	for i := range devServers {
		server := &devServers[i]
		if !scope.Includes(server.Workspace) {
			continue
		}
		processes = append(processes, psProcess{
			Kind:      "dev-server",
			PID:       server.PID,
			PGID:      server.PGID,
			Parent:    server.Parent,
			Status:    processStatus(server.Owned, server.Orphaned),
			Command:   server.Command,
			Cwd:       server.Cwd,
			Workspace: server.Workspace,
			Started:   server.Started,
		})
	}
	for i := range natives {
		native := &natives[i]
		if !scope.Includes(native.Workspace) {
			continue
		}
		processes = append(processes, psProcess{
			Kind:      "native",
			PID:       native.PID,
			PGID:      native.PGID,
			Parent:    native.Parent,
			Status:    processStatus(native.Owned, native.Orphaned),
			Command:   native.Command,
			Cwd:       native.Cwd,
			Workspace: native.Workspace,
			Started:   native.Started,
		})
	}
	sort.Slice(processes, func(i, j int) bool { return processes[i].PID < processes[j].PID })
	return processes, nil
}

// processStatus labels a scanned process: "external" when it is not codefly's
// (never reaped by clear), "orphaned" when it is codefly's but has been
// reparented away from its supervisor (a reap candidate), and "tracked" when it
// is codefly's and still supervised.
func processStatus(owned, orphaned bool) string {
	switch {
	case !owned:
		return "external"
	case orphaned:
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
	PsCmd.Flags().BoolVar(&psJSON, "json", false, "Print the processes as JSON")
	PsCmd.Flags().BoolVar(&psAllWorkspaces, "all", false, "List every workspace's processes on this machine, not just the current one")
}
