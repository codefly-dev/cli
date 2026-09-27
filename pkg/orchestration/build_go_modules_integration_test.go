//go:build integration

package orchestration

import (
	"context"
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
