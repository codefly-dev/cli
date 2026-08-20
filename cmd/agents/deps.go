package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// DepsCmd manages an agent's build dependencies: the local↔published core
// wiring. Two concerns, one command:
//
//   - link (default): set up a local go.work so the agent builds against the
//     monorepo's core SOURCE — the "always allow local build" path. No network,
//     no version pull (internal sync). Toggle off for a build with the native
//     GOWORK=off, or `--unlink` to remove it entirely.
//   - --pin: the ONLY mode that pulls — bump every dependency lock the agent
//     owns (its go.mod, any nested base fixtures, and the factory templates
//     generated from them) to a published core version, tidy, and verify each
//     builds standalone (release / CI readiness).
//
// `--all` applies any of these across every agent in the directory tree.
var DepsCmd = &cobra.Command{
	Use:   "deps",
	Short: "Switch an agent between a local core checkout and a published version",
	Long: `Manage how an agent resolves its codefly-core dependency.

By default (or with --link) it wires a local go.work so the agent builds against
the monorepo's core SOURCE — local builds always work, with no network pull
(internal-only sync). CI ignores go.work (it's gitignored) and builds against the
published core in go.mod.

  --link            wire go.work -> local core (default; internal, no pull)
  --unlink          remove the local go.work (revert to published deps)
  --pin <version>   pin every lock the agent owns (go.mod, nested base fixtures,
                    and their factory templates) to a published core version +
                    tidy + verify the standalone build (the ONLY mode that
                    pulls); "latest" allowed
  --all             apply to every agent under the directory tree
  --dir <path>      target agent directory (default: current directory)

Examples:
  codefly agent deps                       # link this agent to local core
  codefly agent deps --all                 # link every agent in the tree
  codefly agent deps --pin latest          # pin to latest published core
  codefly agent deps --unlink --all        # drop local go.work everywhere`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		dir, err := cmd.Flags().GetString("dir")
		if err != nil {
			return fmt.Errorf("cannot read --dir: %w", err)
		}
		all, err := cmd.Flags().GetBool("all")
		if err != nil {
			return fmt.Errorf("cannot read --all: %w", err)
		}
		o := depsOptions{}
		o.link, err = cmd.Flags().GetBool("link")
		if err != nil {
			return fmt.Errorf("cannot read --link: %w", err)
		}
		o.unlink, err = cmd.Flags().GetBool("unlink")
		if err != nil {
			return fmt.Errorf("cannot read --unlink: %w", err)
		}
		o.pin, err = cmd.Flags().GetString("pin")
		if err != nil {
			return fmt.Errorf("cannot read --pin: %w", err)
		}
		if o.unlink && o.link {
			return fmt.Errorf("--link and --unlink are mutually exclusive")
		}
		// Default action when no mode flag is given: link locally (the internal
		// sync). --pin / --unlink alone don't imply a link.
		if !o.link && !o.unlink && o.pin == "" {
			o.link = true
		}

		if dir == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot get working directory: %w", err)
			}
			dir = cwd
		}
		dir, err = filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("cannot resolve directory: %w", err)
		}

		var targets []string
		if all {
			targets, err = discoverAgentDirs(dir)
			if err != nil {
				return fmt.Errorf("cannot discover agents: %w", err)
			}
			if len(targets) == 0 {
				return fmt.Errorf("no agent directories (agent.codefly.yaml) found under %s", dir)
			}
			cli.Header(1, "Applying deps to %d agent(s)", len(targets))
		} else {
			targets = []string{dir}
		}

		var failures []error
		for _, t := range targets {
			if err := ctx.Err(); err != nil {
				failures = append(failures, err)
				break
			}
			if all {
				cli.Header(2, "%s", filepath.Base(t))
			}
			if err := applyDeps(ctx, t, o); err != nil {
				cli.Error("  %v", err)
				failures = append(failures, fmt.Errorf("%s: %w", filepath.Base(t), err))
			}
		}
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
		cli.Header(1, "Done")
		return nil
	},
}

type depsOptions struct {
	link   bool
	unlink bool
	pin    string
}

