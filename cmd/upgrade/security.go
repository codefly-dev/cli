package upgrade

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/services/audit"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/spf13/cobra"
)

// SecurityCmd applies the dependency upgrades that clear actionable govulncheck
// findings — turning the old `scripts/govulncheck.sh` audit-only flow into a
// command that also FIXES. It reuses audit.Golang (the exact analysis
// `codefly agent build` runs), so the fixes it applies are precisely the ones
// the audit reports as actionable (an upstream fix exists):
//
//   - module vulns  → `go get <module>@<fixedVersion>`
//   - stdlib vulns  → bump the go.mod `toolchain` to the fixed Go version
//
// then `go mod tidy` and re-audit to show what (if anything) remains. Unpatched
// upstream findings (no fix yet) are reported but never block — no upgrade can
// clear them.
var SecurityCmd = &cobra.Command{
	Use:   "security",
	Short: "Upgrade dependencies to clear actionable vulnerabilities (govulncheck-driven)",
	Long: `Security runs the same govulncheck audit as ` + "`codefly agent build`" + ` and then
APPLIES the fixes it finds actionable:

  module vulnerabilities → go get <module>@<fixedVersion>
  stdlib  vulnerabilities → bump go.mod toolchain to the fixed Go version

followed by go mod tidy and a re-audit. Unpatched-upstream findings (no fix
available) are reported but never fail the command.

By default it operates on the Go module in the current directory. Use --all to
sweep every Go module in the codefly.dev monorepo (cli, core, wool, and every
agent under agents/services/). Use --dry-run to see the plan without changing
anything.

Examples:
  codefly upgrade security                 # fix the current module
  codefly upgrade security --all           # fix every module in the monorepo
  codefly upgrade security --all --dry-run # preview the monorepo-wide plan`,
	Run: func(cmd *cobra.Command, args []string) {
		all, _ := cmd.Flags().GetBool("all")
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		dir, _ := cmd.Flags().GetString("dir")

		if dir == "" {
			var err error
			dir, err = os.Getwd()
			if err != nil {
				cli.Error("Cannot get working directory: %v", err)
				cli.ExitError()
				return
			}
		}
		absDir, err := filepath.Abs(dir)
		if err != nil {
			cli.Error("Cannot resolve directory: %v", err)
			cli.ExitError()
			return
		}

		var modules []string
		if all {
			root := monorepoRoot(absDir)
			if root == "" {
				cli.Error("--all: not inside the codefly.dev monorepo (no dir with both core/ and wool/ above %s)", absDir)
				cli.ExitError()
				return
			}
			modules = goModules(root)
			if len(modules) == 0 {
				cli.Error("--all: no Go modules found under %s", root)
				cli.ExitError()
				return
			}
			cli.Header(1, "Upgrading %d Go module(s) under %s", len(modules), root)
		} else {
			modules = []string{absDir}
		}

		var failed []string
		for i, m := range modules {
			if len(modules) > 1 {
				cli.Header(2, "[%d/%d] %s", i+1, len(modules), shortPath(m))
			}
			if err := upgradeModuleSecurity(m, dryRun); err != nil {
				cli.Error("  %v", err)
				failed = append(failed, shortPath(m))
			}
		}

		if len(failed) > 0 {
			cli.Error("Security upgrade left actionable findings in %d module(s): %s", len(failed), strings.Join(failed, ", "))
			cli.ExitError()
			return
		}
		if dryRun {
			cli.Header(1, "Dry-run complete — no changes written")
		} else {
			cli.Header(1, "Security upgrade complete — no actionable vulnerabilities remain")
		}
	},
}

func init() {
	SecurityCmd.Flags().Bool("all", false, "Sweep every Go module in the codefly.dev monorepo")
	SecurityCmd.Flags().Bool("dry-run", false, "Show the upgrade plan without applying it")
	SecurityCmd.Flags().String("dir", "", "Module directory (default: current directory)")
}

