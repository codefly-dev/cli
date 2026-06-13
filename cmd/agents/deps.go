package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// DepsCmd manages an agent's build dependencies: the local↔published core
// wiring and its CI. Three concerns, one command:
//
//   - link (default): set up a local go.work so the agent builds against the
//     monorepo's core SOURCE — the "always allow local build" path. No network,
//     no version pull (internal sync). Toggle off for a build with the native
//     GOWORK=off, or `--unlink` to remove it entirely.
//   - --pin: the ONLY mode that pulls — bump go.mod to a published core version,
//     tidy, and verify the agent builds standalone (release / CI readiness).
//   - --ci: scaffold/repair .github/workflows/ci.yml (build+vet vs published core).
//
// `--all` applies any of these across every agent in the directory tree.
var DepsCmd = &cobra.Command{
	Use:   "deps",
	Short: "Manage an agent's core dependency (local go.work, published pin, CI)",
	Long: `Manage how an agent resolves its codefly-core dependency.

By default (or with --link) it wires a local go.work so the agent builds against
the monorepo's core SOURCE — local builds always work, with no network pull
(internal-only sync). CI ignores go.work (it's gitignored) and builds against the
published core in go.mod.

  --link            wire go.work -> local core (default; internal, no pull)
  --unlink          remove the local go.work (revert to published deps)
  --pin <version>   pin go.mod to a published core version + tidy + verify the
                    standalone build (the ONLY mode that pulls); "latest" allowed
  --ci              scaffold/repair .github/workflows/ci.yml
  --all             apply to every agent under the directory tree
  --dir <path>      target agent directory (default: current directory)

Examples:
  codefly agent deps                       # link this agent to local core
  codefly agent deps --all                 # link every agent in the tree
  codefly agent deps --pin latest --ci     # pin to latest published core + add CI
  codefly agent deps --unlink --all        # drop local go.work everywhere`,
	Run: func(cmd *cobra.Command, _ []string) {
		dir, _ := cmd.Flags().GetString("dir")
		all, _ := cmd.Flags().GetBool("all")
		o := depsOptions{}
		o.link, _ = cmd.Flags().GetBool("link")
		o.unlink, _ = cmd.Flags().GetBool("unlink")
		o.pin, _ = cmd.Flags().GetString("pin")
		o.ci, _ = cmd.Flags().GetBool("ci")

		if o.unlink && o.link {
			cli.Error("--link and --unlink are mutually exclusive")
			cli.ExitError()
			return
		}
		// Default action when no mode flag is given: link locally (the internal
		// sync). --pin / --ci / --unlink alone don't imply a link.
		if !o.link && !o.unlink && o.pin == "" && !o.ci {
			o.link = true
		}

		if dir == "" {
			cwd, err := os.Getwd()
			cli.ExitOnError(err, "cannot get working directory")
			dir = cwd
		}
		dir, err := filepath.Abs(dir)
		cli.ExitOnError(err, "cannot resolve directory")

		var targets []string
		if all {
			targets, err = discoverAgentDirs(dir)
			cli.ExitOnError(err, "cannot discover agents")
			if len(targets) == 0 {
				cli.Error("no agent directories (agent.codefly.yaml) found under %s", dir)
				cli.ExitError()
				return
			}
			cli.Header(1, "Applying deps to %d agent(s)", len(targets))
		} else {
			targets = []string{dir}
		}

		var failed []string
		for _, t := range targets {
			if all {
				cli.Header(2, "%s", filepath.Base(t))
			}
			if err := applyDeps(t, o); err != nil {
				cli.Error("  %v", err)
				failed = append(failed, filepath.Base(t))
			}
		}
		if len(failed) > 0 {
			cli.Error("%d agent(s) failed: %s", len(failed), strings.Join(failed, ", "))
			cli.ExitError()
			return
		}
		cli.Header(1, "Done")
	},
}

type depsOptions struct {
	link   bool
	unlink bool
	pin    string
	ci     bool
}

