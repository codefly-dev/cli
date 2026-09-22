package publish

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v89/github"
)

const (
	publicationCLI      = "cli"
	publicationWorkflow = "workflow"
	checkSuccess        = "success"
)

type agentPublication struct {
	Owner    string `yaml:"owner"`
	Workflow string `yaml:"workflow,omitempty"`
}

func (p agentPublication) validate(root string) error {
	switch p.Owner {
	case publicationCLI:
		if p.Workflow != "" {
			return fmt.Errorf("release.workflow conflicts with release.owner: cli")
		}
	case publicationWorkflow:
		if filepath.Base(p.Workflow) != p.Workflow || (filepath.Ext(p.Workflow) != ".yml" && filepath.Ext(p.Workflow) != ".yaml") {
			return fmt.Errorf("release.workflow must name a workflow file inside .github/workflows")
		}
		info, err := os.Lstat(filepath.Join(root, ".github", "workflows", p.Workflow))
		if err != nil {
			return fmt.Errorf("read release workflow: %w", err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release workflow must be a regular file")
		}
	default:
		return fmt.Errorf("executable agent releases require explicit release.owner: workflow or cli; competing publishers are forbidden")
	}
	return nil
}

// Match the tag event as well as the commit: a green branch run at the same
// commit does not establish that the tag's publication jobs ran.
func workflowReleaseReady(runs []*github.WorkflowRun, head, tag string) (bool, error) {
	var selected *github.WorkflowRun
	for _, run := range runs {
		if run.GetHeadSHA() == head && run.GetHeadBranch() == tag && run.GetEvent() == "push" && (selected == nil || run.GetID() > selected.GetID()) {
			selected = run
		}
	}
	if selected == nil || selected.GetStatus() != "completed" {
		return false, nil
	}
	if selected.GetConclusion() != checkSuccess {
		return false, fmt.Errorf("release workflow %s concluded %s", selected.GetHTMLURL(), selected.GetConclusion())
	}
	return true, nil
}

func (r *agentReleaser) waitForWorkflowRelease(ctx context.Context, client *github.Client, owner, repo, tag string) error {
	engine := &Engine{WorkDir: r.agentDir}
	output, err := engine.git(ctx, "rev-parse", "--verify", tag+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve published tag commit: %w", err)
	}
	head := strings.TrimSpace(output)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		runs, _, readErr := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, r.publication.Workflow, &github.ListWorkflowRunsOptions{
			HeadSHA: head, Branch: tag, Event: "push", ListOptions: github.ListOptions{PerPage: 100},
		})
		if readErr != nil {
			return fmt.Errorf("read release workflow: %w", readErr)
		}
		ready, workflowErr := workflowReleaseReady(runs.WorkflowRuns, head, tag)
		if workflowErr != nil {
			return workflowErr
		}
		if ready {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s at %s: %w", r.publication.Workflow, tag, ctx.Err())
		case <-ticker.C:
		}
	}
	release, _, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, tag)
	if err != nil {
		return fmt.Errorf("read workflow-published release: %w", err)
	}
	if release.GetTagName() != tag || release.GetDraft() {
		return fmt.Errorf("workflow did not publish release %s", tag)
	}
	assets := make(map[string]*github.ReleaseAsset)
	for _, asset := range release.Assets {
		assets[asset.GetName()] = asset
	}
	for _, staged := range r.assets {
		archiveName := filepath.Base(staged.archivePath)
		for _, name := range []string{archiveName, archiveName + ".sbom.json"} {
			asset := assets[name]
			if asset == nil {
				return fmt.Errorf("workflow release %s is missing loader archive %s", tag, name)
			}
			stream, _, err := client.Repositories.DownloadReleaseAsset(ctx, owner, repo, asset.GetID(), http.DefaultClient)
			if err != nil {
				return fmt.Errorf("download workflow archive %s: %w", name, err)
			}
			err = verifyPublishedAsset(stream, asset)
			closeErr := stream.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return verifyReleaseAssets(ctx, r.reg, r.publisher, r.name, strings.TrimPrefix(tag, "v"), r.assets)
}

func verifyPublishedAsset(stream io.Reader, asset *github.ReleaseAsset) error {
	if asset.GetSize() <= 0 || !strings.HasPrefix(asset.GetDigest(), "sha256:") {
		return fmt.Errorf("published asset %s has no verifiable size and SHA-256 digest", asset.GetName())
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(stream, int64(asset.GetSize())+1))
	if err != nil {
		return err
	}
	if count != int64(asset.GetSize()) || fmt.Sprintf("sha256:%x", digest.Sum(nil)) != asset.GetDigest() {
		return fmt.Errorf("published asset %s does not match its recorded size and digest", asset.GetName())
	}
	return nil
}
