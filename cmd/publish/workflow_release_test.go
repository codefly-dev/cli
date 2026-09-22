package publish

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v89/github"
	"github.com/stretchr/testify/require"
)

func TestAgentPublicationRequiresOneExplicitOwner(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".github", "workflows")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "release.yml"), []byte("name: release\n"), 0600))
	for _, tc := range []struct {
		publication agentPublication
		valid       bool
	}{
		{agentPublication{}, false},
		{agentPublication{Owner: "other"}, false},
		{agentPublication{Owner: publicationCLI}, true},
		{agentPublication{Owner: publicationCLI, Workflow: "release.yml"}, false},
		{agentPublication{Owner: publicationWorkflow}, false},
		{agentPublication{Owner: publicationWorkflow, Workflow: "../release.yml"}, false},
		{agentPublication{Owner: publicationWorkflow, Workflow: "absent.yml"}, false},
		{agentPublication{Owner: publicationWorkflow, Workflow: "release.yml"}, true},
	} {
		err := tc.publication.validate(root)
		require.Equal(t, tc.valid, err == nil, "%+v: %v", tc.publication, err)
	}
}

func TestWorkflowReleaseRequiresSuccessfulExactTagEvent(t *testing.T) {
	for _, tc := range []struct {
		head, branch, event, status, conclusion string
		ready, failed                           bool
	}{
		{"commit", "v1.2.3", "push", "completed", "success", true, false},
		{"other", "v1.2.3", "push", "completed", "success", false, false},
		{"commit", "main", "push", "completed", "success", false, false},
		{"commit", "v1.2.3", "workflow_dispatch", "completed", "success", false, false},
		{"commit", "v1.2.3", "push", "in_progress", "", false, false},
		{"commit", "v1.2.3", "push", "completed", "failure", false, true},
		{"commit", "v1.2.3", "push", "completed", "cancelled", false, true},
		{"commit", "v1.2.3", "push", "completed", "skipped", false, true},
	} {
		run := &github.WorkflowRun{ID: github.Ptr(int64(1)), HeadSHA: github.Ptr(tc.head), HeadBranch: github.Ptr(tc.branch), Event: github.Ptr(tc.event), Status: github.Ptr(tc.status), Conclusion: github.Ptr(tc.conclusion)}
		ready, err := workflowReleaseReady([]*github.WorkflowRun{run}, "commit", "v1.2.3")
		require.Equal(t, tc.ready, ready, "%+v", tc)
		require.Equal(t, tc.failed, err != nil, "%+v: %v", tc, err)
	}
	ready, err := workflowReleaseReady(nil, "commit", "v1.2.3")
	require.NoError(t, err)
	require.False(t, ready)
}

func TestPublishedAssetMustMatchActualBytes(t *testing.T) {
	payload := "published executable"
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(payload)))
	asset := &github.ReleaseAsset{Name: github.Ptr("agent.tar.gz"), Size: github.Ptr(len(payload)), Digest: github.Ptr(digest)}
	require.NoError(t, verifyPublishedAsset(strings.NewReader(payload), asset))
	for _, wrong := range []string{"", payload[:len(payload)-1], payload + "extra", strings.Repeat("x", len(payload))} {
		require.Error(t, verifyPublishedAsset(strings.NewReader(wrong), asset))
	}
	asset.Digest = nil
	require.ErrorContains(t, verifyPublishedAsset(strings.NewReader(payload), asset), "no verifiable")
}

// publishedRelease serves the release an owner workflow published, the way
// GitHub serves it: the tag lookup, then a redirect per asset to the host that
// actually holds the bytes. Any request that is not a read fails the test,
// which is the property under test — the workflow owns these assets and publish
// may only look at them.
type publishedRelease struct {
	t        *testing.T
	tag      string
	draft    bool
	payloads map[string]string // asset name -> bytes the workflow published
	digests  map[string]string // asset name -> digest GitHub recorded for it

	// beforeTagResponse runs before the release lookup is answered, so a test
	// can spend the caller's publish budget midway through verification.
	beforeTagResponse func()
	// transientFailures is how many asset lookups answer 500  before serving bytes.
	transientFailures int
	assetLookups      int
	blobReads         int // reads served from the storage host behind the redirect
}

func recordedDigests(payloads map[string]string) map[string]string {
	digests := map[string]string{}
	for name, payload := range payloads {
		digests[name] = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(payload)))
	}
	return digests
}

