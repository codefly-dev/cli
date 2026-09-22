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

const (
	testArchive   = "service-go_0.0.16_linux_amd64.tar.gz"
	testSBOM      = testArchive + ".sbom.json"
	testChecksums = "service-go_0.0.16_checksums.txt"
)

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

	// checksumDigests overrides what the checksums file lists for an asset, and
	// omitFromChecksums leaves it out, standing in for a release whose recorded
	// assets are not the ones the workflow built.
	checksumDigests   map[string]string
	omitFromChecksums map[string]bool
	omitChecksums     bool
	secondChecksums   bool

	// beforeTagResponse runs before the release lookup is answered, so a test
	// can spend the caller's publish budget midway through verification.
	beforeTagResponse func()
	// transient is how many initial lookups of an asset answer 500.
	transient map[string]int

	mu      sync.Mutex
	lookups map[string]int // asset name -> API lookups served
	blobs   int            // reads served from the storage host behind the redirect
}

func recordedDigests(payloads map[string]string) map[string]string {
	digests := map[string]string{}
	for name, payload := range payloads {
		digests[name] = digestOf(payload)
	}
	return digests
}

func digestOf(payload string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(payload)))
}

func publishedBy(t *testing.T, payloads map[string]string) *publishedRelease {
	return &publishedRelease{t: t, tag: "v0.0.16", payloads: payloads, digests: recordedDigests(payloads)}
}

