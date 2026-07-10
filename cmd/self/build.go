// Package self contains commands that operate on the codefly CLI itself,
// mirroring `codefly agent build` but for the `codefly` binary.
package self

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/agents"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

const cliModulePath = "github.com/codefly-dev/cli"

// BuildCmd rebuilds the codefly CLI from source and installs it over the
// currently running binary. It is the CLI counterpart to
// `codefly agent build`: where that rebuilds a plugin agent, this rebuilds
// codefly itself so local changes to cli/ (and, via go.work, core/ and
// wool/) take effect without remembering the build script path.
var BuildCmd = &cobra.Command{
	Use:   "build",
	Short: "Rebuild the codefly CLI from source and install it",
	Long: `Build compiles the codefly CLI from source and installs it over the
binary currently on your PATH (the one you just invoked).

The CLI source directory is auto-detected by walking up from the current
directory: it looks for the cli module (the directory whose go.mod is
` + cliModulePath + `), or a monorepo root containing cli/. Inside the
codefly.dev monorepo the build picks up local core/ and wool/ changes
automatically via go.work.

With --with-agents, after the CLI is installed it also rebuilds every
agent under agents/services/ (the same set as ` + "`codefly agent build --all`" + `).
This is the ONE command to pick up local changes to both the CLI and the
agents in a single step. The per-agent govulncheck audit is skipped during
the bulk build (it adds minutes); pass --audit-agents to run it.

By default each agent is built twice: the native host binary and a
Linux/amd64 static binary for Docker-mode container runs. On a mac running
everything natively the Linux binary is never executed, so --native-only
skips that cross-build and rebuilds only the host binaries — halving the
agent build work for a local edit→rebuild→run loop.

Agents build in parallel, defaulting to one job per CPU. Use -j/--jobs to
cap that concurrency (mirrors ` + "`codefly agent build --all -j`" + `).

Examples:
  codefly self build
  codefly self build --with-agents
  codefly self build --with-agents --native-only
  codefly self build --with-agents -j 4
  codefly self build --with-agents --audit-agents
  codefly self build --dir ./cli
  codefly self build --output /usr/local/bin/codefly`,
	Run: func(cmd *cobra.Command, args []string) {
		dir, _ := cmd.Flags().GetString("dir")
		output, _ := cmd.Flags().GetString("output")
		withAgents, _ := cmd.Flags().GetBool("with-agents")
		auditAgents, _ := cmd.Flags().GetBool("audit-agents")
		nativeOnly, _ := cmd.Flags().GetBool("native-only")
		jobs, _ := cmd.Flags().GetInt("jobs")
		goos, _ := cmd.Flags().GetString("os")
		goarch, _ := cmd.Flags().GetString("arch")

		srcDir, err := resolveCLISource(dir)
		if err != nil {
			cli.Error("Cannot locate codefly CLI source: %v", err)
			cli.ExitError()
			return
		}

		// Cross-compile mode (--os/--arch): produce a static binary for
		// another platform. This replaces scripts/build/linux.sh. We never
		// install over the running binary in this mode — a foreign-arch
		// binary can't replace the one in use — so --output is required
		// (defaulting to a conventional bin/<os>/codefly under the monorepo).
		cross := goos != "" || goarch != ""
		if cross {
			if goos == "" {
				goos = "linux"
			}
			if goarch == "" {
				goarch = "amd64"
			}
			if output == "" {
				output = defaultCrossOutput(srcDir, goos)
			}
			cli.Info("Cross-compiling codefly from %s for %s/%s", srcDir, goos, goarch)
			if err := buildCLICross(srcDir, output, goos, goarch); err != nil {
				cli.Error("Build failed: %v", err)
				cli.ExitError()
				return
			}
			cli.Info("Built %s/%s codefly at %s", goos, goarch, output)
			return
		}

		if output == "" {
			output, err = os.Executable()
			if err != nil {
				cli.Error("Cannot resolve the running codefly binary (pass --output): %v", err)
				cli.ExitError()
				return
			}
			// Resolve symlinks so we overwrite the real binary, not a link.
			if resolved, lerr := filepath.EvalSymlinks(output); lerr == nil {
				output = resolved
			}
		}

		cli.Info("Building codefly from %s", srcDir)
		if err := buildCLI(srcDir, output); err != nil {
			cli.Error("Build failed: %v", err)
			cli.ExitError()
			return
		}
		cli.Info("Installed codefly to %s", output)

		if withAgents {
			agentsDir, aerr := resolveAgentsServices(srcDir)
			if aerr != nil {
				cli.Error("Cannot locate agents to build (--with-agents): %v", aerr)
				cli.ExitError()
				return
			}
			cli.Info("Building all agents in %s", agentsDir)
			// Skip the per-agent govulncheck audit by default: `self build
			// --with-agents` is a fast "rebuild to pick up local changes" loop,
			// and auditing every agent adds minutes (each runs govulncheck
			// against the vuln DB). Opt in with --audit-agents when you want it.
			if berr := agents.BuildAllAgents(agentsDir, agents.BuildOptions{SkipAudit: !auditAgents, NativeOnly: nativeOnly, Jobs: jobs}); berr != nil {
				cli.Error("Agent build failed: %v", berr)
				cli.ExitError()
				return
			}
		}
	},
}

