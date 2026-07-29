package gitops

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalK3dDisposableGitQualification(t *testing.T) {
	if os.Getenv("CODEFLY_GITOPS_K3D_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GITOPS_K3D_QUALIFY=1 to run the disposable k3d qualification")
	}
	for _, binary := range []string{"docker", "k3d", "kubectl", "ssh-keygen"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("%s is required: %v", binary, err)
		}
	}

	remote := createBareRepository(t)
	workspace := loadGitopsWorkspace(t, remote)
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: filepath.Join(workspace.Dir(), "deployments", "environments", "local", "modules", "payments"),
		Module:      "payments", Environment: "local", AppProject: "payments", Promotable: true,
	}, func(ctx context.Context, root string) error {
		if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte(`apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "configmap.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: codefly-gitops-qualification
  namespace: payments
data:
  release: qualified
`), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	configureSSHSigning(t)
	request := PublishRequest{
		Module: "payments", Environment: "local", Local: true,
		PromotionBranch: "codefly/promote-payments-local",
	}
	plan, err := PlanPublish(context.Background(), workspace, &request)
	if err != nil {
		t.Fatal(err)
	}
	published, err := Publish(context.Background(), workspace, &PublishMutation{Request: request, PlanID: plan.ID}, preparedPermit)
	if err != nil {
		t.Fatal(err)
	}
	mergePromotionToMain(t, remote, request.PromotionBranch)
	gitRun(t, "", "--git-dir", remote, "update-server-info")

	cluster := "codefly-gitops-" + fmt.Sprintf("%x", time.Now().UnixNano())
	runExternal(t, "", nil, "k3d", "cluster", "create", cluster,
		"--servers", "1", "--agents", "0", "--wait", "--timeout", "2m",
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false")
	t.Cleanup(func() {
		command := exec.Command("k3d", "cluster", "delete", cluster)
		_ = command.Run()
	})
	gitServer := cluster + "-git"
	runExternal(t, "", nil, "docker", "run", "--detach", "--name", gitServer,
		"--network", "k3d-"+cluster, "--volume", filepath.Dir(remote)+":/git:ro",
		"alpine:3.22.1", "sh", "-c",
		"apk add --no-cache git-daemon >/dev/null && exec git daemon --reuseaddr --export-all --base-path=/git --listen=0.0.0 --port=9418 /git")
	t.Cleanup(func() {
		command := exec.Command("docker", "rm", "--force", gitServer)
		_ = command.Run()
	})
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig.yaml")
	config := runExternal(t, "", nil, "k3d", "kubeconfig", "get", cluster)
	if err := os.WriteFile(kubeconfig, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	kubectl := func(input []byte, args ...string) string {
		full := append([]string{"--kubeconfig", kubeconfig}, args...)
		return runExternal(t, "", input, "kubectl", full...)
	}
	kubectl(nil, "create", "namespace", "argocd")
	kubectl(nil, "apply", "--server-side", "--force-conflicts", "-n", "argocd", "-f",
		"https://raw.githubusercontent.com/argoproj/argo-cd/v3.4.1/manifests/install.yaml")
	kubectl(nil, "wait", "--for=condition=Ready", "pod", "--all", "-n", "argocd", "--timeout=5m")
	kubectl(nil, "create", "namespace", "payments")

	repository := "git://" + gitServer + "/" + filepath.Base(remote)
	argoResources := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: payments
  namespace: argocd
spec:
  sourceRepos:
    - %s
  destinations:
    - namespace: payments
      server: https://kubernetes.default.svc
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: payments
  namespace: argocd
spec:
  project: payments
  source:
    repoURL: %s
    targetRevision: main
    path: environments/local/modules/payments
  destination:
    server: https://kubernetes.default.svc
    namespace: payments
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
`, repository, repository)
	kubectl([]byte(argoResources), "apply", "-f", "-")

	bin := t.TempDir()
	argocd := filepath.Join(bin, "argocd")
	shim := `#!/bin/sh
if [ "$1" = "proj" ]; then
  exec kubectl --kubeconfig "$CODEFLY_TEST_KUBECONFIG" -n argocd get appproject "$3" -o json
fi
if [ "$1" = "app" ]; then
  exec kubectl --kubeconfig "$CODEFLY_TEST_KUBECONFIG" -n argocd get application "$3" -o json
fi
if [ "$1" = "cluster" ]; then
  printf '{"server":"https://kubernetes.default.svc","name":"%s","config":{"kubeconfig":"%s"}}\n' "$CODEFLY_TEST_CLUSTER" "$CODEFLY_TEST_KUBECONFIG"
  exit 0
fi
exit 2
`
	if err := os.WriteFile(argocd, []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEFLY_TEST_KUBECONFIG", kubeconfig)
	t.Setenv("CODEFLY_TEST_CLUSTER", cluster)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	observed, err := Observe(context.Background(), &ObserveRequest{
		WorkspaceRoot: workspace.Dir(), Module: "payments", Environment: "local",
		AppProject: "payments", Applications: []string{"payments"},
		Revision: published.Commit, Commit: published.Commit, Tree: published.Tree,
		RenderDigest: published.RenderDigest, Repository: published.Repository, Path: published.Path,
		PullRequest: published.PullRequest, Local: true,
		Timeout: 5 * time.Minute, PollInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Evidence.Health != "Healthy" || observed.Evidence.ArgoRevision != published.Commit {
		t.Fatalf("qualification evidence = %+v", observed.Evidence)
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