func (p *publishedRelease) client() *github.Client {
	p.t.Helper()
	names := make([]string, 0, len(p.payloads))
	for name := range p.payloads {
		names = append(names, name)
	}
	sort.Strings(names)
	byID := map[string]string{}
	entries := make([]string, 0, len(names))
	for i, name := range names {
		id := strconv.Itoa(i + 1)
		byID[id] = name
		entries = append(entries, fmt.Sprintf(`{"id":%s,"name":%q,"size":%d,"digest":%q}`,
			id, name, len(p.payloads[name]), p.digests[name]))
	}
	const repoPath = "/repos/codefly-dev/service-go/releases/"
	const blobPath = "/release-assets/"

	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(p.t, http.MethodGet, r.Method,
			"publish must not write to a workflow-owned release: %s %s", r.Method, r.URL.Path)
		switch {
		case r.URL.Path == repoPath+"tags/"+p.tag:
			if p.beforeTagResponse != nil {
				p.beforeTagResponse()
			}
			fmt.Fprintf(w, `{"id":42,"tag_name":%q,"draft":%t,"assets":[%s]}`,
				p.tag, p.draft, strings.Join(entries, ","))
		case strings.HasPrefix(r.URL.Path, repoPath+"assets/"):
			mu.Lock()
			p.assetLookups++
			transient := p.assetLookups <= p.transientFailures
			mu.Unlock()
			if transient {
				http.Error(w, `{"message":"unavailable"}`, http.StatusInternalServerError)
				return
			}
			// GitHub never serves asset bytes from the API host; it redirects
			// to the storage host, which is the path production takes.
			http.Redirect(w, r, blobPath+strings.TrimPrefix(r.URL.Path, repoPath+"assets/"), http.StatusFound)
		case strings.HasPrefix(r.URL.Path, blobPath):
			name, ok := byID[strings.TrimPrefix(r.URL.Path, blobPath)]
			if !ok {
				http.Error(w, "no such asset", http.StatusNotFound)
				return
			}
			mu.Lock()
			p.blobReads++
			mu.Unlock()
			fmt.Fprint(w, p.payloads[name])
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	p.t.Cleanup(server.Close)
	base := server.URL + "/"
	client, err := github.NewClient(github.WithURLs(&base, &base))
	require.NoError(p.t, err)
	return client
}

func TestVerifyWorkflowReleaseReadsPublishedAssetsWithoutWriting(t *testing.T) {
	const archive = "service-go_0.0.16_linux_amd64.tar.gz"
	const sbom = archive + ".sbom.json"
	published := func() map[string]string {
		return map[string]string{archive: "archive bytes", sbom: "sbom bytes"}
	}

	for _, tc := range []struct {
		name     string
		draft    bool
		payloads map[string]string
		tamper   func(digests map[string]string)
		wantErr  string
	}{
		{name: "archive and sbom", payloads: published()},
		{
			name:     "archive alone, workflow publishes no sbom",
			payloads: map[string]string{archive: "archive bytes"},
		},
		{
			name:     "archive absent",
			payloads: map[string]string{sbom: "sbom bytes"},
			wantErr:  "missing loader archive " + archive,
		},
		{name: "draft", draft: true, payloads: published(), wantErr: "workflow did not publish release"},
		{
			name:     "archive bytes differ from recorded digest",
			payloads: published(),
			tamper:   func(digests map[string]string) { digests[archive] = digests[sbom] },
			wantErr:  archive + " does not match its recorded size and digest",
		},
		{
			name:     "sbom bytes differ from recorded digest",
			payloads: published(),
			tamper:   func(digests map[string]string) { digests[sbom] = digests[archive] },
			wantErr:  sbom + " does not match its recorded size and digest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digests := recordedDigests(tc.payloads)
			if tc.tamper != nil {
				tc.tamper(digests)
			}
			release := &publishedRelease{t: t, tag: "v0.0.16", draft: tc.draft, payloads: tc.payloads, digests: digests}

			err := verifyWorkflowRelease(t.Context(), release.client(), "codefly-dev", "service-go", "v0.0.16", stageAssets(t, archive))

			if tc.wantErr == "" {
				require.NoError(t, err)
				require.Positive(t, release.blobReads,
					"asset bytes must be read from the storage host GitHub redirects to, not the API host")
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// The tag is live and the workflow has published by the time verification
// runs, so a transient read of a just-accepted asset must not be reported as a
// failed publication.
func TestVerifyWorkflowReleaseRetriesATransientAssetRead(t *testing.T) {
	const archive = "service-go_0.0.16_linux_amd64.tar.gz"
	payloads := map[string]string{archive: "archive bytes"}
	release := &publishedRelease{
		t: t, tag: "v0.0.16", payloads: payloads, digests: recordedDigests(payloads),
		transientFailures: 2,
	}

	require.NoError(t, verifyWorkflowRelease(t.Context(), release.client(), "codefly-dev", "service-go", "v0.0.16", stageAssets(t, archive)))
	require.Equal(t, 3, release.assetLookups, "the first two reads failed and must have been retried")
}

// A wrong digest is a verdict on the bytes themselves: re-reading cannot change
// it, and retrying would re-download every archive for nothing.
func TestVerifyWorkflowReleaseDoesNotRetryAVerdictOnTheBytes(t *testing.T) {
	const archive = "service-go_0.0.16_linux_amd64.tar.gz"
	payloads := map[string]string{archive: "archive bytes"}
	digests := recordedDigests(payloads)
	digests[archive] = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("some other build")))
	release := &publishedRelease{t: t, tag: "v0.0.16", payloads: payloads, digests: digests}

	err := verifyWorkflowRelease(t.Context(), release.client(), "codefly-dev", "service-go", "v0.0.16", stageAssets(t, archive))

	require.ErrorIs(t, err, errAssetVerdict)
	require.Equal(t, 1, release.assetLookups, "a verdict on the bytes must not be re-read")
}

// Verification runs after the tag is live, so the budget the workflow wait
// consumed must not decide whether a release that shipped is reported as
// published.
func TestPostPublicationVerificationOutlivesTheSpentPublishBudget(t *testing.T) {
	const archive = "service-go_0.0.16_linux_amd64.tar.gz"
	payloads := map[string]string{archive: "archive bytes"}
	spent, exhaust := context.WithCancel(t.Context())
	release := &publishedRelease{
		t: t, tag: "v0.0.16", payloads: payloads, digests: recordedDigests(payloads),
		beforeTagResponse: func() {
			exhaust()
			<-spent.Done()
		},
	}
	client := release.client()

	ctx, cancel := postPublicationContext(spent)
	defer cancel()
	err := verifyWorkflowRelease(ctx, client, "codefly-dev", "service-go", "v0.0.16", stageAssets(t, archive))

	require.NoError(t, err)
	require.ErrorIs(t, spent.Err(), context.Canceled, "the publish budget must be spent by the time verification finishes")
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.Greater(t, time.Until(deadline), time.Minute, "verification needs a budget sized to download every archive")
}
