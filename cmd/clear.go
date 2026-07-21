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
	"github.com/codefly-dev/core/agents/manager"
	runnersbase "github.com/codefly-dev/core/runners/base"
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

	// Always announce what we're doing — `clear` was silent on the common path
	// (no docker containers + a quiet process kill), so it looked like a no-op.
	switch {
	case options.dryRun:
		fmt.Printf("codefly %s (dry-run): nothing will be removed\n", options.verb)
	case len(args) > 0:
		fmt.Printf("codefly %s: scope=%v\n", options.verb, args)
	case options.keepContainers:
		fmt.Printf("codefly %s: reap processes + orphaned groups (containers kept for reuse)\n", options.verb)
	default:
		fmt.Printf("codefly %s: full reset\n", options.verb)
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
			fmt.Printf("processes: cannot enumerate: %v\n", err)
			failures = append(failures, err)
		}
		if options.dryRun {
			fmt.Printf("processes: would kill %d codefly process(es): %v\n", len(pids), pids)
		} else if len(pids) == 0 {
			fmt.Println("processes: none running")
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
			fmt.Printf("processes: killed %d of %d codefly process(es)\n", killed, len(pids))
		}
	} else {
		fmt.Println("processes: kept (--keep-processes)")
	}

	// Stale state left by crashed/exited CLIs: per-spawn UDS sockets under
	// /tmp/codefly-uds and process-group tracking files under ~/.codefly/runs.
	// These accumulate over time and were never cleaned by `clear`.
	if options.dryRun {
		if n := manager.CountStaleAgentSockets(); n > 0 {
			fmt.Printf("sockets: would remove %d stale agent socket(s) (dry-run)\n", n)
		} else {
			fmt.Println("sockets: none stale")
		}
	} else {
		if n := manager.SweepStaleAgentSockets(); n > 0 {
			fmt.Printf("sockets: removed %d stale agent socket(s)\n", n)
		} else {
			fmt.Println("sockets: none stale")
		}
		if err := runnersbase.ReapStaleProcessGroups(ctx); err != nil {
			fmt.Printf("process-groups: reap error: %v\n", err)
			failures = append(failures, fmt.Errorf("reap stale process groups: %w", err))
		} else {
			fmt.Println("process-groups: reaped orphaned groups")
		}
	}

	if options.keepContainers {
		return errors.Join(failures...)
	}

	dockerCLI, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Printf("containers: docker unavailable (%v) — skipping (nix-run services are not docker containers)\n", err)
		clearNixDataNote()
		return errors.Join(failures...)
	}
	defer func() {
		if err := dockerCLI.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close docker client: %w", err))
		}
	}()
	cos, err := dockerCLI.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		fmt.Printf("containers: cannot list (%v) — docker may be down; skipping\n", err)
		clearNixDataNote()
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
			fmt.Printf("would remove container: %s (%s)\n", strings.TrimPrefix(name, "/"), c.State)
			removed++
			continue
		}
		fmt.Printf("removing container: %s\n", strings.TrimPrefix(name, "/"))
		// ContainerKill can error when container is already stopped;
		// the Remove --force below handles cleanup either way, so
		// a kill error here is noise, not fatal.
		_ = dockerCLI.ContainerKill(ctx, c.ID, "SIGKILL")
		if err := dockerCLI.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			// Don't abort the whole sweep on one stubborn container — warn and
			// keep going so the rest still get cleaned (was log.Fatalf).
			fmt.Printf("warning: can't remove container %s: %v\n", strings.TrimPrefix(name, "/"), err)
			failures = append(failures, fmt.Errorf("remove container %s: %w", strings.TrimPrefix(name, "/"), err))
			continue
		}
		removed++
	}
	if removed == 0 {
		if len(args) > 0 {
			fmt.Printf("containers: none matched filter %v\n", args)
		} else {
			fmt.Println("containers: none found")
		}
	} else if options.dryRun {
		fmt.Printf("containers: %d would be removed (dry-run)\n", removed)
	} else {
		fmt.Printf("containers: %d removed\n", removed)
	}
	clearNixDataNote()
	return errors.Join(failures...)
}

// clearNixDataNote explains the one thing `clear` deliberately does NOT touch:
// nix-run service DATA (Postgres/Neo4j data dirs). `clear` removes processes +
// docker containers, but nix services keep their data under ~/.codefly/data, so a
// wedged DB (e.g. a dirty golang-migrate state) survives `clear`. This note makes
// that explicit + gives the reset command.
func clearNixDataNote() {
	fmt.Println("note: nix-run service DATA is NOT removed by clear (only processes + docker containers).")
	fmt.Println("      to reset a service's data — e.g. a dirty/wedged DB migration — stop codefly and run:")
	fmt.Println("        rm -rf ~/.codefly/data/<workspace>    # e.g. ~/.codefly/data/mind-server")
	fmt.Println("      then `codefly run service <svc>` re-applies migrations from scratch.")
}
