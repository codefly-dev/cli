package cmd

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

var (
	clearKeepProcesses bool
	clearKeepContainers bool
	clearDryRun         bool
)

// ClearCmd removes codefly state (running processes + docker containers).
// Without arguments: removes EVERYTHING codefly-owned.
// With positional args (e.g. `codefly clear neo4j`): removes only
// containers whose name contains any of the substrings — useful when
// you want to recycle one piece of infra without nuking the rest.
var ClearCmd = &cobra.Command{
	Use:   "clear [name-filter...]",
	Short: "Clear codefly processes + docker containers",
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
	Run: func(cmd *cobra.Command, args []string) {
		clearCommand(args)
	},
}

func init() {
	ClearCmd.Flags().BoolVar(&clearKeepProcesses, "keep-processes", false, "Don't kill running codefly processes (only remove containers)")
	ClearCmd.Flags().BoolVar(&clearKeepContainers, "keep-containers", false, "Don't remove docker containers (only kill processes)")
	ClearCmd.Flags().BoolVar(&clearDryRun, "dry-run", false, "List what would be removed without removing anything")
}

func clearCommand(args []string) {
	ctx := context.Background()

	// Always announce what we're doing — `clear` was silent on the common path
	// (no docker containers + a quiet process kill), so it looked like a no-op.
	if clearDryRun {
		fmt.Println("codefly clear (dry-run): nothing will be removed")
	} else if len(args) > 0 {
		fmt.Printf("codefly clear: scope=%v\n", args)
	} else {
		fmt.Println("codefly clear: full reset")
	}

	if !clearKeepProcesses {
		// Capture matching PIDs first so we can REPORT the count — a silent
		// kill made `clear` look like it did nothing.
		c := exec.CommandContext(ctx, "bash", "-c", "ps aux | grep codefly.dev | grep -v grep | awk '{print $2}'")
		out, _ := c.Output()
		pids := strings.Fields(strings.TrimSpace(string(out)))
		if clearDryRun {
			fmt.Printf("processes: would kill %d codefly process(es): %v\n", len(pids), pids)
		} else if len(pids) == 0 {
			fmt.Println("processes: none running")
		} else {
			k := exec.CommandContext(ctx, "bash", "-c", "ps aux | grep codefly.dev | grep -v grep | awk '{print $2}' | xargs kill -9 2>/dev/null; true")
			_ = k.Run()
			fmt.Printf("processes: killed %d codefly process(es)\n", len(pids))
		}
	} else {
		fmt.Println("processes: kept (--keep-processes)")
	}

	if clearKeepContainers {
		return
	}

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Printf("containers: docker unavailable (%v) — skipping (nix-run services are not docker containers)\n", err)
		clearNixDataNote()
		return
	}
	defer cli.Close()
	cos, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		fmt.Printf("containers: cannot list (%v) — docker may be down; skipping\n", err)
		clearNixDataNote()
		return
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
		if clearDryRun {
			fmt.Printf("would remove container: %s (%s)\n", strings.TrimPrefix(name, "/"), c.State)
			removed++
			continue
		}
		fmt.Printf("removing container: %s\n", strings.TrimPrefix(name, "/"))
		// ContainerKill can error when container is already stopped;
		// the Remove --force below handles cleanup either way, so
		// a kill error here is noise, not fatal.
		_ = cli.ContainerKill(ctx, c.ID, "SIGKILL")
		if err := cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
			log.Fatalf("can't remove container %s: %s\n", name, err)
		}
		removed++
	}
	if removed == 0 {
		if len(args) > 0 {
			fmt.Printf("containers: none matched filter %v\n", args)
		} else {
			fmt.Println("containers: none found")
		}
	} else if clearDryRun {
		fmt.Printf("containers: %d would be removed (dry-run)\n", removed)
	} else {
		fmt.Printf("containers: %d removed\n", removed)
	}
	clearNixDataNote()
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
