package control

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunDeployRejectsRemoteTargetBeforeStartingFlow(t *testing.T) {
	root := writeWorkspace(t)
	workspace := fixtureWorkspaceYAML + `environments:
    - name: production
      cluster:
          kind: eks
          kubeconfig: /does/not/exist
          context: k3d-production
`
	if err := os.WriteFile(filepath.Join(root, "workspace.codefly.yaml"), []byte(workspace), 0o600); err != nil {
		t.Fatal(err)
	}
	plane, err := NewAt(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plane.Close() })

	_, err = plane.Deploy(context.Background(), DeployRequest{
		Service: "backend/api",
		Env:     "production",
	})

	if err == nil {
		t.Fatal("remote direct deploy succeeded")
	}
	if got := err.Error(); !containsAll(got, "exact local k3d target", "--render-only", "GitOps") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