func applyDeps(dir string, o depsOptions) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("%s has no go.mod (not a Go module)", dir)
	}
	// pin first: it rewrites go.mod/go.sum from a pulled published version, then
	// link/ci operate on the result.
	if o.pin != "" {
		if err := pinCore(dir, o.pin); err != nil {
			return err
		}
	}
	if o.unlink {
		if err := unlinkLocal(dir); err != nil {
			return err
		}
	}
	if o.link {
		if err := linkLocal(dir); err != nil {
			return err
		}
	}
	if o.ci {
		if err := scaffoldCI(dir); err != nil {
			return err
		}
	}
	return nil
}

// linkLocal ensures the agent's module(s) are in the monorepo's ROOT go.work,
// so `go build` resolves core (and every other workspace module) from SOURCE —
// no published version, no network. The codefly.dev monorepo uses a single
// root-level go.work (gitignored; CI sets GOWORK=off), so we maintain that
// shared file additively rather than scatter per-agent go.work files (which Go
// rejects as nested workspaces anyway).
func linkLocal(dir string) error {
	root := findMonorepoRoot(dir)
	if root == "" {
		return fmt.Errorf("not inside the codefly.dev monorepo (no core/ module found above %s) — can't link local core", dir)
	}
	// Create the workspace with the shared modules if it doesn't exist yet.
	if !fileExists(filepath.Join(root, "go.work")) {
		seed := []string{"work", "init"}
		for _, m := range []string{"core", "cli", "sdk-go"} {
			if fileExists(filepath.Join(root, m, "go.mod")) {
				seed = append(seed, "./"+m)
			}
		}
		if err := runGo(root, seed...); err != nil {
			return fmt.Errorf("go work init: %w", err)
		}
	}
	// Add the agent itself + any nested modules (base/code, module/services/*/code).
	mods := findGoModDirs(dir)
	if len(mods) == 0 {
		return fmt.Errorf("no go.mod found under %s", dir)
	}
	if err := runGo(root, append([]string{"work", "use"}, mods...)...); err != nil {
		return fmt.Errorf("go work use: %w", err)
	}
	cli.Info("  linked → %d module(s) in %s/go.work [GOWORK=off bypasses]", len(mods), root)
	return nil
}

// unlinkLocal drops the agent's module(s) from the root go.work — it does NOT
// delete the shared file (that would break every other agent's local build).
func unlinkLocal(dir string) error {
	root := findMonorepoRoot(dir)
	if root == "" || !fileExists(filepath.Join(root, "go.work")) {
		cli.Info("  no root go.work to edit")
		return nil
	}
	mods := findGoModDirs(dir)
	for _, m := range mods {
		if err := runGo(root, "work", "edit", "-dropuse", m); err != nil {
			return fmt.Errorf("go work edit -dropuse %s: %w", m, err)
		}
	}
	cli.Info("  unlinked → dropped %d module(s) from %s/go.work", len(mods), root)
	return nil
}

// findGoModDirs returns every directory under root that contains a go.mod
// (the agent root plus nested modules like base/code), skipping vendor and
// node_modules trees.
func findGoModDirs(root string) []string {
	var dirs []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if n == "vendor" || n == "node_modules" || n == ".git" || n == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			dirs = append(dirs, filepath.Dir(p))
		}
		return nil
	})
	return dirs
}

