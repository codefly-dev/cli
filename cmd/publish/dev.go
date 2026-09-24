package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// devCmd implements `codefly publish dev` — build and publish an agent under a
// non-semantic dev version, for iteration, without releasing it.
var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Publish an agent build for iteration, without a release",
	Long: `dev builds the agent in the current repository exactly as ` + "`codefly publish`" + `
builds it (release-grade agent CI, the same loader archives and SBOMs) and
publishes it under a dev version derived from the commit:

  <current-version>-dev.<12-char-sha>     e.g. 0.1.47-dev.abc123def456

It runs on any branch and bumps nothing: agent.codefly.yaml keeps its version,
no release pull request is opened, and main is never touched. The build is
published where release assets go, as a GitHub PRERELEASE that is never marked
Latest, under the tag v<dev-version> pointing at HEAD. Semver ranks a prerelease
below its release, so a dev build can never collide with or outrank one, and
` + "`version: latest`" + ` never resolves to it.

A dirty working tree is refused unless --allow-dirty is given; the build then
carries changes the tagged commit does not, and publishing different bytes
again under the same commit is refused (published assets are immutable) —
commit to get a new dev version. An agent whose releases are published by a
workflow (release.owner: workflow) is built by that workflow from the tag, so
--allow-dirty is refused for it, and its GoReleaser configuration must publish
prerelease tags as prereleases (release.prerelease: auto).

Dev builds are for iteration only. ` + "`codefly publish patch`" + ` remains the release path.`,
	Args: cobra.NoArgs,
	RunE: runDev,
}

func init() {
	devCmd.Flags().Bool("allow-dirty", false, "publish from a working tree with uncommitted changes")
	devCmd.Flags().Bool("dry-run", false, "print the dev version without building or publishing")
	devCmd.Flags().String("dir", "", "agent directory (default: cwd)")
	Cmd.AddCommand(devCmd)
}

// devPublishBudget bounds a dev publish: release-grade CI plus, for a
// workflow-owned agent, the workflow run.
const devPublishBudget = 45 * time.Minute

func runDev(c *cobra.Command, _ []string) error {
	allowDirty, _ := c.Flags().GetBool("allow-dirty")
	dryRun, _ := c.Flags().GetBool("dry-run")
	dir, _ := c.Flags().GetString("dir")

	manifest, workDir, err := loadManifest(dir)
	if err != nil {
		return err
	}
	if manifest.Mode != ModeAgent {
		return fmt.Errorf("publish dev publishes agent builds; %s is a %s manifest", manifest.Path, manifest.Mode)
	}
	agentDir := filepath.Dir(manifest.Path)
	identity, err := readAgentIdentity(manifest.Path)
	if err != nil {
		return err
	}
	if !loaderAssetKinds[identity.Kind] {
		return fmt.Errorf("publish dev publishes loader assets; a %s agent is consumed from its source tag, so it has no dev build", identity.Kind)
	}
	if precondErr := checkAgentReleasePreconditionsForManifest(manifest.Path); precondErr != nil {
		return precondErr
	}
	if identity.Release.Owner == publicationWorkflow {
		if allowDirty {
			return fmt.Errorf("release.owner is workflow: %s builds from the pushed tag, so uncommitted changes can never reach the build; commit them instead of --allow-dirty", identity.Release.Workflow)
		}
		if configErr := checkWorkflowPublishesPrereleases(agentDir); configErr != nil {
			return configErr
		}
	}

	fmt.Printf("==> dev build of %s/%s (current version %s)\n", identity.Publisher, identity.Name, manifest.Version)
	engine := &Engine{Manifest: manifest, DryRun: dryRun, WorkDir: workDir}
	if !dryRun {
		releaser, rerr := newAgentReleaser(agentDir)
		if rerr != nil {
			return rerr
		}
		defer releaser.cleanup()
		releaser.dev = true
		releaser.attach(engine)
	}

	ctx, cancel := context.WithTimeout(context.Background(), devPublishBudget)
	defer cancel()
	tag, err := engine.PublishDev(ctx, allowDirty)
	if err != nil {
		return fmt.Errorf("publish dev failed: %w", err)
	}
	version := strings.TrimPrefix(tag, "v")
	if dryRun {
		fmt.Printf("==> dry-run complete; %s NOT built or published\n", version)
	} else {
		fmt.Printf("==> published dev build %s (GitHub prerelease %s, never Latest)\n", version, tag)
	}
	fmt.Print(devConsumptionHint(identity.Publisher, identity.Name, version))
	return nil
}

// devConsumptionHint prints the exact spellings that consume a dev build.
func devConsumptionHint(publisher, name, version string) string {
	return fmt.Sprintf(`
Use it in a workspace (workspace.codefly.yaml):
  agent-overrides:
    %s/%s: %s
or pin one service (service.codefly.yaml):
  agent:
    publisher: %s
    name: %s
    version: %s
Dev builds are for iteration only; release with `+"`codefly publish patch`"+`.
`, publisher, name, version, publisher, name, version)
}

