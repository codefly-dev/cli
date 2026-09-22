package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const localCoordinateContract = `{
  "schema": "codefly/coordinate/v1",
  "coordinate": "hosted-eastus2",
  "environment": {
    "name": "local",
    "namespace": "demo",
    "cluster": {"kind": "aks", "context": "hosted-eastus2"}
  }
}`

func runEnvironmentImport(t *testing.T, dir string) (string, error) {
	t.Helper()
	contract := filepath.Join(t.TempDir(), "coordinate.json")
	if err := os.WriteFile(contract, []byte(localCoordinateContract), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	RootCmd.SetArgs([]string{"environment", "import", "local", "--coordinate-contract", contract})
	defer RootCmd.SetArgs(nil)
	return captureStdout(t, func() error {
		return RootCmd.ExecuteContext(context.Background())
	})
}

func TestEnvironmentImportFailsOnNotReadyWorkspace(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"missing"}, nil)

	out, err := runEnvironmentImport(t, dir)
	if err == nil {
		t.Fatalf("import of a not-ready workspace exited without error\noutput: %s", out)
	}
	if !strings.Contains(out, "NOT ready") {
		t.Fatalf("import did not print the readiness diagnostics\noutput: %s", out)
	}
	written := readTestFile(t, filepath.Join(dir, "workspace.codefly.yaml"))
	if !strings.Contains(written, "name: local") {
		t.Fatalf("import failed without writing the environment it validated\nfile: %s", written)
	}
}

func TestEnvironmentImportSucceedsOnReadyWorkspace(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
		"configurations/local/auth0.env": "CLIENT_ID=abc\n",
	})

	out, err := runEnvironmentImport(t, dir)
	if err != nil {
		t.Fatalf("import of a ready workspace returned error: %v\noutput: %s", err, out)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
