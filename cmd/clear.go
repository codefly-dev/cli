package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/wool"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

var (
	clearKeepProcesses  bool
	clearKeepContainers bool
	clearDryRun         bool
	clearAllWorkspaces  bool
	stopAllWorkspaces   bool
)

type clearOptions struct {
	verb           string
	keepProcesses  bool
	keepContainers bool
	dryRun         bool
	// scope bounds which workspace's processes may be signalled. The zero value
	// is machine-wide, so every construction site states what it means.
	scope processgroup.Scope
}

func clearCommandOptions() clearOptions {
	return clearOptions{
		verb:           "clear",
		keepProcesses:  clearKeepProcesses,
		keepContainers: clearKeepContainers,
		dryRun:         clearDryRun,
		scope:          workspaceScope(clearAllWorkspaces),
	}
}

// workspaceScope is the workspace a command acts on or reports: what a stop or
// clear may touch, and what a ps lists. Without --all it is the workspace the
// command was run in, so a machine running several workspaces at once — one per
// checkout, which is the normal shape for anyone working on more than one
// branch — never has one workspace's stop kill another's services, and never
// has one workspace's listing describe another's run.
//
// Outside any workspace there is nothing to scope to, so the scope is
// machine-wide: a bare `stop` in a home directory still behaves as it always
// did. The caller announces which of the two it got, because the difference
// decides what survives.
func workspaceScope(all bool) processgroup.Scope {
	if all {
		return processgroup.AllWorkspaces()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return processgroup.AllWorkspaces()
	}
	workspace, ok := processgroup.CurrentWorkspace(cwd)
	if !ok {
		return processgroup.AllWorkspaces()
	}
	return processgroup.InWorkspace(workspace)
}

// ClearCmd removes codefly state (running processes + docker containers).
// Without arguments: removes EVERYTHING codefly-owned.
// With positional args (e.g. `codefly clear neo4j`): removes only
// containers whose name contains any of the substrings — useful when
// you want to recycle one piece of infra without nuking the rest.
var ClearCmd = &cobra.Command{
	Use:   "clear [name-filter...]",
	Short: "Remove Codefly processes, containers, and stale local runtime state",
	Long: `Clear codefly state.

Without arguments, removes ALL codefly processes and ALL codefly-owned
docker containers. Pass one or more substring filters to scope the
container removal — only containers whose name contains at least one
filter are removed. The process kill is always wholesale because
codefly processes aren't per-service.

Examples:
  codefly clear                     # full reset
  codefly clear neo4j               # only neo4j container (keeps postgres)
  codefly clear neo4j postgres      # both
  codefly clear --keep-processes neo4j  # only the container, leave running codefly alone
  codefly clear --dry-run           # list what would be removed without doing it`,
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		return clearCommand(ctx, args, clearCommandOptions())
	},
}

func init() {
	ClearCmd.Flags().BoolVar(&clearKeepProcesses, "keep-processes", false, "Don't kill running codefly processes (only remove containers)")
	ClearCmd.Flags().BoolVar(&clearKeepContainers, "keep-containers", false, "Don't remove docker containers (only kill processes)")
	ClearCmd.Flags().BoolVar(&clearDryRun, "dry-run", false, "List what would be removed without removing anything")
	ClearCmd.Flags().BoolVar(&clearAllWorkspaces, "all", false, "Act on every workspace on this machine, not just the current one")
}

func clearCommand(ctx context.Context, args []string, options clearOptions) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	var failures []error

	w := wool.Get(ctx).In(options.verb)

	// Always announce what we're doing — `clear` was silent on the common path
	// (no docker containers + a quiet process kill), so it looked like a no-op.
	switch {
	case options.dryRun:
		w.Info("dry-run: nothing will be removed")
	case len(args) > 0:
		w.Info("clearing scoped containers", wool.Field("filter", args))
	case options.keepContainers:
		w.Info("reaping processes and orphaned groups (containers kept for reuse)")
	default:
		w.Info("full reset")
	}

	if options.keepProcesses {
		w.Info("keeping processes (--keep-processes)")
	}

	managedFailures := sweepStaleState(ctx, w, options)
	failures = append(failures, managedFailures...)
	// The fallback scans must not turn a failed registry authentication into
	// permission to signal the same group by its executable signature.
	if len(managedFailures) == 0 {
		failures = append(failures, reapOrphanedDevServers(ctx, w, options)...)
		failures = append(failures, reapOrphanedNativeServices(ctx, w, options)...)
	}

	if options.keepContainers {
		return errors.Join(failures...)
	}

	dockerCLI, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		w.Info("docker unavailable, skipping container removal (nix-run services are not docker containers)", wool.ErrField(err))
		clearNixDataNote(w)
		return errors.Join(failures...)
	}
	defer func() {
		if err = dockerCLI.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close docker client: %w", err))
		}
	}()
	cos, err := dockerCLI.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		w.Info("cannot list containers, docker may be down; skipping", wool.ErrField(err))
		clearNixDataNote(w)
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
		}
		return errors.Join(failures...)
	}

	removed, removeFailures := removeCodeflyContainers(ctx, w, dockerCLI, cos, args, options.dryRun)
	failures = append(failures, removeFailures...)
	switch {
	case removed == 0 && len(args) > 0:
		w.Info("no containers matched filter", wool.Field("filter", args))
	case removed == 0:
		w.Info("no codefly containers found")
	case options.dryRun:
		w.Info("containers would be removed", wool.Field("count", removed))
	default:
		w.Info("removed containers", wool.Field("count", removed))
	}
	clearNixDataNote(w)
	return errors.Join(failures...)
}