// PublishDev publishes the build at HEAD under its dev version and returns the
// tag. Unlike Release it bumps nothing, commits nothing and never touches main:
// the only ref it creates is the dev tag, pushed without --force.
//
// BeforeCommit builds against a working tree whose manifest carries the dev
// version — as a release builds against the bumped manifest — and the manifest
// is restored byte-for-byte right after, so the agent's version never moves.
func (e *Engine) PublishDev(ctx context.Context, allowDirty bool) (string, error) {
	if !allowDirty {
		if err := e.assertCleanTree(ctx); err != nil {
			return "", fmt.Errorf("%w\n(or pass --allow-dirty to publish the working tree as it is)", err)
		}
	}
	head, err := e.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	version, err := DevVersion(e.Manifest.Version, strings.TrimSpace(head))
	if err != nil {
		return "", err
	}
	tag := "v" + version
	if e.DryRun {
		fmt.Printf("[dry-run] would build and publish %s as prerelease %s at HEAD\n", version, tag)
		return tag, nil
	}

	if e.BeforeCommit != nil {
		if buildErr := e.buildAtVersion(ctx, version, tag); buildErr != nil {
			return "", buildErr
		}
	}
	if !e.tagExistsLocally(ctx, tag) {
		if tagErr := e.gitTag(ctx, tag); tagErr != nil {
			return "", fmt.Errorf("tag: %w", tagErr)
		}
	}
	published, err := e.tagExistsRemotely(ctx, tag)
	if err != nil {
		return "", err
	}
	if !published {
		if err := e.pushTag(ctx, tag); err != nil {
			return "", err
		}
	}
	if e.AfterPush != nil {
		if err := e.AfterPush(ctx, tag); err != nil {
			return tag, err
		}
	}
	return tag, nil
}

// buildAtVersion runs BeforeCommit with the manifest transiently at version,
// then restores the manifest's exact bytes — a dirty manifest included.
func (e *Engine) buildAtVersion(ctx context.Context, version, tag string) (err error) {
	info, err := os.Stat(e.Manifest.Path)
	if err != nil {
		return err
	}
	original, err := os.ReadFile(e.Manifest.Path) //nolint:gosec // the detected manifest path
	if err != nil {
		return err
	}
	current := e.Manifest.Version
	defer func() {
		e.Manifest.Version = current
		//nolint:gosec // G703: restores the detected manifest's own bytes in place
		if restoreErr := os.WriteFile(e.Manifest.Path, original, info.Mode().Perm()); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore %s: %w", e.Manifest.Path, restoreErr))
		}
	}()
	devVersion, err := parseDevVersion(version)
	if err != nil {
		return err
	}
	if err := e.Manifest.WriteVersion(devVersion); err != nil {
		return fmt.Errorf("write dev version: %w", err)
	}
	return e.BeforeCommit(ctx, tag)
}

// checkWorkflowPublishesPrereleases refuses a dev build of a workflow-owned
// agent whose GoReleaser configuration would publish the dev tag as a full
// release. The workflow, not this command, creates that release, so the only
// way to keep a dev build from becoming Latest is for the workflow's own
// configuration to mark a prerelease tag as a prerelease. Checked before any
// side effect.
func checkWorkflowPublishesPrereleases(agentDir string) error {
	for _, name := range []string{".goreleaser.yaml", ".goreleaser.yml"} {
		raw, err := os.ReadFile(filepath.Join(agentDir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		var config struct {
			Release struct {
				Prerelease string `yaml:"prerelease"`
			} `yaml:"release"`
		}
		if err := yaml.Unmarshal(raw, &config); err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		switch strings.TrimSpace(config.Release.Prerelease) {
		case "auto", "true":
			return nil
		}
		return fmt.Errorf("%s would publish a dev tag as a full release (and mark it Latest); set `release: {prerelease: auto}` in it before publishing dev builds", name)
	}
	return fmt.Errorf("release.owner is workflow but no .goreleaser.yaml says how it publishes a prerelease tag; a dev build cannot be kept off Latest")
}

// assertDevRelease confirms a workflow-published dev build landed as a
// prerelease. A prerelease cannot be Latest, so this is the never-Latest check
// for a release this command did not create itself.
func assertDevRelease(ctx context.Context, client *github.Client, owner, repo, tag string) error {
	release, _, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, tag)
	if err != nil {
		return fmt.Errorf("read dev release %s: %w", tag, err)
	}
	if !release.GetPrerelease() {
		return fmt.Errorf("the release workflow published dev build %s as a full release; mark it a prerelease on GitHub now and fix the workflow's GoReleaser configuration", tag)
	}
	return nil
}
