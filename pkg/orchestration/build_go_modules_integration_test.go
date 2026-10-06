//go:build integration

package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// privateModulePath is a module only the prefetch can reach: it is served from
// a directory on the host, standing in for a private repository whose
// credential the host holds and the image build does not.
const privateModulePath = "example.com/private/greeting"

// privateModuleSource writes a Go module proxy tree serving privateModulePath
// v1.0.0 and returns its directory.
func privateModuleSource(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	moduleDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module "+privateModulePath+"\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, "greeting.go"), []byte("package greeting\n\nconst Text = \"hello from a private module\"\n"), 0o644))

	versions := filepath.Join(source, filepath.FromSlash(privateModulePath), "@v")
	require.NoError(t, os.MkdirAll(versions, 0o755))
	zip, err := os.Create(filepath.Join(versions, "v1.0.0.zip"))
	require.NoError(t, err)
	require.NoError(t, modzip.CreateFromDir(zip, module.Version{Path: privateModulePath, Version: "v1.0.0"}, moduleDir))
	require.NoError(t, zip.Close())
	gomod, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(versions, "v1.0.0.mod"), gomod, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(versions, "v1.0.0.info"), []byte(`{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(versions, "list"), []byte("v1.0.0\n"), 0o644))
	return source
}

// goModuleContext is a service build context whose module requires the private
// module, with a go.sum resolved against the host-only source, and a
// Dockerfile shaped like a Go agent's recipe: a `gomodproxy` stage the caller's
// named context replaces, read as the only GOPROXY for the dependency download.
func goModuleContext(t *testing.T, source string) string {
	t.Helper()
	contextDir := t.TempDir()
	code := filepath.Join(contextDir, "code")
	require.NoError(t, os.MkdirAll(code, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(code, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n\nrequire "+privateModulePath+" v1.0.0\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(code, "main.go"), []byte("package main\n\nimport (\n\t\"fmt\"\n\n\t\""+privateModulePath+"\"\n)\n\nfunc main() { fmt.Println(greeting.Text) }\n"), 0o644))

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = code
	tidy.Env = append(os.Environ(), "GOPROXY=file://"+source, "GOSUMDB=off", "GOFLAGS=-mod=mod", "GOWORK=off", "GOMODCACHE="+t.TempDir())
	out, err := tidy.CombinedOutput()
	require.NoError(t, err, string(out))
	cleanModCache(t, tidy.Env)

	dockerfile := `FROM scratch AS gomodproxy

FROM golang:1.27-alpine
WORKDIR /app
COPY code/go.mod code/go.sum code/
RUN --mount=type=bind,from=gomodproxy,target=/gomodproxy \
    cd code && GOPROXY=file:///gomodproxy GOSUMDB=off GOFLAGS=-mod=readonly go mod download
COPY code code
RUN --network=none cd code && GOPROXY=off GOFLAGS=-mod=readonly go build -o /app/app .
`
	require.NoError(t, os.WriteFile(filepath.Join(contextDir, "Dockerfile"), []byte(dockerfile), 0o644))
	return contextDir
}

// cleanModCache makes a test's module cache removable: the go tool writes it
// read-only.
func cleanModCache(t *testing.T, env []string) {
	for _, entry := range env {
		if cache, ok := strings.CutPrefix(entry, "GOMODCACHE="); ok {
			clean := exec.Command("go", "clean", "-modcache")
			clean.Env = env
			_ = clean.Run()
			_ = os.RemoveAll(cache)
		}
	}
}

// TestAGoModuleBuildNeedsNoCredentialWithThePrefetchedProxy is the end-to-end
// claim: the host fetches a module the build cannot reach, and the image build —
// given no credential and no route to that module's source — builds from the
// prefetched proxy alone. Without the proxy the same build fails, so it is the
// prefetch, not a leaked route, that made it succeed.
func TestAGoModuleBuildNeedsNoCredentialWithThePrefetchedProxy(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the prefetch runs the host go toolchain")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	source := privateModuleSource(t)
	contextDir := goModuleContext(t, source)

	// The host reaches the private source; the build never does.
	t.Setenv("GOPROXY", "file://"+source)
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOPRIVATE", "")
	prefetch := newGoModulePrefetch()
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	recipe := &builderv0.DockerBuildRecipe{
		Name: "app", Dockerfile: "Dockerfile", Context: ".", Image: "codefly-go-module-prefetch-test:latest",
		Platforms:         []string{"linux/" + goArch(t)},
		GoModuleDownloads: []*builderv0.GoModuleDownload{{ModuleRoot: "code", ProxyContext: "gomodproxy"}},
	}
	proxies, err := prefetch.proxiesFor(ctx, contextDir, recipe)
	require.NoError(t, err)
	require.DirExists(t, filepath.Join(proxies["gomodproxy"], filepath.FromSlash(privateModulePath), "@v"))

	build := func(private privateModuleBuild) (string, error) {
		args, err := cachedBuildxArgs(recipe, filepath.Join(contextDir, "Dockerfile"), contextDir, false, false, "", "", nil, private)
		require.NoError(t, err)
		require.NotContains(t, args, "--secret", "no credential reaches the build")
		command := exec.CommandContext(ctx, "docker", args...)
		out, err := command.CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { _ = exec.Command("docker", "image", "rm", "-f", recipe.GetImage()).Run() })

	out, err := build(privateModuleBuild{}.withProxies(proxies))
	require.NoError(t, err, "the build must succeed from the prefetched proxy:\n%s", out)

	out, err = build(privateModuleBuild{})
	require.Error(t, err, "without the proxy the build has no route to the module:\n%s", out)
	require.Contains(t, out, privateModulePath)
}

func goArch(t *testing.T) string {
	out, err := exec.Command("docker", "info", "--format", "{{.Architecture}}").Output()
	require.NoError(t, err)
	switch arch := strings.TrimSpace(string(out)); arch {
	case "aarch64", "arm64":
		return "arm64"
	default:
		return "amd64"
	}
}

// privateVCSModulePath is a module the prefetch can only reach through git, the
// way a private repository is reached. The `.git` suffix is what lets the go
// tool resolve the path to a repository URL with no meta-tag lookup, and the
// test's own git configuration rewrites that URL to a repository on disk.
const privateVCSModulePath = "example.com/private/lib.git"

// privateVCSRepository writes a bare git repository standing in for the private
// repository two services pin, and returns it with the commit at the tip of each
// of two branches.
//
// Its shape is what the fetch under test needs. Each pinned commit is a ref tip,
// because the go tool fetches a commit it can name a ref for — and only that
// fetch is shallow (`--depth=1`). The repository carries a tag that both commits
// descend from, because that is what makes their pseudo-versions name a base
// version, and validating that base is what makes the go tool deepen the shallow
// clone it just made: every ref, then `--unshallow`. That is the fetch
// codefly-dev/cli#886 fails in.
func privateVCSRepository(t *testing.T) (string, [2]string) {
	t.Helper()
	source := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = source
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=codefly", "GIT_AUTHOR_EMAIL=codefly@example.com",
			"GIT_COMMITTER_NAME=codefly", "GIT_COMMITTER_EMAIL=codefly@example.com")
		out, err := command.CombinedOutput()
		require.NoError(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}
	commit := func(constant int, message string) string {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(source, "lib.go"),
			[]byte(fmt.Sprintf("package lib\n\nconst Version = %d\n", constant)), 0o644))
		git("add", "-A")
		git("commit", "-m", message)
		return git("rev-parse", "HEAD")
	}

	git("init", "-q", "-b", "main", ".")
	require.NoError(t, os.WriteFile(filepath.Join(source, "go.mod"),
		[]byte("module "+privateVCSModulePath+"\n\ngo 1.22\n"), 0o644))
	base := commit(1, "the released version")
	git("tag", "v0.0.1")
	// Two commits a pseudo-version can name, each at the tip of its own branch
	// and each descended from the tag.
	first := commit(2, "what one service pins")
	git("checkout", "-q", "-b", "other", base)
	second := commit(3, "what the other service pins")
	git("checkout", "-q", "main")

	repository := filepath.Join(t.TempDir(), "lib.git")
	clone := exec.Command("git", "clone", "-q", "--bare", source, repository)
	out, err := clone.CombinedOutput()
	require.NoError(t, err, string(out))
	// A commit named by hash is fetched only from a server that allows it.
	// GitHub does; this repository has to say so, or the prefetch's clone is
	// never shallow and the fetch under test is never the one that fails.
	config := exec.Command("git", "-C", repository, "config", "uploadpack.allowAnySHA1InWant", "true")
	out, err = config.CombinedOutput()
	require.NoError(t, err, string(out))
	return repository, [2]string{first, second}
}

// reachPrivateVCSThroughGit points the host's git at the repository on disk for
// the rest of the test, as a developer's or a CI job's `insteadOf` points it at
// a private repository it holds a credential for.
func reachPrivateVCSThroughGit(t *testing.T, repository string) {
	t.Helper()
	configuration := filepath.Join(t.TempDir(), "gitconfig")
	// Both spellings: the go tool drops the `.git` suffix for some of the URLs
	// it tries, and git rewrites the longest match.
	require.NoError(t, os.WriteFile(configuration, []byte(fmt.Sprintf(
		"[url \"file://%s\"]\n\tinsteadOf = https://%s\n\tinsteadOf = https://%s\n",
		repository, privateVCSModulePath, strings.TrimSuffix(privateVCSModulePath, ".git"))), 0o644))
	t.Setenv("GIT_CONFIG_GLOBAL", configuration)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GOPRIVATE", "example.com/*")
	t.Setenv("GONOSUMDB", "example.com/*")
	t.Setenv("GOPROXY", "direct")
	t.Setenv("GOSUMDB", "off")
}

// servicePinning writes a service's module root requiring the private module at
// one commit, resolved to the pseudo-version the go tool computes, and returns
// that version.
func servicePinning(t *testing.T, root, commit string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/"+filepath.Base(root)+"\n\ngo 1.22\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte(
		"package main\n\nimport (\n\t\"fmt\"\n\n\tlib \""+privateVCSModulePath+"\"\n)\n\nfunc main() { fmt.Println(lib.Version) }\n"), 0o644))

	cache := t.TempDir()
	env := append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off", "GOMODCACHE="+cache)
	get := exec.Command("go", "get", privateVCSModulePath+"@"+commit)
	get.Dir, get.Env = root, env
	out, err := get.CombinedOutput()
	require.NoError(t, err, string(out))
	t.Cleanup(func() { cleanModCache(t, env) })

	list := exec.Command("go", "list", "-m", "-f", "{{.Version}}", privateVCSModulePath)
	list.Dir, list.Env = root, env
	version, err := list.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(version))
}

// gitChildren is every command the gits in a GIT_TRACE2_EVENT log launched.
func gitChildren(t *testing.T, trace string) [][]string {
	t.Helper()
	content, err := os.ReadFile(trace)
	require.NoError(t, err, "git wrote no trace: the fetch under test was not observed")
	var children [][]string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || len(event.Argv) == 0 {
			continue
		}
		children = append(children, event.Argv)
	}
	return children
}

// childRuns reports whether any of those commands carries every one of words as
// an argument — the command name itself matching whether git invoked it by name
// or by its path in git-core.
func childRuns(children [][]string, words ...string) bool {
	for _, argv := range children {
		found := 0
		for _, word := range words {
			for _, argument := range argv {
				if argument == word || strings.HasSuffix(argument, "/"+word) {
					found++
					break
				}
			}
		}
		if found == len(words) {
			return true
		}
	}
	return false
}

// TestTwoServicesPinningOneRepositoryFetchWithoutGitMaintainingTheCache is
// codefly-dev/cli#886: two services of one flow pin one private repository at
// two pseudo-versions, and the prefetch has to fetch both.
//
// Fetching a pseudo-version makes the go tool deepen the shallow clone it made:
// every ref, then `git fetch --unshallow`. The first of those fetches ends by
// detaching `git maintenance run --auto`, which repacks the clone — and rewrites
// .git/shallow while the unshallow is working from it, so git refuses the fetch
// ("shallow file has changed since we read it") and the go tool reports an
// invalid pseudo-version, failing a render that was only downloading modules.
//
// So this asserts both: that the fetch the bug lives in is the fetch this test
// performs (the clone was shallow and was deepened), and that no automatic
// maintenance ran in the prefetch's cache to race it.
func TestTwoServicesPinningOneRepositoryFetchWithoutGitMaintainingTheCache(t *testing.T) {
	for _, tool := range []string{"go", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("the prefetch runs the host %s", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	repository, commits := privateVCSRepository(t)
	reachPrivateVCSThroughGit(t, repository)

	contextDir := t.TempDir()
	versions := [2]string{
		servicePinning(t, filepath.Join(contextDir, "one"), commits[0]),
		servicePinning(t, filepath.Join(contextDir, "other"), commits[1]),
	}
	require.NotEqual(t, versions[0], versions[1], "the two services must pin the one repository at two versions")

	trace := filepath.Join(t.TempDir(), "git-trace.json")
	t.Setenv("GIT_TRACE2_EVENT", trace)

	prefetch := newGoModulePrefetch()
	t.Cleanup(func() { require.NoError(t, prefetch.Close()) })
	proxies, err := prefetch.proxiesFor(ctx, contextDir, &builderv0.DockerBuildRecipe{
		Name: "app", GoModuleDownloads: []*builderv0.GoModuleDownload{
			{ModuleRoot: "one", ProxyContext: "oneproxy"},
			{ModuleRoot: "other", ProxyContext: "otherproxy"},
		},
	})
	require.NoError(t, err, "both versions of the one repository must fetch")

	for _, version := range versions {
		require.FileExists(t, filepath.Join(proxies["oneproxy"],
			filepath.FromSlash(privateVCSModulePath), "@v", version+".info"),
			"the proxy handed to the build carries the version this service pins")
	}

	children := gitChildren(t, trace)
	require.True(t, childRuns(children, "fetch", "--depth=1"),
		"the clone must be shallow, or this test is not the fetch that fails")
	require.True(t, childRuns(children, "fetch", "--unshallow"),
		"the shallow clone must be deepened, or this test is not the fetch that fails")
	require.False(t, childRuns(children, "maintenance", "--auto"),
		"a `git fetch` in the prefetch's cache must detach no maintenance to rewrite the clone under the next fetch")
	require.False(t, childRuns(children, "gc", "--auto"),
		"nor reach the same repacking as the gc an older git runs")
}