// sweepStaleState removes the state crashed or exited CLIs left behind —
// per-spawn UDS sockets under /tmp/codefly-uds and process-group tracking
// files under ~/.codefly/runs — or counts it on a dry run.
func sweepStaleState(ctx context.Context, w *wool.Wool, options clearOptions) []error {
	if options.dryRun {
		if !options.keepProcesses {
			groups, err := processgroup.PreviewManagedProcessGroups(ctx, options.scope)
			if err != nil {
				return []error{err}
			}
			w.Info("would stop registered process groups", wool.Field("count", len(groups)), wool.Field("pgids", groups))
		}
		if n := manager.CountStaleAgentSockets(); n > 0 {
			w.Info("would remove stale agent sockets", wool.Field("count", n))
		} else {
			w.Info("no stale agent sockets")
		}
		return nil
	}
	if n := manager.SweepStaleAgentSockets(); n > 0 {
		w.Info("removed stale agent sockets", wool.Field("count", n))
	} else {
		w.Info("no stale agent sockets")
	}
	var processEvidence processgroup.CleanupEvidence
	var processErr error
	if options.keepProcesses {
		processEvidence, processErr = processgroup.ReapStaleProcessGroupsWithEvidence(ctx, options.scope)
	} else {
		processEvidence, processErr = processgroup.StopManagedProcessGroups(ctx, options.scope)
	}
	var failures []error
	if processErr != nil {
		w.Warn("cannot reap stale process groups", wool.ErrField(processErr))
		failures = append(failures, fmt.Errorf("reap stale process groups: %w", processErr))
	} else if recovered := processEvidence.RecoveredPGIDs(); len(recovered) > 0 {
		w.Info("reaped managed process groups", wool.Field("count", len(recovered)), wool.Field("pgids", recovered))
	} else {
		w.Info("managed process groups reconciled")
	}
	// Say what was deliberately left running. Silence here reads as "nothing
	// was up", which is the opposite of the truth and the reason someone
	// reaches for --all without knowing what it will take with it.
	if left := processEvidence.OutOfScope; len(left) > 0 {
		w.Info("left other workspaces' process groups running", wool.Field("count", len(left)), wool.Field("pgids", left), wool.Field("hint", "--all to include them"))
	}
	return failures
}

// containerMatchesFilter: with no filter everything codefly-owned matches;
// with one, the name must contain at least one of its words.
func containerMatchesFilter(name string, args []string) bool {
	if len(args) == 0 {
		return true
	}
	for _, arg := range args {
		if strings.Contains(name, arg) {
			return true
		}
	}
	return false
}

// removeCodeflyContainers removes every codefly-owned container the filter
// matches, or counts them on a dry run.
func removeCodeflyContainers(ctx context.Context, w *wool.Wool, dockerCLI *client.Client, containers []container.Summary, args []string, dryRun bool) (int, []error) {
	removed := 0
	var failures []error
	for index := range containers {
		c := &containers[index]
		if len(c.Names) == 0 {
			continue
		}
		name := c.Names[0]
		if !strings.HasPrefix(name, "/codefly") || !containerMatchesFilter(name, args) {
			continue
		}
		if dryRun {
			w.Info("would remove container", wool.Field("container", strings.TrimPrefix(name, "/")), wool.Field("state", c.State))
			removed++
			continue
		}
		w.Info("removing container", wool.Field("container", strings.TrimPrefix(name, "/")))
		// ContainerKill can error when container is already stopped;
		// the Remove --force below handles cleanup either way, so
		// a kill error here is noise, not fatal.
		_ = dockerCLI.ContainerKill(ctx, c.ID, "SIGKILL")
		if err := dockerCLI.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			// Don't abort the whole sweep on one stubborn container — warn and
			// keep going so the rest still get cleaned (was log.Fatalf).
			w.Warn("cannot remove container", wool.Field("container", strings.TrimPrefix(name, "/")), wool.ErrField(err))
			failures = append(failures, fmt.Errorf("remove container %s: %w", strings.TrimPrefix(name, "/"), err))
			continue
		}
		removed++
	}
	return removed, failures
}

