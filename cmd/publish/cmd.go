package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// Cmd is the cobra surface for `codefly publish`. The parent command
// `codefly publish` accepts an optional bump-type positional argument
// (patch | minor | major; default patch) and a few flags. Mode-
// detection is automatic from cwd.
//
// Subcommand `codefly publish re-tag` runs the re-tag flow.
var Cmd = &cobra.Command{
	Use:   "publish [patch|minor|major]",
	Short: "Cut a release for the current codefly repo",
	Long: `publish bumps the manifest version, commits, tags, and pushes
the new tag to origin. One command for every codefly-dev repo —
modules (core, cli, sdk-go) and agents (every services/* and
modules/*) all use the same flow.

The mode is auto-detected from cwd:
  agent.codefly.yaml         → agent
  version/info.codefly.yaml  → core module
  pkg/cli/info.yaml          → cli
  info.codefly.yaml          → standalone module (sdk-go, etc.)

Pre-flight gates (any failure aborts cleanly with no side effects):
  - working tree clean
  - on main branch
  - in sync with origin/main
  - tag doesn't already exist (local or remote)

Push is NEVER --force for main or the tag. If pre-flight fails the
operator must resolve the divergence by hand — refusing to overwrite
in-flight work is the whole point.

Examples:
  codefly publish              # patch bump
  codefly publish minor
  codefly publish major
  codefly publish --dry-run    # show what would happen, change nothing`,
	Args: cobra.MaximumNArgs(1),
	Run:  run,
}

func init() {
	Cmd.Flags().Bool("dry-run", false, "show what would happen without modifying anything")
	Cmd.Flags().String("dir", "", "manifest directory (default: cwd)")
	Cmd.AddCommand(reTagCmd)
}

func run(c *cobra.Command, args []string) {
	bumpType := "patch"
	if len(args) == 1 {
		bumpType = args[0]
	}
	dryRun, _ := c.Flags().GetBool("dry-run")
	dir, _ := c.Flags().GetString("dir")

	manifest, workDir, err := loadManifest(dir)
	if err != nil {
		cli.Error("%v", err)
		cli.ExitError()
	}

	fmt.Printf("==> %s mode (%s); current version %s\n",
		manifest.Mode, manifest.Path, manifest.Version)

	engine := &Engine{
		Manifest: manifest,
		BumpType: bumpType,
		DryRun:   dryRun,
		WorkDir:  workDir,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tag, err := engine.Release(ctx)
	if err != nil {
		cli.Error("publish failed: %v", err)
		cli.ExitError()
	}
	if dryRun {
		fmt.Printf("==> dry-run complete; tag %s NOT created\n", tag)
		return
	}
	fmt.Printf("==> released %s\n", tag)
}

// reTagCmd implements `codefly publish re-tag` — re-cut an existing
// tag at HEAD. Subcommand rather than a flag because the semantics
// are meaningfully different (the only valid force-push path).
var reTagCmd = &cobra.Command{
	Use:   "re-tag",
	Short: "Re-cut the manifest's current tag at HEAD (force-pushes the TAG only, never main)",
	Long: `re-tag deletes the manifest's current tag locally + on origin and
recreates it at HEAD. Used when CI needs a fresh tag-push trigger
without bumping the version.

The TAG is force-pushed (that's the whole point); main is never
touched. Pre-flight checks are the same as publish minus the
"tag doesn't exist" gate (which would invert the meaning here).

If the tag doesn't exist, re-tag refuses and points at plain
publish — first-time releases use the bump path.`,
	Run: runReTag,
}

func init() {
	reTagCmd.Flags().Bool("dry-run", false, "show what would happen without modifying anything")
	reTagCmd.Flags().String("dir", "", "manifest directory (default: cwd)")
}

func runReTag(c *cobra.Command, _ []string) {
	dryRun, _ := c.Flags().GetBool("dry-run")
	dir, _ := c.Flags().GetString("dir")

	manifest, workDir, err := loadManifest(dir)
	if err != nil {
		cli.Error("%v", err)
		cli.ExitError()
	}

	fmt.Printf("==> re-tag %s @ HEAD (%s mode, manifest %s)\n",
		manifest.Tag(), manifest.Mode, manifest.Path)

	engine := &Engine{
		Manifest: manifest,
		DryRun:   dryRun,
		WorkDir:  workDir,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tag, err := engine.ReTag(ctx)
	if err != nil {
		cli.Error("re-tag failed: %v", err)
		cli.ExitError()
	}
	if dryRun {
		fmt.Printf("==> dry-run complete; tag %s NOT moved\n", tag)
		return
	}
	fmt.Printf("==> re-tagged %s\n", tag)
}

// loadManifest resolves the working dir, detects the manifest, and
// returns the workDir to run git from. Centralized so publish + re-tag
// share the same surface.
//
// workDir = the git repo root (manifest's containing repo), found by
// walking up from the manifest path looking for `.git`. We don't
// assume the manifest is at the repo root — core's manifest lives
// at `version/info.codefly.yaml`, several levels deep.
func loadManifest(dirFlag string) (*Manifest, string, error) {
	dir := dirFlag
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return nil, "", fmt.Errorf("read cwd: %w", err)
		}
	}

	manifest, err := Detect(dir)
	if err != nil {
		return nil, "", err
	}

	workDir, err := findGitRoot(filepath.Dir(manifest.Path))
	if err != nil {
		return nil, "", fmt.Errorf("find git root from manifest %s: %w", manifest.Path, err)
	}
	return manifest, workDir, nil
}

// findGitRoot walks upward from start looking for a `.git` directory.
// Returns the first parent that contains it. If no `.git` is found
// before hitting the filesystem root, returns an error — every
// codefly repo is a git repo and a missing `.git` is a real
// configuration problem.
func findGitRoot(start string) (string, error) {
	cur, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if info, err := os.Stat(filepath.Join(cur, ".git")); err == nil && (info.IsDir() || !info.IsDir()) {
			// `.git` can be a directory (regular repo) OR a file
			// (worktree / submodule); both count.
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no .git found walking up from %s", start)
		}
		cur = parent
	}
}
