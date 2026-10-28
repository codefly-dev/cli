package publish

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	runCompleted        = "completed"
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
	ready, _, err := workflowReleaseState(runs, head, tag)
	return ready, err
}

// workflowReleaseState also reports whether the tag's run exists at all, which
// is what tells a release workflow still running from one that never started.
func workflowReleaseState(runs []*github.WorkflowRun, head, tag string) (ready, started bool, err error) {
	var selected *github.WorkflowRun
	for _, run := range runs {
		if run.GetHeadSHA() == head && run.GetHeadBranch() == tag && run.GetEvent() == "push" && (selected == nil || run.GetID() > selected.GetID()) {
			selected = run
		}
	}
	if selected == nil {
		return false, false, nil
	}
	if selected.GetStatus() != runCompleted {
		return false, true, nil
	}
	if selected.GetConclusion() != checkSuccess {
		return false, true, fmt.Errorf("release workflow %s concluded %s", selected.GetHTMLURL(), selected.GetConclusion())
	}
	return true, true, nil
}

func (r *agentReleaser) waitForWorkflowRelease(ctx context.Context, client *github.Client, owner, repo, tag string) error {
	engine := &Engine{WorkDir: r.agentDir}
	output, err := engine.git(ctx, "rev-parse", "--verify", tag+"^{commit}")
	if err != nil {
		return fmt.Errorf("resolve published tag commit: %w", err)
	}
	head := strings.TrimSpace(output)
	poll := r.workflowPoll
	if poll <= 0 {
		poll = 20 * time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	deadline := newRegistrationDeadline(r.registrationGrace)
	for {
		runs, _, readErr := client.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, r.publication.Workflow, &github.ListWorkflowRunsOptions{
			HeadSHA: head, Branch: tag, Event: "push", ListOptions: github.ListOptions{PerPage: 100},
		})
		if readErr != nil {
			return fmt.Errorf("read release workflow: %w", readErr)
		}
		ready, started, workflowErr := workflowReleaseState(runs.WorkflowRuns, head, tag)
		if workflowErr != nil {
			return workflowErr
		}
		if ready {
			break
		}
		if deadline.expired(started) {
			return fmt.Errorf("tag %s is pushed but %s has not started for it after %s: %w", tag, r.publication.Workflow, deadline.grace, errNothingRegistered)
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
	return r.verifyPublication(ctx, client, owner, repo, tag)
}

// releaseVerifyBudget sizes the work that follows a live tag: downloading every
// loader archive, not a cleanup call.
const releaseVerifyBudget = 10 * time.Minute

func postPublicationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), releaseVerifyBudget)
}

// verifyPublishedArchives checks both CLI-owned and workflow-owned publication
// against producer checksums, GitHub digests and the downloaded bytes. Every
// call here reads: nothing is created, replaced or deleted.
//
// The archive is the contract — core's downloader fetches exactly that name.
// Whether a workflow also publishes an SBOM beside it, and under what name, is
// its own GoReleaser configuration and nothing the manifest declares, so one is
// verified when present. A CLI-staged SBOM has a known filename and is required.
func verifyPublishedArchives(ctx context.Context, client *github.Client, owner, repo, tag string, staged []loaderAsset) error {
	release, _, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, tag)
	if err != nil {
		return fmt.Errorf("read published release: %w", err)
	}
	if release.GetTagName() != tag || release.GetDraft() {
		return fmt.Errorf("release %s is absent or still draft", tag)
	}
	assets := make(map[string]*github.ReleaseAsset)
	for _, asset := range release.Assets {
		assets[asset.GetName()] = asset
	}
	checksums, err := publishedChecksums(ctx, client, owner, repo, tag, assets)
	if err != nil {
		return err
	}
	for _, candidate := range staged {
		archiveName := filepath.Base(candidate.archivePath)
		archive, ok := assets[archiveName]
		if !ok {
			return fmt.Errorf("release %s is missing loader archive %s", tag, archiveName)
		}
		if err := verifyPublishedRelease(ctx, client, owner, repo, archive, checksums); err != nil {
			return err
		}
		sbomName := archiveName + ".sbom.json"
		if candidate.sbomPath != "" {
			sbomName = filepath.Base(candidate.sbomPath)
			if assets[sbomName] == nil {
				return fmt.Errorf("release %s is missing staged SBOM %s", tag, sbomName)
			}
		}
		if sbom, ok := assets[sbomName]; ok {
			if err := verifyPublishedRelease(ctx, client, owner, repo, sbom, checksums); err != nil {
				return err
			}
		}
	}
	return nil
}

