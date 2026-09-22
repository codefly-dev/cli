package publish

import (
	"context"
	"crypto/sha256"
	"errors"
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
	// The tag is live and the workflow has published. A deadline consumed by
	// the wait above must not turn a release that shipped into one reported as
	// failed, so what follows runs on a budget of its own.
	ctx, cancel := postPublicationContext(ctx)
	defer cancel()
	if err := verifyWorkflowRelease(ctx, client, owner, repo, tag, r.assets); err != nil {
		return err
	}
	return verifyReleaseAssets(ctx, r.reg, r.publisher, r.name, strings.TrimPrefix(tag, "v"), r.assets)
}

// releaseVerifyBudget sizes the work that follows a live tag: downloading every
// loader archive, not a cleanup call.
const releaseVerifyBudget = 10 * time.Minute

func postPublicationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), releaseVerifyBudget)
}

// verifyWorkflowRelease confirms the release the owner workflow published
// carries every loader archive, with the bytes GitHub recorded for it. The
// workflow is the sole publisher of those assets, so every call here reads:
// nothing is created, replaced or deleted.
//
// The archive is the contract — core's downloader fetches exactly that name.
// Whether a workflow also publishes an SBOM beside it, and under what name, is
// its own GoReleaser configuration and nothing the manifest declares, so one is
// verified when the release carries it and never demanded.
func verifyWorkflowRelease(ctx context.Context, client *github.Client, owner, repo, tag string, staged []loaderAsset) error {
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
	for _, candidate := range staged {
		archiveName := filepath.Base(candidate.archivePath)
		archive := assets[archiveName]
		if archive == nil {
			return fmt.Errorf("workflow release %s is missing loader archive %s", tag, archiveName)
		}
		if err := downloadAndVerifyAsset(ctx, client, owner, repo, archive); err != nil {
			return err
		}
		if sbom, ok := assets[archiveName+".sbom.json"]; ok {
			if err := downloadAndVerifyAsset(ctx, client, owner, repo, sbom); err != nil {
				return err
			}
		}
	}
	return nil
}

// downloadAndVerifyAsset re-reads a freshly published asset before giving up.
// GitHub can take a moment to serve an asset it has just accepted, and the tag
// naming this release is already live, so a transient read must not be reported
// as a failed publication. A verdict on the asset's own metadata or bytes
// cannot change on a re-read and is returned immediately.
func downloadAndVerifyAsset(ctx context.Context, client *github.Client, owner, repo string, asset *github.ReleaseAsset) error {
	const attempts = 5
	var lastErr error
	for attempt := range attempts {
		err := readPublishedAsset(ctx, client, owner, repo, asset)
		if err == nil {
			return nil
		}
		if errors.Is(err, errAssetVerdict) {
			return err
		}
		lastErr = err
		if attempt == attempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return lastErr
}

func readPublishedAsset(ctx context.Context, client *github.Client, owner, repo string, asset *github.ReleaseAsset) error {
	stream, _, err := client.Repositories.DownloadReleaseAsset(ctx, owner, repo, asset.GetID(), http.DefaultClient)
	if err != nil {
		return fmt.Errorf("download published asset %s: %w", asset.GetName(), err)
	}
	err = verifyPublishedAsset(stream, asset)
	closeErr := stream.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// errAssetVerdict marks a failure that re-reading the asset cannot change: its
// recorded metadata or its published bytes are wrong. A transport failure
// carries no such marker and is retried.
var errAssetVerdict = errors.New("published asset failed verification")

func verifyPublishedAsset(stream io.Reader, asset *github.ReleaseAsset) error {
	if asset.GetSize() <= 0 || !strings.HasPrefix(asset.GetDigest(), "sha256:") {
		return fmt.Errorf("%w: %s has no verifiable size and SHA-256 digest", errAssetVerdict, asset.GetName())
	}
	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(stream, int64(asset.GetSize())+1))
	if err != nil {
		return err
	}
	if count != int64(asset.GetSize()) || fmt.Sprintf("sha256:%x", digest.Sum(nil)) != asset.GetDigest() {
		return fmt.Errorf("%w: %s does not match its recorded size and digest", errAssetVerdict, asset.GetName())
	}
	return nil
}