func applyDeps(ctx context.Context, dir string, o depsOptions) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("%s has no go.mod (not a Go module)", dir)
	}
	// pin first: it rewrites go.mod/go.sum from a pulled published version, then
	// link/ci operate on the result.
	if o.pin != "" {
		if err := pinCore(ctx, dir, o.pin); err != nil {
			return err
		}
	}
	if o.unlink {
		if err := unlinkLocal(ctx, dir); err != nil {
			return err
		}
	}
	if o.link {
		if err := linkLocal(ctx, dir); err != nil {
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
func linkLocal(ctx context.Context, dir string) (returnErr error) {
	root := findMonorepoRoot(dir)
	if root == "" {
		return fmt.Errorf("not inside the codefly.dev monorepo (no core/ module found above %s) — can't link local core", dir)
	}
	snapshots, err := snapshotFiles(filepath.Join(root, "go.work"), filepath.Join(root, "go.work.sum"))
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, restoreFiles(context.WithoutCancel(ctx), snapshots))
		}
	}()

	// Create the workspace with the shared modules if it doesn't exist yet.
	if !fileExists(filepath.Join(root, "go.work")) {
		seed := []string{"work", "init"}
		for _, m := range []string{"core", "cli", "sdk-go"} {
			if fileExists(filepath.Join(root, m, "go.mod")) {
				seed = append(seed, "./"+m)
			}
		}
		if err := runGo(ctx, root, seed...); err != nil {
			return fmt.Errorf("go work init: %w", err)
		}
	}
	// Add the agent itself + any nested modules (base/code, module/services/*/code).
	mods, err := findGoModDirs(dir)
	if err != nil {
		return err
	}
	if len(mods) == 0 {
		return fmt.Errorf("no go.mod found under %s", dir)
	}
	if err := runGo(ctx, root, append([]string{"work", "use"}, mods...)...); err != nil {
		return fmt.Errorf("go work use: %w", err)
	}
	cli.Info("  linked → %d module(s) in %s/go.work [GOWORK=off bypasses]", len(mods), root)
	return nil
}

// unlinkLocal drops the agent's module(s) from the root go.work — it does NOT
// delete the shared file (that would break every other agent's local build).
func unlinkLocal(ctx context.Context, dir string) (returnErr error) {
	root := findMonorepoRoot(dir)
	if root == "" || !fileExists(filepath.Join(root, "go.work")) {
		cli.Info("  no root go.work to edit")
		return nil
	}
	snapshots, err := snapshotFiles(filepath.Join(root, "go.work"), filepath.Join(root, "go.work.sum"))
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, restoreFiles(context.WithoutCancel(ctx), snapshots))
		}
	}()
	mods, err := findGoModDirs(dir)
	if err != nil {
		return err
	}
	for _, m := range mods {
		if err := runGo(ctx, root, "work", "edit", "-dropuse", m); err != nil {
			return fmt.Errorf("go work edit -dropuse %s: %w", m, err)
		}
	}
	cli.Info("  unlinked → dropped %d module(s) from %s/go.work", len(mods), root)
	return nil
}

// findGoModDirs returns every directory under root that contains a go.mod
// (the agent root plus nested modules like base/code), skipping vendor and
// node_modules trees.
func findGoModDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
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
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)
	return dirs, nil
}

const coreModule = "github.com/codefly-dev/core"