// upgradeModuleSecurity audits one module, applies the actionable fixes, then
// re-audits. It returns an error if actionable findings remain after the
// upgrade (so --all can report which modules still need a human).
func upgradeModuleSecurity(dir string, dryRun bool) error {
	ctx := context.Background()
	res, err := audit.Golang(ctx, dir, false)
	if err != nil {
		return fmt.Errorf("audit failed: %w", err)
	}
	if res.Tool == "missing" {
		cli.Info("  govulncheck not installed — skipping. Install: go install golang.org/x/vuln/cmd/govulncheck@latest")
		return nil
	}

	modBumps, toolchain, unpatched := planFixes(res.Findings)

	if len(modBumps) == 0 && toolchain == "" {
		if len(unpatched) > 0 {
			cli.Info("  No actionable vulnerabilities. %d unpatched upstream (no fix available): %s", len(unpatched), strings.Join(unpatched, ", "))
		} else {
			cli.Info("  No vulnerabilities found.")
		}
		return nil
	}

	// Report the plan.
	if toolchain != "" {
		cli.Info("  stdlib → bump toolchain to go%s", toolchain)
	}
	for _, b := range modBumps {
		cli.Info("  %s → %s", b.module, b.version)
	}
	if len(unpatched) > 0 {
		cli.Info("  (%d unpatched upstream, no fix available — not blocking: %s)", len(unpatched), strings.Join(unpatched, ", "))
	}

	if dryRun {
		return nil
	}

	// Apply: toolchain first (so go get resolves against the right stdlib).
	if toolchain != "" {
		if err := runGo(dir, "mod", "edit", "-toolchain=go"+toolchain); err != nil {
			return fmt.Errorf("set toolchain go%s: %w", toolchain, err)
		}
	}
	// Apply ALL module bumps in ONE `go get` so minimal-version selection
	// resolves them together and takes the MAX of every constraint. Separate
	// per-module calls let a later bump downgrade an earlier one — e.g.
	// `go get x/sys@v0.44.0` after `x/crypto@v0.52.0` (which needs x/sys
	// v0.45.0) drags x/sys, and then x/crypto, back down.
	if len(modBumps) > 0 {
		getArgs := []string{"get"}
		for _, b := range modBumps {
			getArgs = append(getArgs, b.module+"@"+b.version)
		}
		if err := runGo(dir, getArgs...); err != nil {
			return fmt.Errorf("go get: %w", err)
		}
	}
	if err := runGo(dir, "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}

	// Re-audit to confirm.
	after, err := audit.Golang(ctx, dir, false)
	if err != nil {
		return fmt.Errorf("re-audit failed: %w", err)
	}
	stillActionable, _, _ := planFixes(after.Findings)
	if len(stillActionable) > 0 {
		var ids []string
		for _, b := range stillActionable {
			ids = append(ids, b.module+"@"+b.version)
		}
		return fmt.Errorf("actionable findings remain after upgrade: %s", strings.Join(ids, ", "))
	}
	cli.Info("  ✓ actionable vulnerabilities cleared")
	return nil
}

type moduleBump struct {
	module  string
	version string
}

// planFixes partitions findings into (module bumps, toolchain version, unpatched
// ids). A finding is actionable when it has a FixedVersion. stdlib findings drive
// a single toolchain bump to the highest fixed Go version; module findings each
// become a `go get module@fixedVersion`, deduped to the highest fixed version
// per module.
func planFixes(findings []*builderv0.AuditFinding) (mods []moduleBump, toolchain string, unpatched []string) {
	highestMod := map[string]string{}
	unpatchedSet := map[string]struct{}{}
	for _, f := range findings {
		if f.FixedVersion == "" {
			// govulncheck reports the same vuln across many reachable call
			// sites — dedupe by id so the report is one line per advisory.
			unpatchedSet[f.Id] = struct{}{}
			continue
		}
		if isStdlib(f.Package) {
			v := normalizeGoVersion(f.FixedVersion)
			if goVersionLess(toolchain, v) {
				toolchain = v
			}
			continue
		}
		v := f.FixedVersion
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		if cur, ok := highestMod[f.Package]; !ok || semverLess(cur, v) {
			highestMod[f.Package] = v
		}
	}
	for m, v := range highestMod {
		mods = append(mods, moduleBump{module: m, version: v})
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].module < mods[j].module })
	for id := range unpatchedSet {
		unpatched = append(unpatched, id)
	}
	sort.Strings(unpatched)
	return mods, toolchain, unpatched
}

func isStdlib(pkg string) bool {
	return pkg == "stdlib" || pkg == "toolchain" || pkg == "std"
}

// normalizeGoVersion strips "go"/"v" prefixes so "go1.26.4", "v1.26.4" and
// "1.26.4" all become "1.26.4".
func normalizeGoVersion(s string) string {
	s = strings.TrimPrefix(s, "go")
	s = strings.TrimPrefix(s, "v")
	return s
}

// goVersionLess reports whether a < b for dotted Go versions ("1.26.4").
// An empty a is treated as the lowest.
func goVersionLess(a, b string) bool {
	if a == "" {
		return b != ""
	}
	return compareDotted(a, b) < 0
}

// semverLess reports a < b for "vX.Y.Z" semver (prefix-tolerant).
func semverLess(a, b string) bool {
	return compareDotted(strings.TrimPrefix(a, "v"), strings.TrimPrefix(b, "v")) < 0
}

func compareDotted(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var ai, bi int
		if i < len(as) {
			ai, _ = strconv.Atoi(strings.SplitN(as[i], "-", 2)[0])
		}
		if i < len(bs) {
			bi, _ = strconv.Atoi(strings.SplitN(bs[i], "-", 2)[0])
		}
		if ai != bi {
			if ai < bi {
				return -1
			}
			return 1
		}
	}
	return 0
}

// runGo runs a go command in dir with GOTOOLCHAIN=auto so a bumped toolchain
// directive auto-downloads the required Go.
func runGo(dir string, args ...string) error {
	cli.Info("  $ go %s", strings.Join(args, " "))
	c := exec.Command("go", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	// A generous timeout: toolchain auto-download + module fetch can be slow.
	done := make(chan error, 1)
	if err := c.Start(); err != nil {
		return err
	}
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Minute):
		_ = c.Process.Kill()
		return fmt.Errorf("go %s timed out after 10m", strings.Join(args, " "))
	}
}

// monorepoRoot walks up from dir to the codefly.dev root (the dir containing
// both core/ and wool/), mirroring the agent build's detection.
func monorepoRoot(dir string) string {
	for cur := dir; ; {
		if isDirPresent(filepath.Join(cur, "core")) && isDirPresent(filepath.Join(cur, "wool")) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// goModules returns every directory under root that holds a go.mod, skipping
// vendor/ and testdata/ trees (their go.mod files are fixtures, not real
// modules to upgrade).
func goModules(root string) []string {
	var mods []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "testdata", "node_modules", ".gomodcache", ".git", "out":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "go.mod" {
			mods = append(mods, filepath.Dir(path))
		}
		return nil
	})
	sort.Strings(mods)
	return mods
}

func isDirPresent(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func shortPath(p string) string {
	if root := monorepoRoot(p); root != "" {
		if rel, err := filepath.Rel(root, p); err == nil {
			if rel == "." {
				return filepath.Base(p)
			}
			return rel
		}
	}
	return p
}
