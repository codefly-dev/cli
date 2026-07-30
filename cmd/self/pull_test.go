package self

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// initRepoWithRemote creates a git repo at root/name with the given origin URL.
func initRepoWithRemote(t *testing.T, root, name, originURL string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if originURL != "" {
		run("remote", "add", "origin", originURL)
	}
}

func markPlugin(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name, "agent.codefly.yaml"), []byte("name: example\n"), 0o644); err != nil {
		t.Fatalf("write plugin manifest: %v", err)
	}
}

func TestPullTargetsIncludesFoundationAndCanonicalPluginsOnly(t *testing.T) {
	root := t.TempDir()

	initRepoWithRemote(t, root, "core", "git@github.com:codefly-dev/core.git")
	initRepoWithRemote(t, root, "cli", "https://github.com/codefly-dev/cli.git")
	initRepoWithRemote(t, root, "llm", "git@github.com:codefly-dev/llm.git")

	initRepoWithRemote(t, root, "service-go", "git@github.com:codefly-dev/service-go.git")
	markPlugin(t, root, "service-go")
	initRepoWithRemote(t, root, "third-party-plugin", "https://github.com:partner/third-party-plugin.git")
	markPlugin(t, root, "third-party-plugin")

	// Codefly repositories outside the explicit foundation are not pulled.
	initRepoWithRemote(t, root, "sdk-go", "git@github.com:codefly-dev/sdk-go.git")
	initRepoWithRemote(t, root, "landing", "git@github.com:codefly-dev/landing.git")

	// Duplicate task checkouts may still contain a plugin manifest, but only
	// the canonical checkout matching the origin repository is touched.
	initRepoWithRemote(t, root, "service-go-experiment", "git@github.com:codefly-dev/service-go.git")
	markPlugin(t, root, "service-go-experiment")

	names, err := pullTargets(context.Background(), root)
	if err != nil {
		t.Fatalf("pullTargets: %v", err)
	}
	want := []string{"cli", "core", "llm", "service-go", "third-party-plugin"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("pullTargets = %v, want %v", names, want)
	}
}

func TestDiscoverCanonicalPluginRepos(t *testing.T) {
	root := t.TempDir()

	for _, repo := range []struct {
		name   string
		origin string
	}{
		{name: "service-postgres-docker-retry", origin: "git@github.com:codefly-dev/service-postgres.git"},
		{name: "toolbox-docker", origin: "git@github.com:codefly-dev/toolbox-docker.git"},
		{name: "module-saas-starter-onboarding-library-v2", origin: "https://github.com/codefly-dev/module-saas-starter.git"},
		{name: "application-flux", origin: "https://github.com/codefly-dev/application-flux.git"},
		{name: "service-postgres", origin: "https://github.com/codefly-dev/service-postgres.git"},
		{name: "module-saas-starter-onboarding-library", origin: "git@github.com:codefly-dev/module-saas-starter.git"},
		{name: "module-saas-starter", origin: "git@github.com:codefly-dev/module-saas-starter.git"},
	} {
		initRepoWithRemote(t, root, repo.name, repo.origin)
		markPlugin(t, root, repo.name)
	}

	initRepoWithRemote(t, root, "sdk-go", "git@github.com:codefly-dev/sdk-go.git")
	if err := os.MkdirAll(filepath.Join(root, "service-not-a-repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	markPlugin(t, root, "service-not-a-repo")

	plugins, skipped, err := discoverCanonicalPluginRepos(context.Background(), root)
	if err != nil {
		t.Fatalf("discoverCanonicalPluginRepos: %v", err)
	}

	var names []string
	for _, plugin := range plugins {
		names = append(names, plugin.label)
	}
	wantNames := []string{
		"application-flux",
		"module-saas-starter",
		"service-postgres",
		"toolbox-docker",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("canonical plugins = %v, want %v", names, wantNames)
	}

	wantSkipped := []skippedPluginCheckout{
		{name: "module-saas-starter-onboarding-library", originRepository: "module-saas-starter"},
		{name: "module-saas-starter-onboarding-library-v2", originRepository: "module-saas-starter"},
		{name: "service-postgres-docker-retry", originRepository: "service-postgres"},
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("skipped plugins = %#v, want %#v", skipped, wantSkipped)
	}
}

func TestRemoteRepositoryName(t *testing.T) {
	for remote, want := range map[string]string{
		"git@github.com:codefly-dev/service-go.git":      "service-go",
		"https://github.com/codefly-dev/toolbox-web.git": "toolbox-web",
		"/tmp/repos/module-example/":                     "module-example",
	} {
		if got := remoteRepositoryName(remote); got != want {
			t.Errorf("remoteRepositoryName(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestPullRepoSkipsFeatureBranchBeforeFetch(t *testing.T) {
	root := t.TempDir()
	initRepoWithRemote(t, root, "service-go", "file:///definitely/missing/service-go.git")
	repo := filepath.Join(root, "service-go")

	checkout := exec.Command("git", "checkout", "-q", "-b", "feature/local-work")
	checkout.Dir = repo
	if out, err := checkout.CombinedOutput(); err != nil {
		t.Fatalf("create feature branch: %v\n%s", err, out)
	}

	result, err := pullRepo(context.Background(), repo, "origin", "main")
	if err != nil {
		t.Fatalf("pullRepo should skip before fetching the missing remote: %v", err)
	}
	if result.kind != pullResultSkipped {
		t.Fatalf("pullRepo result kind = %v, want skipped", result.kind)
	}
	want := "skipped (on feature/local-work; expected branch main)"
	if result.message != want {
		t.Fatalf("pullRepo message = %q, want %q", result.message, want)
	}
}