// clearNixDataNote explains the one thing `clear` deliberately does NOT touch:
// nix-run service DATA (Postgres/Neo4j data dirs). `clear` removes processes +
// docker containers, but nix services keep their data under ~/.codefly/data, so a
// wedged DB (e.g. a dirty golang-migrate state) survives `clear`. This note makes
// that explicit + gives the reset command.
func clearNixDataNote(w *wool.Wool) {
	w.Info("nix-run service DATA is NOT removed by clear (only processes + docker containers).\n" +
		"      to reset a service's data — e.g. a dirty/wedged DB migration — stop codefly and run:\n" +
		"        rm -rf ~/.codefly/data/<workspace>    (e.g. ~/.codefly/data/mind-server)\n" +
		"      then `codefly run service <svc>` re-applies migrations from scratch.")
}

// reapOrphanedDevServers reaps codefly-owned dev servers (next dev / npm run dev)
// that escaped the process-group registry: found by signature, proven codefly's
// by their process-group authentication, and reaped only when their group has
// lost its supervisor. The registry reap cannot see these; it only knows records.
func reapOrphanedDevServers(ctx context.Context, w *wool.Wool, options clearOptions) []error {
	if options.keepProcesses {
		return nil
	}
	reaped, err := processgroup.ReapDevServerOrphans(ctx, options.dryRun, options.scope)
	if err != nil {
		w.Warn("cannot reap orphaned dev servers", wool.ErrField(err))
	}
	switch {
	case len(reaped) > 0 && options.dryRun:
		w.Info("would reap orphaned dev servers", wool.Field("count", len(reaped)), wool.Field("pgids", devServerPGIDs(reaped)))
	case len(reaped) > 0:
		w.Info("reaped orphaned dev servers", wool.Field("count", len(reaped)), wool.Field("pgids", devServerPGIDs(reaped)))
	case err == nil:
		w.Info("no orphaned dev servers")
	}
	if err != nil {
		return []error{fmt.Errorf("reap orphaned dev servers: %w", err)}
	}
	return nil
}

func devServerPGIDs(orphans []processgroup.DevServerOrphan) []int {
	pgids := make([]int, 0, len(orphans))
	for i := range orphans {
		pgids = append(pgids, orphans[i].PGID)
	}
	return pgids
}

// reapOrphanedNativeServices reaps codefly-owned native-mode service processes —
// compiled user binaries under a build cache and the postgres cluster — that
// survived their supervisor and keep squatting on their deterministic ports. Like
// the dev-server reap, it only signals groups proven codefly's by their
// process-group authentication and stale enough to have lost their supervisor.
func reapOrphanedNativeServices(ctx context.Context, w *wool.Wool, options clearOptions) []error {
	if options.keepProcesses {
		return nil
	}
	reaped, err := processgroup.ReapNativeServiceOrphans(ctx, options.dryRun, options.scope)
	if err != nil {
		w.Warn("cannot reap orphaned native services", wool.ErrField(err))
	}
	switch {
	case len(reaped) > 0 && options.dryRun:
		w.Info("would reap orphaned native services", wool.Field("count", len(reaped)), wool.Field("pgids", nativeServicePGIDs(reaped)))
	case len(reaped) > 0:
		w.Info("reaped orphaned native services", wool.Field("count", len(reaped)), wool.Field("pgids", nativeServicePGIDs(reaped)))
	case err == nil:
		w.Info("no orphaned native services")
	}
	if err != nil {
		return []error{fmt.Errorf("reap orphaned native services: %w", err)}
	}
	return nil
}

func nativeServicePGIDs(orphans []processgroup.NativeServiceOrphan) []int {
	pgids := make([]int, 0, len(orphans))
	for i := range orphans {
		pgids = append(pgids, orphans[i].PGID)
	}
	return pgids
}
