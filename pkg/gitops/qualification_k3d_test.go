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
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/google/go-github/v89/github"
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
	workspace.Environments = append(workspace.Environments, &resources.Environment{
		Name:    "aws",
		Cluster: &resources.EnvironmentCluster{Kind: "eks"},
		ManagedServices: map[string]resources.EnvironmentManagedService{
			"cache":          {},
			"object-storage": {},
			"store":          {},
			"vault":          {},
		},
	})
	renderMindShapedFixture(t, workspace.Dir(), "aws")
	configureSSHSigning(t)
	repository := "https://github.com/codefly-test/manifests.git"
	workspace.Gitops.RepoURL = repository
	t.Setenv("GIT_CONFIG_COUNT", "4")
	t.Setenv("GIT_CONFIG_KEY_3", "url.file://"+remote+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_3", repository)

	bin := t.TempDir()
	kubectlCalled := filepath.Join(t.TempDir(), "kubectl-called")
	kubectl := filepath.Join(bin, "kubectl")
	if err := os.WriteFile(kubectl, []byte("#!/bin/sh\ntouch \"$CODEFLY_TEST_KUBECTL_CALLED\"\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The promotion pull-request flow is a GitHub platform operation on the
	// go-github API. Serve the three REST endpoints it touches from a local
	// server and point the client at it, so no request reaches api.github.com.
	// The head SHA is read from the promotion branch of the local remote, so
	// verifyPullRequest sees the commit Publish actually pushed.
	promotionPR := func(w http.ResponseWriter) {
		out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "refs/heads/codefly/promote-payments-aws").Output()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"number":1,"html_url":"https://github.com/codefly-test/manifests/pull/1","head":{"sha":"%s"},"base":{"ref":"main"}}`,
			strings.TrimSpace(string(out)))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/repos/codefly-test/manifests/pulls" {
			fmt.Fprint(w, "[]") // no open promotion pull request yet
			return
		}
		promotionPR(w) // create (POST .../pulls) and verify (GET .../pulls/1)
	}))
	defer server.Close()
	originalNewClient := newGitHubClient
	newGitHubClient = func() (*github.Client, error) {
		endpoint := server.URL + "/"
		return github.NewClient(github.WithURLs(&endpoint, &endpoint))
	}
	t.Cleanup(func() { newGitHubClient = originalNewClient })

	t.Setenv("CODEFLY_TEST_KUBECTL_CALLED", kubectlCalled)
	t.Setenv("CODEFLY_TEST_REMOTE", remote)
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
	workspace := loadGitopsWorkspaceWithServices(t, remote, mindShapedServices)
	renderMindShapedFixture(t, workspace.Dir(), "local")
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
	var argoResources strings.Builder
	fmt.Fprintf(&argoResources, `apiVersion: argoproj.io/v1alpha1
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
`, repository)
	for _, service := range mindShapedServices {
		fmt.Fprintf(&argoResources, `---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: payments-%s
  namespace: argocd
spec:
  project: payments
  source:
    repoURL: %s
    targetRevision: %s
    path: environments/deployments/modules/payments/services/%s/overlays/local
  destination:
    server: https://kubernetes.default.svc
    namespace: payments
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
`, service, repository, published.SnapshotRevision, service)
	}
	kubectl([]byte(argoResources.String()), "apply", "-f", "-")

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
	applications := make([]string, 0, len(mindShapedServices))
	for _, service := range mindShapedServices {
		applications = append(applications, "payments-"+service)
	}
	observed, err := Observe(context.Background(), &ObserveRequest{
		WorkspaceRoot: workspace.Dir(), Module: "payments", Environment: "local",
		AppProject: "payments", Applications: applications,
		Revision: published.SnapshotRevision, Commit: published.Commit, Tree: published.Tree,
		RenderDigest: published.RenderDigest, Repository: published.Repository, Path: published.Path,
		PullRequest: published.PullRequest, Local: true,
		Timeout: 5 * time.Minute, PollInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Evidence.Health != "Healthy" || observed.Evidence.ArgoRevision != published.SnapshotRevision {
		t.Fatalf("qualification evidence = %+v", observed.Evidence)
	}
	for _, service := range mindShapedServices {
		name := "codefly-gitops-" + service
		if value := kubectl(nil, "get", "configmap", name, "-n", "payments", "-o", "jsonpath={.data.release}"); value != "qualified" {
			t.Fatalf("ConfigMap %s release = %q", name, value)
		}
	}
}

// TestLocalK3dDisposableSolutionQualification is the acceptance bar for the
// codefly:solution deploy path: a hello-solution goes OCI package → rendered
// owned tree → published snapshot → ArgoCD Application → its own synced
// namespace. The in-process executor stands in for the verified-artifact plugin;
// everything downstream (render pipeline, publish, ArgoCD reconciliation) is the
// real service path, unchanged, driving a solution unit to the last mile the
// previous non-service kinds never reached.
func TestLocalK3dDisposableSolutionQualification(t *testing.T) {
	if os.Getenv("CODEFLY_GITOPS_K3D_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GITOPS_K3D_QUALIFY=1 to run the disposable k3d solution qualification")
	}
	for _, binary := range []string{"docker", "k3d", "kubectl", "ssh-keygen"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("%s is required: %v", binary, err)
		}
	}

	installFakeSolutionExecutor(t, &fakeSolutionExecutor{})
	remote := createBareRepository(t)
	workspace := loadSolutionWorkspace(t, remote)
	env := workspace.FindEnvironment("local")
	if env == nil {
		t.Fatal("environment local not found")
	}
	agent := &resources.Agent{
		Kind: resources.SolutionAgent, Publisher: "codefly.dev", Name: "hello-solution", Version: "0.0.1",
	}
	if _, err := RenderSolution(context.Background(), &SolutionRenderRequest{
		Workspace: workspace, Environment: env, Agent: agent, Name: "hello",
		Source:     filepath.Join(workspace.Dir(), "solution-src"),
		Reference:  "ghcr.io/codefly-dev/hello-solution:0.0.1",
		AppProject: "hello",
	}); err != nil {
		t.Fatal(err)
	}

	configureSSHSigning(t)
	request := PublishRequest{
		Module: "hello", Environment: "local", Local: true,
		PromotionBranch: "codefly/promote-hello-local",
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

	cluster := "codefly-solution-" + fmt.Sprintf("%x", time.Now().UnixNano())
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

	// The solution provisions its own namespace: the AppProject whitelists the
	// cluster-scoped Namespace the rendered overlay carries, and the namespace is
	// NOT pre-created — proving the packaged solution stamps its own anatomy.
	repository := "git://" + gitServer + "/" + filepath.Base(remote)
	argoResources := fmt.Sprintf(`apiVersion: argoproj.io/v1alpha1
kind: AppProject
metadata:
  name: hello
  namespace: argocd
spec:
  sourceRepos:
    - %s
  destinations:
    - namespace: hello
      server: https://kubernetes.default.svc
  clusterResourceWhitelist:
    - group: ""
      kind: Namespace
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: hello
  namespace: argocd
spec:
  project: hello
  source:
    repoURL: %s
    targetRevision: %s
    path: environments/deployments/modules/hello/solutions/hello/overlays/local
  destination:
    server: https://kubernetes.default.svc
    namespace: hello
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=false
`, repository, repository, published.SnapshotRevision)
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
		WorkspaceRoot: workspace.Dir(), Module: "hello", Environment: "local",
		AppProject: "hello", Applications: []string{"hello"},
		Revision: published.SnapshotRevision, Commit: published.Commit, Tree: published.Tree,
		RenderDigest: published.RenderDigest, Repository: published.Repository, Path: published.Path,
		PullRequest: published.PullRequest, Local: true,
		Timeout: 5 * time.Minute, PollInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observed.Evidence.Health != "Healthy" || observed.Evidence.ArgoRevision != published.SnapshotRevision {
		t.Fatalf("solution qualification evidence = %+v", observed.Evidence)
	}
	// The solution's rendered ConfigMap landed in the namespace it stamped.
	if value := kubectl(nil, "get", "configmap", "hello", "-n", "hello", "-o", "jsonpath={.data.release}"); value != "qualified" {
		t.Fatalf("solution ConfigMap release = %q", value)
	}
}

// TestLocalFetchRemoteLifecycle proves the CLI-owned read-only fetch remote on a
// disposable k3d network: host loopback exposure, private reachability of the
// exact reviewed revision over container DNS + TLS with declarative CA trust, no
// leaked private key, and a validated teardown that preserves repository data.
func TestLocalFetchRemoteLifecycle(t *testing.T) {
	if os.Getenv("CODEFLY_GITOPS_K3D_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GITOPS_K3D_QUALIFY=1 to run the disposable k3d fetch-remote qualification")
	}
	for _, binary := range []string{"docker", "k3d", "git"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("%s is required: %v", binary, err)
		}
	}

	source := createBareRepository(t)
	revision := runExternal(t, "", nil, "git", "--git-dir", source, "rev-parse", "refs/heads/main")

	cluster := "codefly-remote-" + fmt.Sprintf("%x", time.Now().UnixNano())
	runExternal(t, "", nil, "k3d", "cluster", "create", cluster,
		"--servers", "1", "--agents", "0", "--wait", "--timeout", "2m",
		"--kubeconfig-update-default=false", "--kubeconfig-switch-context=false")
	t.Cleanup(func() {
		command := exec.Command("k3d", "cluster", "delete", cluster)
		_ = command.Run()
	})

	// Pin the runtime image by the digest actually pulled — a floating tag is a
	// mutable image the lifecycle refuses.
	runExternal(t, "", nil, "docker", "pull", "nginx:1.27.3-alpine")
	digest := runExternal(t, "", nil, "docker", "inspect", "--format", "{{index .RepoDigests 0}}", "nginx:1.27.3-alpine")
	t.Setenv("CODEFLY_GITOPS_REMOTE_IMAGE", digest)

	spec, err := NewRemoteSpec(&RemoteConfig{
		WorkspaceRoot:  t.TempDir(),
		Owner:          "qualify",
		Workspace:      "payments",
		Environment:    "local",
		Cluster:        cluster,
		RepositorySlug: "codefly-test/manifests",
		SourceRepo:     "file://" + source,
	})
	if err != nil {
		t.Fatal(err)
	}
	remote := &FetchRemote{Spec: spec}
	t.Cleanup(func() { _ = remote.Down(context.Background()) })

	status, err := remote.Up(context.Background(), revision)
	if err != nil {
		t.Fatalf("fetch remote up: %v", err)
	}
	if len(status.Findings) != 0 {
		t.Fatalf("fresh fetch remote reported drift: %+v", status.Findings)
	}
	for _, binding := range status.State.PortBindings {
		if isWildcardHost(binding.HostIP) {
			t.Fatalf("host binding is not loopback-only: %+v", binding)
		}
	}
	if !strings.Contains(status.State.Image, "@sha256:") {
		t.Fatalf("runtime image is not digest-pinned: %s", status.State.Image)
	}
	if status.State.Labels[labelRole] != remoteRole || status.State.Labels[labelOwner] != "qualify" {
		t.Fatalf("ownership labels missing: %+v", status.State.Labels)
	}

	// Private reachability: a sibling on the k3d network fetches the exact
	// revision over container DNS + TLS, trusting only the generated CA. No host
	// port is involved.
	clone := runExternal(t, "", nil, "docker", "run", "--rm", "--network", spec.Network,
		"--volume", filepath.Join(spec.TLSDir(), "ca.crt")+":/ca.crt:ro",
		"alpine:3.22.1", "sh", "-c",
		"apk add --no-cache git >/dev/null 2>&1 && GIT_SSL_CAINFO=/ca.crt git clone --quiet https://"+spec.DNSName+"/repo.git /tmp/clone && git -C /tmp/clone cat-file -t "+revision)
	if clone != "commit" {
		t.Fatalf("private Argo-style fetch did not resolve the reviewed revision: %q", clone)
	}

	// Zero leaked credentials: the private key is owner-only on the host and is
	// never mirrored into the repository the remote serves.
	keyInfo, err := os.Stat(filepath.Join(spec.TLSDir(), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	if keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("server key mode = %v, want 0600", keyInfo.Mode().Perm())
	}
	served := runExternal(t, "", nil, "git", "--git-dir", spec.RepoDir(), "log", "--all", "-p")
	if strings.Contains(served, "PRIVATE KEY") {
		t.Fatalf("served repository leaks a private key")
	}

	// Validated teardown preserves repository data.
	if err := remote.Down(context.Background()); err != nil {
		t.Fatalf("fetch remote down: %v", err)
	}
	if out, err := exec.Command("docker", "inspect", spec.ContainerName).CombinedOutput(); err == nil {
		t.Fatalf("container survived teardown: %s", out)
	}
	if _, err := os.Stat(filepath.Join(spec.RepoDir(), "HEAD")); err != nil {
		t.Fatalf("teardown did not preserve the mirror: %v", err)
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
		Destination: filepath.Join(root, "deployments", "modules", "payments"),
		Module:      "payments", UnitNames: services, Environment: environment,
		AppProject: "payments", OwnedPath: "environments/deployments/modules/payments",
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
