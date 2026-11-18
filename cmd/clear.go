package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/runners/runstate"
	"github.com/codefly-dev/core/wool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
)

var (
	clearKeepProcesses  bool
	clearKeepContainers bool
	clearDryRun         bool
	clearAllWorkspaces  bool
	stopAllWorkspaces   bool
	clearStates         []string
	clearOlderThan      string
)

type clearOptions struct {
	verb           string
	keepProcesses  bool
	keepContainers bool
	dryRun         bool
	// scope bounds which workspace's processes may be signalled. The zero value
	// is machine-wide, so every construction site states what it means.
	scope processgroup.Scope
	// states and olderThan select the persistent run state a person asked to
	// remove (clear_persistent_state.go). Either makes clear do only that.
	states    []string
	olderThan time.Duration
}

func clearCommandOptions() (clearOptions, error) {
	options := clearOptions{
		verb:           "clear",
		keepProcesses:  clearKeepProcesses,
		keepContainers: clearKeepContainers,
		dryRun:         clearDryRun,
		scope:          workspaceScope(clearAllWorkspaces),
		states:         clearStates,
	}
	if clearOlderThan != "" {
		age, err := parseStateAge(clearOlderThan)
		if err != nil {
			return options, err
		}
		options.olderThan = age
	}
	return options, nil
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

// ClearCmd removes codefly state (running processes, docker containers, and
// the native state directories of disposable runs that are no longer alive).
// Without arguments: removes everything codefly-owned in the workspace's scope.
// With positional args (e.g. `codefly clear neo4j`): removes only
// containers whose name contains any of the substrings — useful when
// you want to recycle one piece of infra without nuking the rest.
var ClearCmd = &cobra.Command{
	Use:   "clear [name-filter...]",
	Short: "Remove Codefly processes, containers, and stale local runtime state",
	Long: `Clear codefly state.

Without arguments, removes the codefly processes and the codefly-owned
docker containers of the workspace it is run in: a container is the
workspace's when a run of that workspace created it. Other workspaces'
containers, and codefly containers no workspace can be told for, are left
and named; --all (or a clear run outside any workspace) acts on every
workspace on this machine. Pass one or more substring filters to narrow
the container removal further — only containers whose name contains at
least one filter are removed. The process kill is always wholesale within
the scope because codefly processes aren't per-service.

It also removes the native state directories (a database run as a host
process keeps its data under the user cache directory) of disposable runs
that are no longer alive: runs with --temporary-ports and test-owned
dependency stacks. A run that was killed leaves its database servers
running, and they keep its directory in use: unless --keep-processes, those
servers are stopped first (only the processes the run's agent recorded in
that directory, and only when no live run claims it). A developer's
persistent database is never removed, and its server never stopped.

Persistent runs keep their native state for good: every naming scope ever
run leaves a directory. --dry-run lists the workspace's, each with the run
it belongs to, how long ago a run last claimed it and its size. Nothing
removes one but a person: --state <directory> removes the directory named
(repeatable), and --older-than <age> removes every one of the scope that no
run has claimed for that long (72h, 30d). A directory still in use, one of
another workspace (without --all) and one no run is recorded on are refused.
With either flag, clear does only this: no process is stopped and no
container removed.

Examples:
  codefly clear                     # full reset of this workspace
  codefly clear --all               # full reset of every workspace on this machine
  codefly clear neo4j               # only neo4j container (keeps postgres)
  codefly clear neo4j postgres      # both
  codefly clear --keep-processes neo4j  # only the container, leave running codefly alone
  codefly clear --dry-run           # list what would be removed without doing it
  codefly clear --dry-run --older-than 30d   # the persistent run state that would go
  codefly clear --older-than 30d    # remove persistent run state unused for 30 days
  codefly clear --state <directory> # remove one persistent run's state, as --dry-run lists it`,
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		options, err := clearCommandOptions()
		if err != nil {
			return err
		}
		return clearCommand(ctx, args, options)
	},
}

