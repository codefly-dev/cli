package localcluster

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/resources"
)

// A local Secret reaches the cluster on kubectl's stdin, never in its
// arguments, and a refusal never repeats kubectl's message, which can carry
// the value.
func TestApplySecretsSendsEachManifestOnStdin(t *testing.T) {
	const value = "s3cr3t-value-for-test"
	workspace := &resources.Workspace{Name: "example"}
	workspace.WithDir(t.TempDir())
	dir := gitops.LocalSecretsDir(workspace.Dir(), "local", "saas")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"secret-accounts","namespace":"ns"},"stringData":{"K":"` + value + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdins []string
	var argv []string
	refuse := false
	driver := &ClusterDriver{
		Workspace: workspace, Environment: localEnvironment(),
		Kubectl: func(_ context.Context, stdin string, args ...string) (string, error) {
			stdins = append(stdins, stdin)
			argv = append(argv, args...)
			if refuse {
				return "", errors.New(`Secret "secret-accounts" is invalid: data[K]: Invalid value: "` + value + `"`)
			}
			return "", nil
		},
	}
	published := []gitops.PublishResult{{Path: "environments/local/deployments/modules/saas"}}
	if err := driver.ApplySecrets(context.Background(), published); err != nil {
		t.Fatal(err)
	}
	if len(stdins) != 1 || !strings.Contains(stdins[0], value) {
		t.Fatalf("the Secret did not reach kubectl's stdin: %d calls", len(stdins))
	}
	if strings.Contains(strings.Join(argv, " "), value) {
		t.Fatal("a secret value was passed as a kubectl argument")
	}
	refuse = true
	err := driver.ApplySecrets(context.Background(), published)
	if err == nil || strings.Contains(err.Error(), value) {
		t.Fatalf("a refused Secret must fail without its value: %v", err)
	}
}