func sortedNames(payloads map[string]string) []string {
	names := make([]string, 0, len(payloads))
	for name := range payloads {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// withChecksums appends the digest list a release workflow computes from its own
// build output, which is the record GitHub's own is cross-checked against.
func (p *publishedRelease) withChecksums() *publishedRelease {
	if p.omitChecksums {
		return p
	}
	body := func() string {
		var out strings.Builder
		for _, name := range sortedNames(p.payloads) {
			if p.omitFromChecksums[name] {
				continue
			}
			hex := strings.TrimPrefix(p.digests[name], "sha256:")
			if override, ok := p.checksumDigests[name]; ok {
				hex = override
			}
			fmt.Fprintf(&out, "%s  %s\n", hex, name)
		}
		return out.String()
	}
	first := body()
	p.payloads[testChecksums] = first
	p.digests[testChecksums] = digestOf(first)
	if p.secondChecksums {
		second := body()
		p.payloads["extra_checksums.txt"] = second
		p.digests["extra_checksums.txt"] = digestOf(second)
	}
	return p
}

func (p *publishedRelease) client() *github.Client {
	p.t.Helper()
	p.lookups = map[string]int{}
	names := sortedNames(p.payloads)
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
			id := strings.TrimPrefix(r.URL.Path, repoPath+"assets/")
			p.mu.Lock()
			p.lookups[byID[id]]++
			transient := p.lookups[byID[id]] <= p.transient[byID[id]]
			p.mu.Unlock()
			if transient {
				http.Error(w, `{"message":"unavailable"}`, http.StatusInternalServerError)
				return
			}
			// GitHub never serves asset bytes from the API host; it redirects to
			// the storage host, which is the path production takes.
			http.Redirect(w, r, blobPath+id, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, blobPath):
			name, ok := byID[strings.TrimPrefix(r.URL.Path, blobPath)]
			if !ok {
				http.Error(w, "no such asset", http.StatusNotFound)
				return
			}
			p.mu.Lock()
			p.blobs++
			p.mu.Unlock()
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

func (p *publishedRelease) verify(staged []loaderAsset) error {
	return verifyWorkflowRelease(p.t.Context(), p.client(), "codefly-dev", "service-go", p.tag, staged)
}

func TestVerifyWorkflowReleaseReadsPublishedAssetsWithoutWriting(t *testing.T) {
	published := func() map[string]string {
		return map[string]string{testArchive: "archive bytes", testSBOM: "sbom bytes"}
	}
	const foreign = "a build nobody published"

	for _, tc := range []struct {
		name    string
		release func(t *testing.T) *publishedRelease
		wantErr string
	}{
		{
			name:    "archive and sbom",
			release: func(t *testing.T) *publishedRelease { return publishedBy(t, published()).withChecksums() },
		},
		{
			name: "archive alone, workflow publishes no sbom",
			release: func(t *testing.T) *publishedRelease {
				return publishedBy(t, map[string]string{testArchive: "archive bytes"}).withChecksums()
			},
		},
		{
			name: "archive absent",
			release: func(t *testing.T) *publishedRelease {
				return publishedBy(t, map[string]string{testSBOM: "sbom bytes"}).withChecksums()
			},
			wantErr: "missing loader archive " + testArchive,
		},
		{
			name: "draft",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published()).withChecksums()
				r.draft = true
				return r
			},
			wantErr: "workflow did not publish release",
		},
		{
			name: "no checksums file",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published())
				r.omitChecksums = true
				return r.withChecksums()
			},
			wantErr: "publishes no checksums file",
		},
		{
			name: "two checksums files",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published())
				r.secondChecksums = true
				return r.withChecksums()
			},
			wantErr: "more than one checksums file",
		},
		{
			name: "archive absent from the checksums the workflow built",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published())
				r.omitFromChecksums = map[string]bool{testArchive: true}
				return r.withChecksums()
			},
			wantErr: testArchive + " is absent from the checksums",
		},
		{
			name: "release records a digest the workflow did not build",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published())
				r.checksumDigests = map[string]string{
					testArchive: strings.TrimPrefix(digestOf(foreign), "sha256:"),
				}
				return r.withChecksums()
			},
			wantErr: "but the release records",
		},
		{
			name: "served archive bytes match neither record",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published())
				r.digests[testArchive] = digestOf(foreign)
				return r.withChecksums()
			},
			wantErr: testArchive + " does not match its recorded size and digest",
		},
		{
			name: "served sbom bytes match neither record",
			release: func(t *testing.T) *publishedRelease {
				r := publishedBy(t, published())
				r.digests[testSBOM] = digestOf(foreign)
				return r.withChecksums()
			},
			wantErr: testSBOM + " does not match its recorded size and digest",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := tc.release(t)

			err := release.verify(stageAssets(t, testArchive))

			if tc.wantErr == "" {
				require.NoError(t, err)
				require.Positive(t, release.blobs,
					"asset bytes must be read from the storage host GitHub redirects to, not the API host")
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// The tag is live and the workflow has published by the time verification runs,
// so a transient read of a just-accepted asset must not be reported as a failed
// publication.
func TestVerifyWorkflowReleaseRetriesATransientAssetRead(t *testing.T) {
	release := publishedBy(t, map[string]string{testArchive: "archive bytes"}).withChecksums()
	release.transient = map[string]int{testArchive: 2, testChecksums: 1}

	require.NoError(t, release.verify(stageAssets(t, testArchive)))
	require.Equal(t, 3, release.lookups[testArchive], "the first two archive reads failed and must have been retried")
	require.Equal(t, 2, release.lookups[testChecksums], "the first checksums read failed and must have been retried")
}

// A wrong digest is a verdict on the bytes themselves: re-reading cannot change
// it, and retrying would re-download every archive for nothing.
func TestVerifyWorkflowReleaseDoesNotRetryAVerdictOnTheBytes(t *testing.T) {
	release := publishedBy(t, map[string]string{testArchive: "archive bytes"})
	foreign := digestOf("a build nobody published")
	release.digests[testArchive] = foreign
	release.checksumDigests = map[string]string{testArchive: strings.TrimPrefix(foreign, "sha256:")}
	release.withChecksums()

	err := release.verify(stageAssets(t, testArchive))

	require.ErrorIs(t, err, errAssetVerdict)
	require.Equal(t, 1, release.lookups[testArchive], "a verdict on the bytes must not be re-read")
}

// A digest that already disagrees with the workflow's own record settles the
// question before any archive is fetched.
func TestVerifyWorkflowReleaseRejectsAMismatchWithoutDownloadingTheArchive(t *testing.T) {
	release := publishedBy(t, map[string]string{testArchive: "archive bytes"})
	release.checksumDigests = map[string]string{
		testArchive: strings.TrimPrefix(digestOf("a build nobody published"), "sha256:"),
	}
	release.withChecksums()

	err := release.verify(stageAssets(t, testArchive))

	require.ErrorIs(t, err, errAssetVerdict)
	require.Zero(t, release.lookups[testArchive], "the archive need not be downloaded to settle a digest mismatch")
}

// Verification runs after the tag is live, so the budget the workflow wait
// consumed must not decide whether a release that shipped is reported as
// published.
func TestPostPublicationVerificationOutlivesTheSpentPublishBudget(t *testing.T) {
	spent, exhaust := context.WithCancel(t.Context())
	release := publishedBy(t, map[string]string{testArchive: "archive bytes"}).withChecksums()
	release.beforeTagResponse = func() {
		exhaust()
		<-spent.Done()
	}
	client := release.client()

	ctx, cancel := postPublicationContext(spent)
	defer cancel()
	err := verifyWorkflowRelease(ctx, client, "codefly-dev", "service-go", "v0.0.16", stageAssets(t, testArchive))

	require.NoError(t, err)
	require.ErrorIs(t, spent.Err(), context.Canceled, "the publish budget must be spent by the time verification finishes")
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.Greater(t, time.Until(deadline), time.Minute, "verification needs a budget sized to download every archive")
}

func TestParseChecksumsReadsBothSha256sumForms(t *testing.T) {
	raw := "aaaa  text-mode.tar.gz\nbbbb *binary-mode.tar.gz\n\ngarbage\ncccc  extra  fields\n"

	require.Equal(t, map[string]string{
		"text-mode.tar.gz":   "aaaa",
		"binary-mode.tar.gz": "bbbb",
	}, parseChecksums([]byte(raw)))
}
