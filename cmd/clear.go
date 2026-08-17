package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/processgroup"
	"github.com/codefly-dev/core/agents/manager"
	postgresipc "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/wool"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

// codeflyOwnedPIDs returns the PIDs of running codefly-owned processes: the
// `codefly` CLI binary itself and any agent binary under ~/.codefly/agents/.
// It matches on the EXECUTABLE path, never on a substring of the full command
// line, so unrelated processes that merely mention the repo path are spared.
// The caller's own PID (self) is always excluded.
func codeflyOwnedPIDs(ctx context.Context, self int) ([]int, error) {
	// `command=` prints the full argv; the first token is the executable.
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	return parseCodeflyOwnedPIDs(out, self), nil
}

func parseCodeflyOwnedPIDs(out []byte, self int) []int {
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, convErr := strconv.Atoi(fields[0])
		if convErr != nil || pid == self {
			continue
		}
		exePath := fields[1]
		owned := filepath.Base(exePath) == "codefly" || strings.Contains(exePath, "/.codefly/agents/")
		if owned {
			pids = append(pids, pid)
		}
	}
	return pids
}

var (
	clearKeepProcesses  bool
	clearKeepContainers bool
	clearDryRun         bool
)

type clearOptions struct {
	verb           string
	keepProcesses  bool
	keepContainers bool
	dryRun         bool
}

func clearCommandOptions() clearOptions {
	return clearOptions{
		verb:           "clear",
		keepProcesses:  clearKeepProcesses,
		keepContainers: clearKeepContainers,
		dryRun:         clearDryRun,
	}
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
	RunE: func(cmd *cobra.Command, args []string) error {
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

	if !options.keepProcesses {
		// Match codefly-owned processes by their EXECUTABLE, not by a substring
		// over the whole command line. The old `ps aux | grep codefly.dev`
		// matched any process whose argv merely mentioned the repo path — an
		// editor, a `tail -f`, a build — and kill -9'd it. Agent binaries always
		// live under ~/.codefly/agents/, and the CLI itself is the `codefly`
		// executable; nothing else qualifies.
		self := os.Getpid()
		pids, err := codeflyOwnedPIDs(ctx, self)
		if err != nil {
			w.Warn("cannot enumerate codefly processes", wool.ErrField(err))
			failures = append(failures, err)
		}
		if options.dryRun {
			w.Info("would kill codefly processes", wool.Field("count", len(pids)), wool.Field("pids", pids))
		} else if len(pids) == 0 {
			w.Info("no codefly processes running")
		} else {
			killed := 0
			for _, pid := range pids {
				p, err := os.FindProcess(pid)
				if err != nil {
					failures = append(failures, fmt.Errorf("find process %d: %w", pid, err))
					continue
				}
				if err := p.Kill(); err != nil {
					failures = append(failures, fmt.Errorf("kill process %d: %w", pid, err))
					continue
				}
				killed++
			}
			w.Info("killed codefly processes", wool.Field("killed", killed), wool.Field("total", len(pids)))
		}
	} else {
		w.Info("keeping processes (--keep-processes)")
	}

	// Stale state left by crashed/exited CLIs: per-spawn UDS sockets under
	// /tmp/codefly-uds and process-group tracking files under ~/.codefly/runs.
	// These accumulate over time and were never cleaned by `clear`.
	if options.dryRun {
		if n := manager.CountStaleAgentSockets(); n > 0 {
			w.Info("would remove stale agent sockets", wool.Field("count", n))
		} else {
			w.Info("no stale agent sockets")
		}
	} else {
		if n := manager.SweepStaleAgentSockets(); n > 0 {
			w.Info("removed stale agent sockets", wool.Field("count", n))
		} else {
			w.Info("no stale agent sockets")
		}
		if err := processgroup.ReapStaleProcessGroups(ctx); err != nil {
			w.Warn("cannot reap stale process groups", wool.ErrField(err))
			failures = append(failures, fmt.Errorf("reap stale process groups: %w", err))
		} else {
			w.Info("reaped orphaned process groups")
		}
		if err := postgresipc.ReapOrphanedPostgresIPC(ctx); err != nil {
			w.Warn("cannot reap stale PostgreSQL IPC", wool.ErrField(err))
			failures = append(failures, fmt.Errorf("reap stale PostgreSQL IPC: %w", err))
		} else {
			w.Info("reaped orphaned PostgreSQL IPC")
		}
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
		if err := dockerCLI.Close(); err != nil {
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

	removed := 0
	for _, c := range cos {
		if len(c.Names) == 0 {
			continue
		}
		name := c.Names[0]
		if !strings.HasPrefix(name, "/codefly") {
			continue
		}
		// Filter: if any args were given, the container name must
		// contain at least one of them. With no args, everything
		// codefly-owned matches.
		if len(args) > 0 {
			match := false
			for _, arg := range args {
				if strings.Contains(name, arg) {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		if options.dryRun {
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
	if removed == 0 {
		if len(args) > 0 {
			w.Info("no containers matched filter", wool.Field("filter", args))
		} else {
			w.Info("no codefly containers found")
		}
	} else if options.dryRun {
		w.Info("containers would be removed", wool.Field("count", removed))
	} else {
		w.Info("removed containers", wool.Field("count", removed))
	}
	clearNixDataNote(w)
	return errors.Join(failures...)
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
