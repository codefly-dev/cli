package ci

import (
	"context"
	"encoding/hex"
	"errors"
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
)

func TestCICacheIdentityIsStableAndInvalidatesTaskDimensions(t *testing.T) {
	_, workspace := loadSchedulerFixture(t)
	plan := cacheTestPlan(workspace, "management/consumer")
	base := ScheduleOptions{Phase: "compile", RuntimeContext: "native"}

	first := preparedCacheIdentity(t, workspace, plan, base, "1.2.3")
	second := preparedCacheIdentity(t, workspace, plan, base, "1.2.3")
	if first.Status != cacheStatusIdentityOnly || first.Key == "" {
		t.Fatalf("cache identity = %#v", first)
	}
	if first.Key != second.Key {
		t.Fatalf("unchanged cache key drifted: %s != %s", first.Key, second.Key)
	}
	if len(strings.TrimPrefix(first.Key, "sha256:")) != 64 {
		t.Fatalf("cache key is not a SHA-256 identity: %q", first.Key)
	}

	phase := preparedCacheIdentity(t, workspace, plan, ScheduleOptions{Phase: "lint", RuntimeContext: "native"}, "1.2.3")
	if phase.Key == first.Key {
		t.Fatal("phase did not invalidate cache identity")
	}
	runtimeIdentity := preparedCacheIdentity(t, workspace, plan, ScheduleOptions{Phase: "compile", RuntimeContext: "nix"}, "1.2.3")
	if runtimeIdentity.Key == first.Key {
		t.Fatal("runtime context did not invalidate cache identity")
	}
	toolVersion := preparedCacheIdentity(t, workspace, plan, base, "1.2.4")
	if toolVersion.Key == first.Key {
		t.Fatal("Codefly version did not invalidate cache identity")
	}
	unit := preparedCacheIdentity(t, workspace, plan, ScheduleOptions{Phase: "test", Suite: "unit", RuntimeContext: "native"}, "1.2.3")
	integration := preparedCacheIdentity(t, workspace, plan, ScheduleOptions{Phase: "test", Suite: "integration", RuntimeContext: "native"}, "1.2.3")
	if unit.Key == integration.Key {
		t.Fatal("test suite did not invalidate cache identity")
	}
}