// pinCore bumps every dependency lock the agent owns to a published core
// version and verifies each module builds standalone. Beyond the agent's own
// go.mod this covers nested base fixtures (base/code) and the factory templates
// generated from them (templates/factory/code/go.{mod,sum}.tmpl) — pinning only
// the root leaves fresh services generated on the stale lock while reporting
// success. Runs with GOWORK=off so the local go.work can't mask a
// missing/incompatible published version.
func pinCore(ctx context.Context, dir, version string) (returnErr error) {
	env := append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")

	nested, err := nestedGoModDirs(dir)
	if err != nil {
		return err
	}
	// Snapshot every lock a pin touches up front — agent + nested modules and
	// their factory templates — so any failure restores the whole set rather
	// than leaving a partial pin, the exact drift this command guards against.
	var snapPaths []string
	for _, m := range append([]string{dir}, nested...) {
		snapPaths = append(snapPaths, filepath.Join(m, "go.mod"), filepath.Join(m, "go.sum"))
	}
	for _, m := range nested {
		if td := factoryTemplateDir(dir, m); td != "" {
			snapPaths = append(snapPaths, filepath.Join(td, "go.mod.tmpl"), filepath.Join(td, "go.sum.tmpl"))
		}
	}
	snapshots, err := snapshotFiles(snapPaths...)
	if err != nil {
		return err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, restoreFiles(context.WithoutCancel(ctx), snapshots))
		}
	}()

	if err := pinModule(ctx, dir, env, version, ""); err != nil {
		return err
	}
	for _, m := range nested {
		label := relLabel(dir, m)
		if err := pinModule(ctx, m, env, version, label); err != nil {
			return err
		}
		if err := regenerateFactoryTemplate(ctx, dir, m); err != nil {
			return err
		}
	}
	return nil
}

// pinModule pins a single Go module to coreModule@version, tidies, and verifies
// its standalone build. label names the module in progress output ("" for the
// agent root).
func pinModule(ctx context.Context, dir string, env []string, version, label string) error {
	// Strip committed filesystem `replace => ../path` directives first: those are
	// local-dev overrides that don't exist in a single-repo CI checkout, so they
	// break the standalone build. Local builds keep working via the root go.work.
	if dropped, err := stripLocalReplaces(ctx, dir, env); err != nil {
		return err
	} else if len(dropped) > 0 {
		cli.Info("  stripped %d local replace(s): %s", len(dropped), strings.Join(dropped, ", "))
	}
	if err := runGoEnv(ctx, dir, env, "get", coreModule+"@"+version); err != nil {
		return fmt.Errorf("go get %s@%s: %w", coreModule, version, err)
	}
	if err := runGoEnv(ctx, dir, env, "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	// Verify the standalone (published-core) build — fail loudly if the module
	// uses an API the pinned version doesn't have.
	if err := runGoEnv(ctx, dir, env, "build", "./..."); err != nil {
		return fmt.Errorf("standalone build against %s@%s FAILED — the agent likely uses an API not in that version: %w", coreModule, version, err)
	}
	got := goModVersion(dir, coreModule)
	if label == "" {
		cli.Info("  pinned → %s@%s (standalone build OK)", coreModule, got)
	} else {
		cli.Info("  pinned %s → %s@%s (standalone build OK)", label, coreModule, got)
	}
	return nil
}

// nestedGoModDirs returns every go.mod directory strictly under dir (the agent's
// base fixtures and other embedded modules), excluding the agent root itself.
func nestedGoModDirs(dir string) ([]string, error) {
	all, err := findGoModDirs(dir)
	if err != nil {
		return nil, err
	}
	var nested []string
	for _, d := range all {
		if d != dir {
			nested = append(nested, d)
		}
	}
	return nested, nil
}

// factoryTemplateDir maps a base fixture module at agentDir/base/<rel> to its
// factory template directory agentDir/templates/factory/<rel>, returning "" when
// the module isn't a base fixture or carries no go.mod.tmpl there.
func factoryTemplateDir(agentDir, moduleDir string) string {
	rel, err := filepath.Rel(agentDir, moduleDir)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 || parts[0] != "base" {
		return ""
	}
	td := filepath.Join(append([]string{agentDir, "templates", "factory"}, parts[1:]...)...)
	if !fileExists(filepath.Join(td, "go.mod.tmpl")) {
		return ""
	}
	return td
}

// regenerateFactoryTemplate rewrites a base fixture's factory go.mod.tmpl and
// go.sum.tmpl from the just-pinned base module so a fresh service can never be
// generated on an older lock than base/. The base module path is swapped back
// for the template's service-name placeholder, read from the existing template
// so the token isn't hard-coded here.
func regenerateFactoryTemplate(ctx context.Context, agentDir, moduleDir string) error {
	td := factoryTemplateDir(agentDir, moduleDir)
	if td == "" {
		return nil
	}
	baseMod, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		return err
	}
	baseSum, err := os.ReadFile(filepath.Join(moduleDir, "go.sum"))
	if err != nil {
		return err
	}
	baseModule := goModModule(baseMod)
	if baseModule == "" {
		return fmt.Errorf("cannot read module path from %s/go.mod", relLabel(agentDir, moduleDir))
	}
	placeholder, err := templateModulePlaceholder(filepath.Join(td, "go.mod.tmpl"))
	if err != nil {
		return err
	}
	modTmpl := strings.ReplaceAll(string(baseMod), baseModule, placeholder)
	sumTmpl := strings.ReplaceAll(string(baseSum), baseModule, placeholder)
	if err := shared.WriteFileAtomic(ctx, filepath.Join(td, "go.mod.tmpl"), []byte(modTmpl), 0o644); err != nil {
		return err
	}
	if err := shared.WriteFileAtomic(ctx, filepath.Join(td, "go.sum.tmpl"), []byte(sumTmpl), 0o644); err != nil {
		return err
	}
	cli.Info("  regenerated factory template %s from %s", relLabel(agentDir, td), relLabel(agentDir, moduleDir))
	return nil
}