func init() {
	ClearCmd.Flags().BoolVar(&clearKeepProcesses, "keep-processes", false, "Don't kill running codefly processes (only remove containers)")
	ClearCmd.Flags().BoolVar(&clearKeepContainers, "keep-containers", false, "Don't remove docker containers (only kill processes)")
	ClearCmd.Flags().BoolVar(&clearDryRun, "dry-run", false, "List what would be removed without removing anything")
	ClearCmd.Flags().BoolVar(&clearAllWorkspaces, "all", false, "Act on every workspace on this machine, not just the current one")
	ClearCmd.Flags().StringArrayVar(&clearStates, "state", nil, "Remove this persistent run state directory, as --dry-run lists it (repeatable); clear then does only this")
	ClearCmd.Flags().StringVar(&clearOlderThan, "older-than", "", "Remove the scope's persistent run state that no run has claimed for this long (72h, 30d); clear then does only this")
}

func clearCommand(ctx context.Context, args []string, options clearOptions) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	var failures []error

	w := wool.Get(ctx).In(options.verb)

	// Removing a persistent run's state is its own deliberate act: it stops
	// no process and removes no container.
	if options.prunesRunState() {
		root, err := runstate.Root()
		if err != nil {
			return fmt.Errorf("locate native run state: %w", err)
		}
		return errors.Join(pruneRunState(w, root, options)...)
	}

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
	// After the reaps: a directory is in use for as long as a server started in
	// it runs, and the reaps are what stop a killed run's servers.
	if root, err := runstate.Root(); err != nil {
		w.Warn("cannot locate native run state", wool.ErrField(err))
	} else {
		failures = append(failures, sweepNativeRunState(w, root, options)...)
		failures = append(failures, reportPersistentRunState(w, root, options)...)
	}

	if options.keepContainers {
		return errors.Join(failures...)
	}

	dockerCLI, err := client.New(client.FromEnv)
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
	listed, err := dockerCLI.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		w.Info("cannot list containers, docker may be down; skipping", wool.ErrField(err))
		clearNixDataNote(w)
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
		}
		return errors.Join(failures...)
	}

	namespace, err := clearContainerNamespace(options)
	if err != nil {
		w.Warn("cannot tell this workspace's containers from another's; none is removed", wool.ErrField(err), wool.Field("hint", "--all removes every codefly container on this machine"))
		failures = append(failures, fmt.Errorf("scope container removal to the workspace: %w", err))
		clearNixDataNote(w)
		return errors.Join(failures...)
	}
	selected, left := clearableContainers(listed.Items, args, namespace)
	removed, removeFailures := removeCodeflyContainers(ctx, w, dockerCLI, selected, options.dryRun)
	failures = append(failures, removeFailures...)
	// Say what was deliberately kept, as for process groups: silence reads as
	// "nothing else was up".
	if len(left) > 0 {
		names := make([]string, 0, len(left))
		for index := range left {
			names = append(names, strings.TrimPrefix(left[index].Names[0], "/"))
		}
		w.Info("left containers that are not this workspace's", wool.Field("count", len(left)), wool.Field("containers", names), wool.Field("hint", "--all to include them"))
	}
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
		var failures []error
		if !options.keepProcesses {
			// A group the preview cannot decide is named in err and kept; the
			// rest are still listed, and the dry run goes on to count sockets.
			groups, err := processgroup.PreviewManagedProcessGroups(ctx, options.scope)
			w.Info("would stop registered process groups", wool.Field("count", len(groups)), wool.Field("pgids", groups))
			if err != nil {
				w.Warn("would keep process groups whose ownership cannot be decided", wool.ErrField(err))
				failures = append(failures, err)
			}
		}
		if n := manager.CountStaleAgentSockets(); n > 0 {
			w.Info("would remove stale agent sockets", wool.Field("count", n))
		} else {
			w.Info("no stale agent sockets")
		}
		return failures
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