func TestCICacheIdentityInvalidatesSourceDependencyAndWorkspaceInputs(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	plan := cacheTestPlan(workspace, "management/consumer")
	options := ScheduleOptions{Phase: "compile", RuntimeContext: "native"}
	baseline := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")

	consumerSource := filepath.Join(root, "modules", "management", "services", "consumer", "code", "consumer.txt")
	writeCacheTestFile(t, consumerSource, "consumer v1")
	consumerChanged := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if consumerChanged.Key == baseline.Key {
		t.Fatal("target source did not invalidate cache identity")
	}

	ignoredOutput := filepath.Join(root, "modules", "management", "services", "consumer", "code", "node_modules", "generated.js")
	writeCacheTestFile(t, ignoredOutput, "generated output")
	ignored := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if ignored.Key != consumerChanged.Key {
		t.Fatal("transient dependency output invalidated cache identity")
	}

	workerSource := filepath.Join(root, "modules", "management", "services", "worker", "code", "worker.txt")
	writeCacheTestFile(t, workerSource, "unrelated worker")
	unrelated := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if unrelated.Key != consumerChanged.Key {
		t.Fatal("unrelated service source invalidated cache identity")
	}

	organizationSource := filepath.Join(root, "modules", "management", "services", "organization", "code", "organization.txt")
	writeCacheTestFile(t, organizationSource, "upstream contract v2")
	dependencyChanged := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if dependencyChanged.Key == unrelated.Key {
		t.Fatal("transitive service dependency did not invalidate cache identity")
	}

	workspacePath := filepath.Join(root, resources.WorkspaceConfigurationName)
	workspaceConfig, err := os.ReadFile(workspacePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspacePath, append(workspaceConfig, []byte("\ndescription: cache invalidation\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	workspaceChanged := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if workspaceChanged.Key == dependencyChanged.Key {
		t.Fatal("workspace configuration did not invalidate cache identity")
	}
}

func TestCICacheIdentityInvalidatesInternalLibraryClosure(t *testing.T) {
	root, workspace := loadSchedulerFixture(t)
	consumerConfig := filepath.Join(root, "modules", "management", "services", "consumer", resources.ServiceConfigurationName)
	payload, err := os.ReadFile(consumerConfig)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte("library-dependencies:\n    - name: shared-models\n      version: 1.0.0\n      languages: [go]\n")...)
	if err := os.WriteFile(consumerConfig, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	libraryConfig := `kind: library
name: shared-models
version: 1.0.0
languages:
    - name: go
      agent: ""
      path: go/
      exports: [example/shared-models]
`
	writeCacheTestFile(t, filepath.Join(root, "libraries", "shared-models", resources.LibraryConfigurationName), libraryConfig)
	librarySource := filepath.Join(root, "libraries", "shared-models", "go", "model.go")
	writeCacheTestFile(t, librarySource, "package model\n")

	plan := cacheTestPlan(workspace, "management/consumer")
	options := ScheduleOptions{Phase: "compile", RuntimeContext: "native"}
	first := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if len(first.Inputs.Libraries) != 1 || first.Inputs.Libraries[0].Resource != "shared-models" {
		t.Fatalf("library inputs = %#v", first.Inputs.Libraries)
	}
	writeCacheTestFile(t, librarySource, "package model\n\nconst Version = 2\n")
	second := preparedCacheIdentity(t, workspace, plan, options, "1.2.3")
	if second.Key == first.Key {
		t.Fatal("internal library source did not invalidate cache identity")
	}
}

func TestCacheContentDigestUsesPathsAndExcludesGitIgnoredFiles(t *testing.T) {
	root := t.TempDir()
	runCacheTestGit(t, root, "init")
	writeCacheTestFile(t, filepath.Join(root, ".gitignore"), "ignored/\n*.tsbuildinfo\n")
	writeCacheTestFile(t, filepath.Join(root, "source", "input.txt"), "same content")
	writeCacheTestFile(t, filepath.Join(root, "source", "renamed.txt"), "same content")
	runCacheTestGit(t, root, "add", ".gitignore", "source/input.txt", "source/renamed.txt")

	files, err := gitCacheFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	builder := &ciCacheIdentityBuilder{repoRoot: cleanAbs(root), gitFiles: files, useGitFiles: true, digestCache: map[string]string{}}
	first, err := builder.digestPath(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, filepath.Join(root, "ignored", "result.txt"), "build output")
	writeCacheTestFile(t, filepath.Join(root, "source", "cache.tsbuildinfo"), "incremental output")
	files, err = gitCacheFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	builder = &ciCacheIdentityBuilder{repoRoot: cleanAbs(root), gitFiles: files, useGitFiles: true, digestCache: map[string]string{}}
	ignored, err := builder.digestPath(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	if ignored != first {
		t.Fatal("Git-ignored/transient output changed content digest")
	}

	if err := os.Rename(filepath.Join(root, "source", "renamed.txt"), filepath.Join(root, "source", "moved.txt")); err != nil {
		t.Fatal(err)
	}
	runCacheTestGit(t, root, "add", "-A")
	files, err = gitCacheFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	builder = &ciCacheIdentityBuilder{repoRoot: cleanAbs(root), gitFiles: files, useGitFiles: true, digestCache: map[string]string{}}
	renamed, err := builder.digestPath(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	if renamed == first {
		t.Fatal("file rename did not invalidate content digest")
	}
}

func TestCICacheIdentityInstallsAPinnedAgentMissingFromTheMachine(t *testing.T) {
	_, workspace := loadReuseFixtureWithoutAgents(t)

	var installed []string
	stubCacheAgentInstall(t, func(ctx context.Context, agent *resources.Agent) error {
		installed = append(installed, agent.Identifier())
		return writeCacheTestAgentBinary(ctx, agent)
	})

	ctx := context.Background()
	options := ScheduleOptions{Phase: "compile", RuntimeContext: "native"}
	builder := newCICacheIdentityBuilder(ctx, workspace, "1.2.3", "runner-image@sha256:test")
	identity := builder.identity(ctx, options, PlannedService{Service: "management/consumer"})

	if len(installed) != 1 || installed[0] != "codefly.ai/go-grpc:0.0.16" {
		t.Fatalf("installed agents = %v", installed)
	}
	if identity.Inputs.Agent.Digest == "" {
		t.Fatalf("agent digest is unbound: %#v", identity.Inputs.Agent)
	}
	if eligible, reason := identity.reuseEligibility(); !eligible {
		t.Fatalf("identity is ineligible: %s (limitations %v)", reason, identity.Limitations)
	}

	sharing := builder.identity(ctx, options, PlannedService{Service: "management/worker"})
	if len(installed) != 1 {
		t.Fatalf("shared agent pin installed %d times", len(installed))
	}
	if sharing.Inputs.Agent.Digest != identity.Inputs.Agent.Digest {
		t.Fatalf("shared agent pin bound different digests: %s != %s", sharing.Inputs.Agent.Digest, identity.Inputs.Agent.Digest)
	}
}

func TestCICacheIdentityStaysIneligibleWhenThePinnedAgentCannotBeInstalled(t *testing.T) {
	_, workspace := loadReuseFixtureWithoutAgents(t)
	stubCacheAgentInstall(t, func(context.Context, *resources.Agent) error {
		return errors.New("release asset is not published")
	})

	ctx := context.Background()
	builder := newCICacheIdentityBuilder(ctx, workspace, "1.2.3", "runner-image@sha256:test")
	identity := builder.identity(ctx, ScheduleOptions{Phase: "compile", RuntimeContext: "native"}, PlannedService{Service: "management/consumer"})

	if identity.Inputs.Agent.Digest != "" {
		t.Fatalf("agent digest was bound without an installed binary: %q", identity.Inputs.Agent.Digest)
	}
	eligible, reason := identity.reuseEligibility()
	if eligible {
		t.Fatal("identity with an uninstallable agent is eligible for reuse")
	}
	if !strings.Contains(reason, "release asset is not published") {
		t.Fatalf("eligibility reason = %q", reason)
	}
}

// TestCacheAgentResolutionPrefersAConfiguredRegistryOverAGitHubRelease pins the
// order manager.Load resolves in. Installing the GitHub release into the local
// cache instead would make Load skip the registry on every later run, because
// Load takes the local path whenever it holds an executable.
func TestCacheAgentResolutionPrefersAConfiguredRegistryOverAGitHubRelease(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	binary := []byte("registry agent binary")
	digest := "sha256:" + hex.EncodeToString(sha256Sum(binary))
	registry := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.Contains(request.URL.Path, "/manifests/"):
			writer.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			fmt.Fprintf(writer, `{"schemaVersion":2,"layers":[{"digest":%q}]}`, digest)
		case strings.Contains(request.URL.Path, "/blobs/"):
			_, _ = writer.Write(binary)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(registry.Close)
	t.Setenv("AGENT_REGISTRY", strings.TrimPrefix(registry.URL, "http://"))

	downloaded := 0
	stubCacheAgentDownload(t, func(context.Context, *resources.Agent) error {
		downloaded++
		return nil
	})

	ctx := context.Background()
	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.ai", Name: "go-grpc", Version: "0.0.16"}
	path, err := resolveAgentBinary(ctx, agent)
	if err != nil {
		t.Fatalf("resolve agent binary: %v", err)
	}
	if downloaded != 0 {
		t.Fatal("a configured registry was bypassed for the GitHub release")
	}
	expected, err := agent.Path(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if path != expected {
		t.Fatalf("resolved path = %q, want %q", path, expected)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(binary) {
		t.Fatalf("resolved binary = %q, want the registry blob", content)
	}
}

// TestCacheAgentInstallRefusesAnUnreachableReleaseAssetBeforeDownloading covers
// the one stall manager.Download cannot be interrupted through: it ignores both
// the deadline and the cancellation of the context it is handed.
func TestCacheAgentInstallRefusesAnUnreachableReleaseAssetBeforeDownloading(t *testing.T) {
	assets := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(assets.Close)
	previousURL := githubAssetURL
	githubAssetURL = func(*resources.Agent) (string, error) { return assets.URL + "/service-go-grpc.tar.gz", nil }
	t.Cleanup(func() { githubAssetURL = previousURL })

	downloaded := 0
	stubCacheAgentDownload(t, func(context.Context, *resources.Agent) error {
		downloaded++
		return nil
	})

	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.ai", Name: "go-grpc", Version: "0.0.16"}
	err := installAgentRelease(context.Background(), agent)
	if err == nil {
		t.Fatal("an unreachable release asset was accepted")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("install error = %v", err)
	}
	if downloaded != 0 {
		t.Fatal("the download ran against an asset that is not there")
	}
}

func stubCacheAgentDownload(t *testing.T, download func(context.Context, *resources.Agent) error) {
	t.Helper()
	previous := downloadCacheAgent
	downloadCacheAgent = download
	t.Cleanup(func() { downloadCacheAgent = previous })
}

// TestCacheAgentResolutionReinstallsANonExecutableCacheEntry matches what
// execution does with such an entry: manager.Load resolves through
// exec.LookPath, so it ignores the file and fetches the agent again. Binding
// the bytes of a file the task will not run would publish a record under a
// digest no execution ever produced.
func TestCacheAgentResolutionReinstallsANonExecutableCacheEntry(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	ctx := context.Background()
	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "codefly.ai", Name: "go-grpc", Version: "0.0.16"}
	path, err := agent.Path(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeCacheTestFile(t, path, "not executable")

	installed := 0
	stubCacheAgentInstall(t, func(installContext context.Context, target *resources.Agent) error {
		installed++
		return writeCacheTestAgentBinary(installContext, target)
	})
	if _, err := resolveAgentBinary(ctx, agent); err != nil {
		t.Fatalf("resolve agent binary: %v", err)
	}
	if installed != 1 {
		t.Fatalf("non-executable cache entry was bound instead of reinstalled (installs = %d)", installed)
	}
}

func stubCacheAgentInstall(t *testing.T, install func(context.Context, *resources.Agent) error) {
	t.Helper()
	previous := installCacheAgent
	installCacheAgent = install
	t.Cleanup(func() { installCacheAgent = previous })
}

func writeCacheTestAgentBinary(ctx context.Context, agent *resources.Agent) error {
	path, err := agent.Path(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("agent "+agent.Identifier()), 0o755)
}

func cacheTestPlan(workspace *resources.Workspace, service string) *Plan {
	return &Plan{
		SchemaVersion: planSchemaVersion,
		Workspace:     workspace.Name,
		ChangedFiles:  []string{},
		Services:      []PlannedService{{Service: service, Classification: "direct", Reasons: []string{"cache test"}}},
	}
}

func preparedCacheIdentity(t *testing.T, workspace *resources.Workspace, plan *Plan, options ScheduleOptions, version string) CICacheIdentity {
	t.Helper()
	fixed := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	reporter, err := newCIReporter(plan, "codefly ci run", version, func() time.Time { return fixed })
	if err != nil {
		t.Fatal(err)
	}
	options.Reporter = reporter
	if err := prepareCIReportTasks(context.Background(), workspace, plan, options); err != nil {
		t.Fatal(err)
	}
	report := reporter.Finalize(nil)
	if len(report.Tasks) != 1 {
		t.Fatalf("report task count = %d, want 1", len(report.Tasks))
	}
	identity := report.Tasks[0].Cache
	if identity.Status == cacheStatusUnavailable {
		t.Fatalf("cache identity unavailable: %v", identity.Limitations)
	}
	return identity
}

func writeCacheTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCacheTestGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
