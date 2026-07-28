package publish

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// allCmd implements `codefly publish all` — the workspace-wide release.
// Discovers every git repo under the workspace that carries a codefly
// manifest and runs the SAME gated publish flow as plain `codefly
// publish` on each, in dependency order.
//
// Replaces the old "cd into each repo and run tag.sh" ritual: one
// command for core + cli + sdk + every agent.
var allCmd = &cobra.Command{
	Use:   "all [patch|minor|major]",
	Short: "Release every Codefly repository in dependency order",
	Long: `all discovers every git repo under the workspace carrying a codefly
manifest (agent.codefly.yaml | version/info.codefly.yaml | pkg/cli/info.yaml
| info.codefly.yaml) and runs the same flow as plain ` + "`codefly publish`" + `
on each.

Order is core → cli → standalone modules → agents, so a dependency is
released before the consumers that pin it.

Safety — the whole run is atomic at the pre-flight boundary:
  1. EVERY repo is validated first (clean tree, on main, in sync with
     origin, target tag free; service agents also: host can build every loader
     platform and gh is available). If ANY repo fails, the run aborts
     before a single tag is pushed.
  2. Only once all repos pass does it publish, sequentially, stopping at
     the first real failure (and reporting what already shipped).

Release-grade agent CI is the publish gate itself, so it runs per-agent
during step 2 — a CI failure there aborts the run with earlier repos
already shipped, exactly like any other execute-phase failure.

As with plain publish, the tag/main push is NEVER --force.

Examples:
  codefly publish all              # patch-bump every repo
  codefly publish all minor
  codefly publish all --dry-run    # print the full plan, change nothing
  codefly publish all --root DIR   # workspace root (default: nearest go.work, else cwd)`,
	Args: cobra.MaximumNArgs(1),
	RunE: runAll,
}

func init() {
	allCmd.Flags().Bool("dry-run", false, "print the full plan without modifying anything")
	allCmd.Flags().String("root", "", "workspace root to scan (default: nearest go.work ancestor, else cwd)")
	Cmd.AddCommand(allCmd)
}

// repoTarget is a discovered release repo: its git root + resolved manifest.
type repoTarget struct {
	Dir        string
	Manifest   *Manifest
	PlannedTag string
}

// engine builds a fresh Engine for one phase. We clone the manifest's
// version because Engine.Release mutates it in place when reconciling
// the bump base against the latest tag — a validate pass must not leak
// that mutation into the execute pass.
func (t *repoTarget) engine(bump string, dry bool) *Engine {
	ver := *t.Manifest.Version
	return &Engine{
		Manifest: &Manifest{Mode: t.Manifest.Mode, Path: t.Manifest.Path, Version: &ver},
		BumpType: bump,
		DryRun:   dry,
		WorkDir:  t.Dir,
	}
}

