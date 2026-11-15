package gitops

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
)

var mindShapedServices = []string{
	"accounts",
	"cache",
	"forge-edge",
	"frontend",
	"object-storage",
	"store",
	"vault",
}

var mindShapedAWSManagedServices = map[string]struct{}{
	"cache":          {},
	"object-storage": {},
	"store":          {},
	"vault":          {},
}

func TestMindShapedAWSRenderPlanPublishDoesNotApplyKubernetes(t *testing.T) {
	remote := createBareRepository(t)
	workspace := loadGitopsWorkspaceWithServices(t, remote, mindShapedServices)
	workspaceConfiguration := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(workspaceConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "gitops:\n", `  - name: aws
    cluster:
      kind: eks
    managed-services:
      cache: {}
      object-storage: {}
      store: {}
      vault: {}
gitops:
`, 1)
	if err := os.WriteFile(workspaceConfiguration, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace.Environments = append(workspace.Environments, resourceEnvironment(t, &environments.Environment{
		Name:    "aws",
		Cluster: &environments.EnvironmentCluster{Kind: "eks"},
		ManagedServices: map[string]environments.EnvironmentManagedService{
			"cache":          {},
			"object-storage": {},
			"store":          {},
			"vault":          {},
		},
	}))
	renderMindShapedFixture(t, workspace.Dir(), "aws")
	configureSSHSigning(t)
	repository := "https://github.com/codefly-test/manifests.git"
	gitopsDefaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		t.Fatal(err)
	}
	gitopsDefaults.RepoURL = repository
	setWorkspaceGitops(t, workspace, gitopsDefaults)
	t.Setenv("GIT_CONFIG_COUNT", "4")
	t.Setenv("GIT_CONFIG_KEY_3", "url.file://"+remote+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_3", repository)

	bin := t.TempDir()
	kubectlCalled := filepath.Join(t.TempDir(), "kubectl-called")
	kubectl := filepath.Join(bin, "kubectl")
	if err := os.WriteFile(kubectl, []byte("#!/bin/sh\ntouch \"$CODEFLY_TEST_KUBECTL_CALLED\"\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	branchRevision := func() string {
		out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "refs/heads/codefly/promote-payments-aws").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	pullRequest := func() string {
		return fmt.Sprintf(`{"number":1,"html_url":"https://github.com/codefly-test/manifests/pull/1","head":{"sha":%q},"base":{"ref":"main"}}`, branchRevision())
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			fmt.Fprint(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			fmt.Fprint(w, pullRequest())
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			fmt.Fprint(w, pullRequest())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("CODEFLY_TEST_KUBECTL_CALLED", kubectlCalled)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_API_URL", server.URL)
	// This environment declares no host, so the publish delivers nothing
	// signed and needs no workflow identity: it runs with none, which is the
	// public guard against the release-identity gate reaching hostless
	// publication (it did, for one round).
	for _, name := range []string{"GITHUB_REPOSITORY", "GITHUB_WORKFLOW_REF"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	request := PublishRequest{
		Module: "payments", Environment: "aws",
		PromotionBranch: "codefly/promote-payments-aws",
	}
	plan, err := PlanPublish(context.Background(), workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changed) == 0 || plan.SnapshotRevision == "" {
		t.Fatalf("AWS publication plan = %+v", plan)
	}
	result, err := Publish(
		context.Background(),
		workspace,
		&PublishMutation{Request: request, PlanID: plan.ID},
		preparedPermit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.SnapshotRevision == "" || result.Commit == "" {
		t.Fatalf("AWS publication = %+v", result)
	}
	if _, err := os.Stat(kubectlCalled); !os.IsNotExist(err) {
		t.Fatalf("AWS GitOps publication invoked kubectl: %v", err)
	}
}

func renderMindShapedFixture(t *testing.T, root, environment string) {
	t.Helper()
	services := append([]string(nil), mindShapedServices...)
	graph := promotableServiceGraph("payments", mindShapedServices)
	if environment == "aws" {
		services = services[:0]
		for index := range graph {
			if _, managed := mindShapedAWSManagedServices[graph[index].Name]; managed {
				graph[index].Managed = true
				graph[index].Path = ""
				graph[index].Output = nil
				continue
			}
			services = append(services, graph[index].Name)
		}
	}
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: workingrender.Module(root, environment, "payments"),
		Module:      "payments", UnitNames: services, Environment: environment,
		AppProject: "payments", OwnedPath: "environments/modules/payments",
		Units: graph, Promotable: true,
	}, func(ctx context.Context, root string) error {
		for _, service := range services {
			overlay := filepath.Join(root, "services", service, "overlays", environment)
			if err := os.MkdirAll(overlay, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`), 0o644); err != nil {
				return err
			}
			manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: codefly-gitops-%s
  namespace: payments
data:
  release: qualified
`, service)
			if err := os.WriteFile(filepath.Join(overlay, "configmap.yaml"), []byte(manifest), 0o644); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runExternal(t *testing.T, dir string, input []byte, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if input != nil {
		command.Stdin = strings.NewReader(string(input))
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