// verifyPublishedRelease establishes one asset three ways: the digest the
// publisher computed from its own build output, the digest GitHub recorded when
// it accepted the upload, and the bytes GitHub actually serves. Comparing the
// served bytes to GitHub's own record alone would only show GitHub agreeing
// with itself; the checksums file is the independent producer that makes the
// comparison mean something. Install-time verification is owned separately
// by Core's downloader; publication does not replace that check.
func verifyPublishedRelease(ctx context.Context, client *github.Client, owner, repo string, asset *github.ReleaseAsset, checksums map[string]string) error {
	built, ok := checksums[asset.GetName()]
	if !ok {
		return fmt.Errorf("%w: %s is absent from the published checksums", errAssetVerdict, asset.GetName())
	}
	if recorded := asset.GetDigest(); recorded != "sha256:"+built {
		return fmt.Errorf("%w: %s was built as sha256:%s but the release records %s",
			errAssetVerdict, asset.GetName(), built, recorded)
	}
	return retryTransientRead(ctx, func() error {
		return readPublishedAsset(ctx, client, owner, repo, asset, nil)
	})
}

// publishedChecksums reads the producer digest list. It is required: without
// it publication has no digest source independent of GitHub's upload record.
func publishedChecksums(ctx context.Context, client *github.Client, owner, repo, tag string, assets map[string]*github.ReleaseAsset) (map[string]string, error) {
	var found *github.ReleaseAsset
	for name, asset := range assets {
		if !strings.HasSuffix(name, "checksums.txt") {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("release %s publishes more than one checksums file (%s, %s); which one records the archives is ambiguous",
				tag, found.GetName(), name)
		}
		found = asset
	}
	if found == nil {
		return nil, fmt.Errorf("release %s publishes no checksums file, so its loader archives have no digest source independent of GitHub's own record (GoReleaser emits one unless `checksum: disable: true`)", tag)
	}
	if found.GetSize() > 1<<20 {
		return nil, fmt.Errorf("%w: release checksums exceed 1 MiB", errAssetVerdict)
	}
	var raw bytes.Buffer
	if err := retryTransientRead(ctx, func() error {
		raw.Reset()
		return readPublishedAsset(ctx, client, owner, repo, found, &raw)
	}); err != nil {
		return nil, err
	}
	return parseChecksums(raw.Bytes())
}

// parseChecksums reads SHA-256 records and refuses ambiguous producer evidence.
// sha256sum's binary-mode marker is accepted alongside its text-mode form.
func parseChecksums(raw []byte) (map[string]string, error) {
	checksums := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("%w: malformed checksum entry", errAssetVerdict)
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size {
			return nil, fmt.Errorf("%w: invalid SHA-256 checksum", errAssetVerdict)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if _, exists := checksums[name]; exists {
			return nil, fmt.Errorf("%w: duplicate checksum for %s", errAssetVerdict, name)
		}
		checksums[name] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: read checksums: %v", errAssetVerdict, err)
	}
	return checksums, nil
}

// retryTransientRead re-runs read while it fails for a reason a re-read could
// change. GitHub can take a moment to serve an asset it has just accepted, and
// the tag naming this release is already live, so a transient read must not be
// reported as a failed publication. A verdict on the asset's own metadata or
// bytes cannot change and is returned at once.
func retryTransientRead(ctx context.Context, read func() error) error {
	const attempts = 5
	var lastErr error
	for attempt := range attempts {
		err := read()
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

// readPublishedAsset downloads an asset, confirming it matches the size and
// digest GitHub recorded. A non-nil capture also receives the bytes, for the
// small checksums file whose content is needed rather than just its integrity.
func readPublishedAsset(ctx context.Context, client *github.Client, owner, repo string, asset *github.ReleaseAsset, capture io.Writer) error {
	stream, _, err := client.Repositories.DownloadReleaseAsset(ctx, owner, repo, asset.GetID(), http.DefaultClient)
	if err != nil {
		return fmt.Errorf("download published asset %s: %w", asset.GetName(), err)
	}
	var source io.Reader = stream
	if capture != nil {
		source = io.TeeReader(stream, capture)
	}
	err = verifyPublishedAsset(source, asset)
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