func runAll(c *cobra.Command, args []string) error {
	bumpType := "patch"
	if len(args) == 1 {
		bumpType = args[0]
	}
	dryRun, _ := c.Flags().GetBool("dry-run")
	rootFlag, _ := c.Flags().GetString("root")

	root, err := resolveWorkspaceRoot(rootFlag)
	if err != nil {
		return err
	}

	targets, err := discoverRepos(root)
	if err != nil {
		return fmt.Errorf("discover repos under %s: %w", root, err)
	}
	if len(targets) == 0 {
		return fmt.Errorf("no codefly manifest repos found under %s", root)
	}
	sortTargets(targets)

	fmt.Printf("==> %d codefly repo(s) under %s (bump: %s)\n", len(targets), root, bumpType)
	for _, t := range targets {
		fmt.Printf("    %-18s %s (v%s)\n", t.Manifest.Mode, relOrBase(root, t.Dir), t.Manifest.Version)
	}

	// Phase 1 — validate EVERY repo. A dry-run Release runs the full
	// pre-flight (clean tree, on main, synced) + bump-base reconciliation
	// + tag-availability check, mutating nothing. Collect ALL failures so
	// the operator fixes them in one pass, then abort if any.
	fmt.Println("==> validating all repos (pre-flight, no changes)...")
	var failures []string
	for _, t := range targets {
		// Gate agent releases on host/gh readiness before any real push, so
		// an incapable host aborts the whole run rather than shipping some
		// repos then failing at an agent. Skipped for --dry-run, which is a
		// pure plan preview and never pushes (matching single publish).
		if t.Manifest.Mode == ModeAgent && !dryRun {
			if perr := checkAgentReleasePreconditionsForManifest(t.Manifest.Path); perr != nil {
				failures = append(failures, fmt.Sprintf("  %-30s %v", relOrBase(root, t.Dir), perr))
				continue
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		tag, verr := t.engine(bumpType, true).Release(ctx)
		cancel()
		if verr != nil {
			failures = append(failures, fmt.Sprintf("  %-30s %v", relOrBase(root, t.Dir), verr))
			continue
		}
		t.PlannedTag = tag
		fmt.Printf("    ✓ %-30s → %s\n", relOrBase(root, t.Dir), tag)
	}
	if len(failures) > 0 {
		return fmt.Errorf("pre-flight failed for %d repo(s) — nothing was pushed:\n%s",
			len(failures), strings.Join(failures, "\n"))
	}

	if dryRun {
		fmt.Printf("==> dry-run complete; %d repo(s) ready, no tags created\n", len(targets))
		return nil
	}

	// Phase 2 — execute in dependency order. Every repo already passed
	// pre-flight; a failure here is a genuine push error, so stop and
	// report what already shipped (those tags are live and cannot be
	// silently rolled back).
	fmt.Println("==> all repos validated; publishing...")
	var done []string
	for _, t := range targets {
		engine := t.engine(bumpType, false)

		// Agent repos additionally run release-grade CI and upload
		// loader-compatible release assets — both far slower than a bare
		// tag push, so they get a generous timeout.
		timeout := 120 * time.Second
		var releaser releaseGate
		if t.Manifest.Mode == ModeAgent {
			releaser, err = newAgentReleaseGate(filepath.Dir(t.Manifest.Path), t.Dir)
			if err != nil {
				return fmt.Errorf("prepare agent release for %s: %w", relOrBase(root, t.Dir), err)
			}
			releaser.attach(engine)
			timeout = 30 * time.Minute
		}

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		tag, rerr := engine.Release(ctx)
		cancel()
		if releaser != nil {
			releaser.cleanup()
		}
		if rerr != nil {
			return fmt.Errorf("publish failed at %s: %w\n  already released: %s",
				relOrBase(root, t.Dir), rerr, strings.Join(done, ", "))
		}
		done = append(done, relOrBase(root, t.Dir)+" "+tag)
		fmt.Printf("    ✓ released %-30s %s\n", relOrBase(root, t.Dir), tag)
	}
	fmt.Printf("==> published %d repo(s)\n", len(done))
	return nil
}

// resolveWorkspaceRoot returns the dir to scan. An explicit --root wins;
// otherwise we walk up from cwd to the nearest go.work (the codefly-dev
// workspace root), falling back to cwd when none is found.
func resolveWorkspaceRoot(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("read cwd: %w", err)
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd, nil
		}
		dir = parent
	}
}

// scanSkip names directories we never descend into: VCS internals, build
// output, and the example/demo/tutorial trees (which carry their own
// nested project repos and node_modules but are NOT release targets).
var scanSkip = map[string]bool{
	".git": true, "node_modules": true, "target": true, "dist": true,
	".next": true, "out": true, "vendor": true, ".cache": true,
	"generated": true, "build": true,
	"examples": true, "examples_old": true, "examples-infrastructure": true,
	"demos": true, "tutorials": true, "landing": true, "signoz": true,
	"googleapis": true, "protovalidate": true,
}

// discoverRepos walks the workspace and returns every git repo carrying a
// codefly manifest. It descends only through NON-repo directories: the
// first `.git` it meets marks a repo boundary, so it tries Detect there
// and never recurses inside (codefly release repos are never nested).
func discoverRepos(root string) ([]*repoTarget, error) {
	var targets []*repoTarget
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // tolerate unreadable dirs — skip, don't abort the whole scan
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && scanSkip[d.Name()] {
			return fs.SkipDir
		}
		// A `.git` entry marks a repo root. Try to detect a codefly
		// manifest; either way, don't recurse past a repo boundary.
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			if m, derr := Detect(path); derr == nil {
				targets = append(targets, &repoTarget{Dir: path, Manifest: m})
			}
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return targets, nil
}

// modeRank orders the release so a dependency ships before its consumers:
// core first, then cli, then standalone modules (sdk), then agents.
func modeRank(m Mode) int {
	switch m {
	case ModeCoreModule:
		return 0
	case ModeCLI:
		return 1
	case ModeStandaloneModule:
		return 2
	case ModeAgent:
		return 3
	default:
		return 4
	}
}

func sortTargets(ts []*repoTarget) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ri, rj := modeRank(ts[i].Manifest.Mode), modeRank(ts[j].Manifest.Mode); ri != rj {
			return ri < rj
		}
		return ts[i].Dir < ts[j].Dir
	})
}

// relOrBase renders a repo path relative to the scan root for readable
// output, falling back to the base name if it isn't under root.
func relOrBase(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil {
		return rel
	}
	return filepath.Base(p)
}