// pinCore bumps go.mod to a published core version and verifies the agent builds
// standalone. This is the only mode that touches the network. Runs with
// GOWORK=off so the local go.work can't mask a missing/incompatible published
// version.
func pinCore(dir, version string) error {
	const core = "github.com/codefly-dev/core"
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	// Strip committed filesystem `replace => ../path` directives first: those are
	// local-dev overrides that don't exist in a single-repo CI checkout, so they
	// break the standalone build. Local builds keep working via the root go.work.
	if dropped, err := stripLocalReplaces(dir, env); err != nil {
		return err
	} else if len(dropped) > 0 {
		cli.Info("  stripped %d local replace(s): %s", len(dropped), strings.Join(dropped, ", "))
	}
	if err := runGoEnv(dir, env, "get", core+"@"+version); err != nil {
		return fmt.Errorf("go get %s@%s: %w", core, version, err)
	}
	if err := runGoEnv(dir, env, "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	// Verify the standalone (published-core) build — fail loudly if the agent
	// uses an API the pinned version doesn't have.
	if err := runGoEnv(dir, env, "build", "./..."); err != nil {
		return fmt.Errorf("standalone build against %s@%s FAILED — the agent likely uses an API not in that version: %w", core, version, err)
	}
	got := goModVersion(dir, core)
	cli.Info("  pinned → %s@%s (standalone build OK)", core, got)
	return nil
}

// stripLocalReplaces removes every filesystem `replace X => ../path` directive
// from dir's go.mod (those whose replacement has no version — i.e. a local
// path, not a module). Returns the module paths dropped. Module-version
// replaces (replace X => Y v1.2.3) are left untouched.
func stripLocalReplaces(dir string, env []string) ([]string, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod edit -json: %w", err)
	}
	var mf struct {
		Replace []struct {
			Old struct{ Path, Version string }
			New struct{ Path, Version string }
		}
	}
	if err := json.Unmarshal(out, &mf); err != nil {
		return nil, fmt.Errorf("parse go.mod json: %w", err)
	}
	var dropped []string
	for _, r := range mf.Replace {
		if r.New.Version != "" {
			continue // a module-version replace, not a local path — keep it
		}
		arg := r.Old.Path
		if r.Old.Version != "" {
			arg = r.Old.Path + "@" + r.Old.Version
		}
		if err := runGoEnv(dir, env, "mod", "edit", "-dropreplace", arg); err != nil {
			return nil, fmt.Errorf("dropreplace %s: %w", arg, err)
		}
		dropped = append(dropped, r.Old.Path)
	}
	return dropped, nil
}

// scaffoldCI writes .github/workflows/ci.yml (build+vet against published core,
// Go version read from go.mod). Idempotent — overwrites the managed file.
func scaffoldCI(dir string) error {
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", wf, err)
	}
	if err := os.WriteFile(filepath.Join(wf, "ci.yml"), []byte(ciWorkflow), 0o644); err != nil {
		return fmt.Errorf("write ci.yml: %w", err)
	}
	cli.Info("  ci → wrote .github/workflows/ci.yml (build+vet)")
	return nil
}

// ciWorkflow is the managed build+vet workflow. It builds against the published
// core (no go.work in CI), with the Go toolchain pinned from go.mod. It runs
// build + vet only — NOT `go test`, because agent tests need real infra
// (NEVER mock) and would be falsely red in plain GitHub Actions.
const ciWorkflow = `name: ci
on:
  push:
    branches: [ main ]
  pull_request:
    branches: [ main ]
concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: true
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: 'go.mod'
      - name: Build
        run: go build ./...
      - name: Vet
        run: go vet ./...
`

// discoverAgentDirs returns every directory under root that holds an
// agent.codefly.yaml (including quarantined ones — they still need dep wiring).
func discoverAgentDirs(root string) ([]string, error) {
	var dirs []string
	// First: is root itself an agent?
	if fileExists(filepath.Join(root, "agent.codefly.yaml")) {
		return []string{root}, nil
	}
	walk := func(base string) {
		entries, err := os.ReadDir(base)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			d := filepath.Join(base, e.Name())
			if fileExists(filepath.Join(d, "agent.codefly.yaml")) {
				dirs = append(dirs, d)
			}
		}
	}
	// Look one and two levels deep (agents/services/<name>, agents/<group>/<name>).
	walk(root)
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			walk(filepath.Join(root, e.Name()))
		}
	}
	return dedupeStrings(dirs), nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func runGo(dir string, args ...string) error {
	return runGoEnv(dir, os.Environ(), args...)
}

func runGoEnv(dir string, env []string, args ...string) error {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// goModVersion returns the required version of module in dir's go.mod (or "?").
func goModVersion(dir, module string) string {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "?"
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) >= 2 && f[0] == module {
			return f[1]
		}
	}
	return "?"
}

func init() {
	DepsCmd.Flags().String("dir", "", "Agent source directory (default: current directory)")
	DepsCmd.Flags().Bool("all", false, "Apply to every agent found in the directory tree")
	DepsCmd.Flags().Bool("link", false, "Wire go.work -> local core for dev builds (default action)")
	DepsCmd.Flags().Bool("unlink", false, "Remove the local go.work (revert to published deps)")
	DepsCmd.Flags().String("pin", "", "Pin go.mod to a published core version (e.g. latest, v0.1.164) + tidy + verify")
	DepsCmd.Flags().Bool("ci", false, "Scaffold/repair .github/workflows/ci.yml")
}