// resolveAgentsServices finds the monorepo's agents/services directory by
// walking up from the CLI source dir. This is where `agent build --all`
// discovers every agent — reusing it keeps `self build --with-agents` and
// `agent build --all` pointed at the same set.
func resolveAgentsServices(cliSrcDir string) (string, error) {
	for d := cliSrcDir; ; {
		candidate := filepath.Join(d, "agents", "services")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", fmt.Errorf("no agents/services directory found above %s", cliSrcDir)
}

func init() {
	BuildCmd.Flags().String("dir", "", "CLI source directory (default: auto-detect from current directory)")
	BuildCmd.Flags().String("output", "", "Install path (default: the running codefly binary; for --os/--arch: bin/<os>/codefly)")
	BuildCmd.Flags().Bool("with-agents", false, "After rebuilding the CLI, also rebuild every agent under agents/services/")
	BuildCmd.Flags().Bool("audit-agents", false, "Run the govulncheck audit on each agent during --with-agents (slow; off by default)")
	BuildCmd.Flags().Bool("native-only", false, "With --with-agents: build only host-platform agent binaries; skip the Linux/amd64 container cross-build (local dev fast path)")
	BuildCmd.Flags().IntP("jobs", "j", 0, "With --with-agents: max agents to build in parallel (default: number of CPUs)")
	BuildCmd.Flags().String("os", "", "Cross-compile for this GOOS (e.g. linux); produces a static binary instead of installing")
	BuildCmd.Flags().String("arch", "", "Cross-compile for this GOARCH (e.g. amd64); implies cross-compile")
}

// defaultCrossOutput returns the conventional cross-compiled binary path:
// <monorepo>/core/bin/<os>/codefly when the core dir can be located
// (matches the path the companion build expects), else bin/<os>/codefly
// under the CLI source. Mirrors scripts/build/linux.sh's destination.
func defaultCrossOutput(cliSrcDir, goos string) string {
	for d := cliSrcDir; ; {
		if info, err := os.Stat(filepath.Join(d, "core")); err == nil && info.IsDir() {
			return filepath.Join(d, "core", "bin", goos, "codefly")
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return filepath.Join(cliSrcDir, "bin", goos, "codefly")
}

// buildCLICross compiles a static CLI binary for goos/goarch. Static
// (CGO_ENABLED=0 + -extldflags "-static") so alpine images can run it
// without glibc; stripped (-s -w) for size. Replaces scripts/build/linux.sh.
func buildCLICross(srcDir, output, goos, goarch string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	start := time.Now()
	build := exec.Command("go", "build",
		"-ldflags", `-s -w -extldflags "-static"`,
		"-o", output,
		".",
	)
	build.Dir = srcDir
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}
	cli.Info("Built in %s", time.Since(start).Round(time.Millisecond))
	return nil
}

// buildCLI compiles the CLI in srcDir and atomically installs it to output.
// It builds to a temp file in the destination directory first, then renames
// over the target so replacing the binary that is currently running does not
// fail with "text file busy".
func buildCLI(srcDir, output string) error {
	destDir := filepath.Dir(output)
	tmp, err := os.CreateTemp(destDir, ".codefly-build-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", destDir, err)
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	// Clean up the temp file on any failure path; on success it has already
	// been renamed away so the remove is a harmless no-op.
	defer func() { _ = os.Remove(tmpPath) }()

	start := time.Now()
	// Build from srcDir so go.work (one level up in the monorepo) is honored
	// and local core/wool replace directives are picked up.
	build := exec.Command("go", "build", "-o", tmpPath, ".")
	build.Dir = srcDir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if err := os.Rename(tmpPath, output); err != nil {
		return fmt.Errorf("install to %s: %w", output, err)
	}
	cli.Info("Built in %s", time.Since(start).Round(time.Millisecond))
	return nil
}

// resolveCLISource finds the codefly CLI source directory. If dir is given it
// is validated directly; otherwise it walks up from the current directory
// looking for the cli module or a monorepo root that contains cli/.
func resolveCLISource(dir string) (string, error) {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		if isCLIModule(abs) {
			return abs, nil
		}
		if cliDir := filepath.Join(abs, "cli"); isCLIModule(cliDir) {
			return cliDir, nil
		}
		return "", fmt.Errorf("%s is not the codefly CLI module", abs)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if found := walkForCLIModule(cwd); found != "" {
		return found, nil
	}

	// Fall back to the running binary's own location. The installed codefly is a
	// symlink (~/.local/bin/codefly -> <repo>/cli/codefly), so the resolved
	// executable lives INSIDE the CLI source tree. This lets `self build` work
	// from anywhere — e.g. a sibling workspace like ./mind, from which the
	// monorepo's cli/ is never an ancestor of cwd and the walk above finds
	// nothing.
	if exe, exeErr := os.Executable(); exeErr == nil {
		if resolved, symErr := filepath.EvalSymlinks(exe); symErr == nil {
			if found := walkForCLIModule(filepath.Dir(resolved)); found != "" {
				return found, nil
			}
		}
	}

	return "", fmt.Errorf("no codefly CLI source found from %s or the codefly binary location (use --dir)", cwd)
}

// walkForCLIModule walks up from start looking for the cli module directly, or a
// monorepo root that contains cli/. Returns "" if neither is found.
func walkForCLIModule(start string) string {
	for d := start; ; {
		if isCLIModule(d) {
			return d
		}
		if cliDir := filepath.Join(d, "cli"); isCLIModule(cliDir) {
			return cliDir
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// isCLIModule reports whether dir contains main.go and a go.mod declaring the
// codefly CLI module path.
func isCLIModule(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest) == cliModulePath
		}
	}
	return false
}
