package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/cmd/audit"
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/cmd/companion"
	"github.com/codefly-dev/cli/pkg/cli"
	coreaudit "github.com/codefly-dev/core/agents/services/audit"
	"github.com/spf13/cobra"
)

// internalModulePrefix marks first-party monorepo modules. `update deps`
// deliberately skips bumping these: their versions are governed by go.work
// (local source) / explicit `codefly agents deps --pin`, not by chasing the
// latest published tag. Bumping them here would fight the workspace.
const internalModulePrefix = "github.com/codefly-dev/"

var (
	depsDir        string
	depsCompanions bool
	depsAudit      bool
	depsStaleDays  int
	depsGo         string
)

// DepsCmd implements `codefly update deps` — the "update all packages"
// command. For every Go module under --dir it bumps outdated EXTERNAL
// dependencies to their latest safe release, runs `go mod tidy`, and
// (by default) re-audits with govulncheck. With --companions it also
// rebuilds the companion Docker images with fresh base layers (--pull),
// which is how upstream base-image CVEs (e.g. the Go stdlib bumps in the
// go companion) get cleared.
//
// Replaces the ad-hoc "go get -u / rebuild images" bash flow.
var DepsCmd = &cobra.Command{
	Use:   "deps",
	Short: "Update outdated dependencies across all Go modules (and optionally companion images)",
	Long: `Update dependencies across the monorepo.

For each Go module under --dir (default: current directory), update deps:
  - finds outdated EXTERNAL deps (govulncheck/go list -u)
  - go get <pkg>@<latest-safe> for each (first-party codefly-dev modules
    are left to go.work / 'codefly agents deps --pin')
  - go mod tidy
  - re-audit with govulncheck (unless --audit=false)

With --companions, also rebuilds every companion image with 'docker build
--pull' so floating base tags (golang:1.26-alpine, …) pick up upstream
patch releases — clearing base-image CVEs like the Go stdlib advisories.

Examples:
  codefly update deps                 # update Go deps under cwd + audit
  codefly update deps --dir .         # same, explicit
  codefly update deps --companions    # also rebuild companion images (--pull)
  codefly update deps --audit=false   # skip the post-update audit`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		dir := depsDir
		if dir == "" {
			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("cannot read working directory: %w", err)
			}
			dir = wd
		}
		root, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("cannot resolve --dir: %w", err)
		}

		modDirs, err := findGoModDirs(root)
		if err != nil {
			return fmt.Errorf("cannot enumerate Go modules: %w", err)
		}
		if len(modDirs) == 0 {
			return fmt.Errorf("no go.mod found under %s", root)
		}
		cli.Header(1, "Updating dependencies across %d module(s)", len(modDirs))

		var failures []error

		// Toolchain bump first: standalone/CI builds (GOWORK=off) read the
		// go.mod toolchain directive, so a stdlib CVE (e.g. the Go 1.26.x
		// advisories) is only cleared once every module pins a fixed
		// toolchain. go.work files are bumped too for workspace builds.
		if depsGo != "" {
			tc := "go" + strings.TrimPrefix(depsGo, "go")
			cli.Info("Pinning toolchain → %s", tc)
			for _, md := range modDirs {
				if err := runGo(ctx, md, "mod", "edit", "-toolchain="+tc); err != nil {
					cli.Error("set toolchain %s in %s: %v", tc, rel(root, md), err)
					failures = append(failures, fmt.Errorf("set toolchain %s in %s: %w", tc, rel(root, md), err))
				}
			}
			workFiles, walkErr := findGoWorkFiles(root)
			if walkErr != nil {
				return fmt.Errorf("cannot enumerate Go workspaces: %w", walkErr)
			}
			for _, wf := range workFiles {
				if err := runGo(ctx, filepath.Dir(wf), "work", "edit", "-toolchain", tc, wf); err != nil {
					cli.Error("set toolchain %s in %s: %v", tc, rel(root, wf), err)
					failures = append(failures, fmt.Errorf("set toolchain %s in %s: %w", tc, rel(root, wf), err))
				}
			}
		}

		for _, md := range modDirs {
			if err := ctx.Err(); err != nil {
				failures = append(failures, err)
				break
			}
			if err := updateModule(ctx, md); err != nil {
				cli.Error("update %s: %v", rel(root, md), err)
				failures = append(failures, fmt.Errorf("update %s: %w", rel(root, md), err))
			}
		}
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			return errors.Join(failures...)
		}

		if depsCompanions {
			cli.Header(1, "Rebuilding companion images (--pull)")
			coreDir := companion.FindCompanionsRoot(root)
			if err := companion.BuildAll(coreDir, companion.BuildOptions{Pull: true}); err != nil {
				cli.Error("rebuild companions: %v", err)
				failures = append(failures, fmt.Errorf("rebuild companions: %w", err))
			}
		}

		blocked := false
		if depsAudit {
			for _, md := range modDirs {
				if err := ctx.Err(); err != nil {
					failures = append(failures, err)
					break
				}
				b, err := audit.RunGoAudit(ctx, md, depsStaleDays, true)
				if err != nil {
					cli.Error("audit %s: %v", rel(root, md), err)
					failures = append(failures, fmt.Errorf("audit %s: %w", rel(root, md), err))
				}
				blocked = blocked || b
			}
		}

		if blocked {
			failures = append(failures, fmt.Errorf("dependency audit remains blocked"))
		}
		if len(failures) > 0 {
			return errors.Join(failures...)
		}
		cli.Header(1, "Dependencies up to date")
		return ctx.Err()
	},
}

