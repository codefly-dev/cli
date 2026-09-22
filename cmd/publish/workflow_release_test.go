package publish

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