// sweepNativeRunState removes the native state directories under root that a
// disposable run left behind and nothing uses any more, or counts them on a
// dry run. An orderly run removes its own when it ends; this is for the runs
// that were killed. Which directory is disposable, and whether it is still in
// use, is recorded on it by the agent that owns it (core's runners/runstate):
// a developer's persistent database and a directory no agent recorded are
// never removed.
//
// A killed run leaves its servers running as well, and they keep its directory
// in use for good. Unless --keep-processes, the sweep stops them first: the
// processes an agent recorded in a disposable directory no live run claims any
// more, each checked against its recorded birth before it is signalled (core's
// runstate.StopAbandoned).
func sweepNativeRunState(w *wool.Wool, root string, options clearOptions) []error {
	sweep := runstate.SweepOptions{
		Include: func(run runstate.Run) bool { return options.scope.Includes(run.Workspace) },
		DryRun:  options.dryRun,
	}
	if !options.keepProcesses {
		sweep.StopAbandoned = &runstate.StopOptions{}
	}
	swept, err := runstate.Sweep(root, sweep)
	directories := make([]string, 0, len(swept))
	var servers []int
	for _, state := range swept {
		directories = append(directories, state.Dir)
		servers = append(servers, state.Stopped...)
	}
	switch {
	case len(servers) > 0 && options.dryRun:
		w.Info("would stop servers a killed disposable run left running", wool.Field("count", len(servers)), wool.Field("pids", servers))
	case len(servers) > 0:
		w.Info("stopped servers a killed disposable run left running", wool.Field("count", len(servers)), wool.Field("pids", servers))
	}
	switch {
	case len(swept) > 0 && options.dryRun:
		w.Info("would remove native state of ended disposable runs", wool.Field("count", len(swept)), wool.Field("directories", directories))
	case len(swept) > 0:
		w.Info("removed native state of ended disposable runs", wool.Field("count", len(swept)), wool.Field("directories", directories))
	case err == nil:
		w.Info("no native state of ended disposable runs")
	}
	if err != nil {
		w.Warn("cannot remove native run state", wool.ErrField(err))
		return []error{fmt.Errorf("remove native run state: %w", err)}
	}
	return nil
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

// clearContainerNamespace is the recovery namespace a clear removes containers
// of: its workspace's own, or none for the machine-wide scope (--all, or a
// clear run outside any workspace).
//
// A scoped clear on a host with no durable identity has no namespace to match
// and cannot attribute any container: it is refused, naming --all, rather than
// read as machine-wide.
func clearContainerNamespace(options clearOptions) (string, error) {
	if options.scope.All() {
		return "", nil
	}
	namespace, err := orchestration.ContainerRecoveryNamespaceOf(options.scope.Workspace)
	if err != nil {
		return "", err
	}
	if namespace == "" {
		return "", errors.New("this host has no durable identity, so no container is labelled with a workspace")
	}
	return namespace, nil
}

// clearableContainers splits the codefly-owned containers the filter matches
// into those this clear may remove and those it leaves because they belong to
// another workspace. A container is a workspace's when it carries that
// workspace's recovery namespace (the label core stamps on every container a
// run creates). namespace is empty for a machine-wide clear, which takes them
// all; a scoped clear leaves a container it cannot attribute, as a scoped stop
// leaves a process group it cannot attribute.
func clearableContainers(containers []container.Summary, args []string, namespace string) (selected, left []container.Summary) {
	for index := range containers {
		c := containers[index]
		if len(c.Names) == 0 {
			continue
		}
		name := c.Names[0]
		if !strings.HasPrefix(name, "/codefly") || !containerMatchesFilter(name, args) {
			continue
		}
		if namespace != "" && c.Labels[dockerrun.LabelCodeflyRecoveryNamespace] != namespace {
			left = append(left, c)
			continue
		}
		selected = append(selected, c)
	}
	return selected, left
}

// removeCodeflyContainers removes the containers given, or counts them on a
// dry run.
func removeCodeflyContainers(ctx context.Context, w *wool.Wool, dockerCLI *client.Client, containers []container.Summary, dryRun bool) (int, []error) {
	removed := 0
	var failures []error
	for index := range containers {
		c := &containers[index]
		name := c.Names[0]
		if dryRun {
			w.Info("would remove container", wool.Field("container", strings.TrimPrefix(name, "/")), wool.Field("state", c.State))
			removed++
			continue
		}
		w.Info("removing container", wool.Field("container", strings.TrimPrefix(name, "/")))
		// ContainerKill can error when container is already stopped;
		// the Remove --force below handles cleanup either way, so
		// a kill error here is noise, not fatal.
		_, _ = dockerCLI.ContainerKill(ctx, c.ID, client.ContainerKillOptions{Signal: "SIGKILL"})
		if _, err := dockerCLI.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
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
// PERSISTENT nix-run service data (Postgres/Neo4j data dirs). `clear` removes
// processes, docker containers and the native state of ended disposable runs
// (sweepNativeRunState), but a developer's own database is kept, so a wedged DB
// (e.g. a dirty golang-migrate state) survives `clear`. This note makes that
// explicit + gives the reset command.
func clearNixDataNote(w *wool.Wool) {
	w.Info("persistent nix-run service DATA is NOT removed by clear (only processes, docker containers and the state of ended disposable runs).\n" +
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