// updateModule bumps every outdated external dependency in the module at
// dir to its latest safe version, then tidies. Internal codefly-dev
// modules are skipped (see internalModulePrefix).
func updateModule(ctx context.Context, dir string) error {
	res, err := coreaudit.Golang(ctx, dir, true)
	if err != nil {
		return fmt.Errorf("list outdated: %w", err)
	}
	var bumped int
	for _, o := range res.Outdated {
		if strings.HasPrefix(o.Package, internalModulePrefix) {
			continue
		}
		if o.LatestSafe == "" || o.LatestSafe == o.Current {
			continue
		}
		if err := runGo(ctx, dir, "get", o.Package+"@"+o.LatestSafe); err != nil {
			return fmt.Errorf("go get %s@%s: %w", o.Package, o.LatestSafe, err)
		}
		cli.Info("  %s %s → %s", o.Package, o.Current, o.LatestSafe)
		bumped++
	}
	// `go mod tidy` ignores go.work: it resolves every import from the
	// module graph (cache/network), not from workspace-local sibling
	// modules. So in a workspace a module that imports an as-yet-unpublished
	// sibling package (e.g. freshly generated code in ../core) can't be
	// tidied until that sibling is published. Use `tidy -e` there so the
	// dependency bump still completes; go.work keeps builds working.
	if inWorkspace(dir) {
		// Capture stderr rather than stream it: tidy -e prints alarming
		// "finding module for package … does not contain package" lines for
		// every unpublished workspace-local import even though it succeeds.
		// Surface that detail only if tidy actually fails.
		if out, err := runGoCapture(ctx, dir, "mod", "tidy", "-e"); err != nil {
			return fmt.Errorf("go mod tidy -e: %w\n%s", err, out)
		}
		cli.Warning("  %s: tidied with -e (workspace) — unpublished sibling packages aren't pruned; publish them to fully tidy", filepath.Base(dir))
	} else if err := runGo(ctx, dir, "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	if bumped == 0 {
		cli.Info("  %s: already up to date", filepath.Base(dir))
	} else {
		cli.Info("  %s: bumped %d dependency(ies)", filepath.Base(dir), bumped)
	}
	return nil
}

// runGo runs `go <args>` in dir, streaming output. Uses the ambient
// environment (go.work stays on) so first-party modules keep resolving
// against local source while external deps update.
func runGo(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runGoCapture runs `go <args>` in dir capturing combined output instead of
// streaming it. Used for `tidy -e`, whose stderr is noisy-but-harmless on
// success; callers surface the output only on failure.
func runGoCapture(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

// findGoModDirs returns every directory under root with a go.mod, skipping
// vendor, node_modules, .git and testdata trees.
func findGoModDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			dirs = append(dirs, filepath.Dir(p))
		}
		return nil
	})
	sort.Strings(dirs)
	return dirs, err
}

// inWorkspace reports whether dir is covered by a go.work (walking up).
// Used to choose `go mod tidy -e` so a workspace-local unpublished import
// doesn't fail the dependency bump.
func inWorkspace(dir string) bool {
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, "go.work")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

// findGoWorkFiles returns every go.work under root, skipping vendor,
// node_modules and .git. Used by --go to bump the toolchain for workspace
// builds in addition to the per-module go.mod directive.
func findGoWorkFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.work" {
			files = append(files, p)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}

func init() {
	DepsCmd.Flags().StringVar(&depsDir, "dir", "", "Root to update (default: current directory; recurses into every go.mod)")
	DepsCmd.Flags().BoolVar(&depsCompanions, "companions", false, "Also rebuild companion Docker images with fresh base layers (docker build --pull)")
	DepsCmd.Flags().BoolVar(&depsAudit, "audit", true, "Run the govulncheck audit after updating")
	DepsCmd.Flags().IntVar(&depsStaleDays, "stale-days", 45, "Audit: fail when a suppression's reviewed date is older than this many days (0 disables)")
	DepsCmd.Flags().StringVar(&depsGo, "go", "", "Pin the Go toolchain to this version across every go.mod + go.work (e.g. 1.26.4) — clears stdlib CVEs in standalone/CI builds")
}