// goModModule returns the module path declared by a go.mod's `module` line.
func goModModule(goMod []byte) string {
	for _, line := range strings.Split(string(goMod), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "module" {
			return f[1]
		}
	}
	return ""
}

// templateModulePlaceholder returns the module-line token of a go.mod.tmpl, e.g.
// "{{ .Service.Name.DNSCase }}" from "module {{ .Service.Name.DNSCase }}".
func templateModulePlaceholder(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "module "); ok {
			return strings.TrimSpace(rest), nil
		}
	}
	return "", fmt.Errorf("no module line in %s", path)
}

// relLabel renders target relative to base with forward slashes for progress
// output, falling back to the raw path when it can't be made relative.
func relLabel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return filepath.ToSlash(rel)
}

// stripLocalReplaces removes every filesystem `replace X => ../path` directive
// from dir's go.mod (those whose replacement has no version — i.e. a local
// path, not a module). Returns the module paths dropped. Module-version
// replaces (replace X => Y v1.2.3) are left untouched.
func stripLocalReplaces(ctx context.Context, dir string, env []string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "go", "mod", "edit", "-json")
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
		if err := runGoEnv(ctx, dir, env, "mod", "edit", "-dropreplace", arg); err != nil {
			return nil, fmt.Errorf("dropreplace %s: %w", arg, err)
		}
		dropped = append(dropped, r.Old.Path)
	}
	return dropped, nil
}

// discoverAgentDirs returns every directory under root that holds an
// agent.codefly.yaml (including quarantined ones — they still need dep wiring).
func discoverAgentDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() == "agent.codefly.yaml" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	dirs = dedupeStrings(dirs)
	sort.Strings(dirs)
	return dirs, nil
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

func runGo(ctx context.Context, dir string, args ...string) error {
	return runGoEnv(ctx, dir, os.Environ(), args...)
}

func runGoEnv(ctx context.Context, dir string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type fileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func snapshotFiles(paths ...string) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				snapshots = append(snapshots, fileSnapshot{path: path})
				continue
			}
			return nil, fmt.Errorf("snapshot %s: %w", path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}
		snapshots = append(snapshots, fileSnapshot{path: path, data: data, mode: info.Mode().Perm(), exists: true})
	}
	return snapshots, nil
}

func restoreFiles(ctx context.Context, snapshots []fileSnapshot) error {
	var failures []error
	for _, snapshot := range snapshots {
		if !snapshot.exists {
			if err := os.Remove(snapshot.path); err != nil && !os.IsNotExist(err) {
				failures = append(failures, fmt.Errorf("remove newly created %s: %w", snapshot.path, err))
			}
			continue
		}
		if err := shared.WriteFileAtomic(ctx, snapshot.path, snapshot.data, snapshot.mode); err != nil {
			failures = append(failures, fmt.Errorf("restore %s: %w", snapshot.path, err))
		}
	}
	return errors.Join(failures...)
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
}
