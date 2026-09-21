package platform

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestDeploySendsEveryQueuedDeployment(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if r.Method != http.MethodPost || r.URL.Path != "/platform/workspace/deploy" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || len(body) == 0 {
			t.Errorf("request body missing: %v", err)
		}
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	previousClient := platformClient
	platformClient = &Client{baseURL: server.URL}
	defer func() { platformClient = previousClient }()

	manager := &DeploymentManager{}
	workspaceRoot := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspaceRoot, "workspace.codefly.yaml"), []byte("name: test\nlayout: modules\n"), 0o600))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), workspaceRoot)
	require.NoError(t, err)
	for _, name := range []string{"api", "worker"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte("kind: Deployment\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		manager.deployments = append(manager.deployments, Deployment{OutputPath: dir, ModuleRoot: dir})
	}

	if err := manager.Deploy(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("Deploy sent %d requests, want 2", got)
	}
	require.NoError(t, os.WriteFile(filepath.Join(manager.deployments[1].ModuleRoot, selectionguard.SelectionFile), []byte("{}"), 0o600))
	err = manager.Deploy(t.Context(), workspace)
	require.ErrorIs(t, err, selectionguard.ErrUnboundExecution)
	require.EqualValues(t, 2, requests.Load(), "a blocked later module must prevent the first deployment request too")
}
