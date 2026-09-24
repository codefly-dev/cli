package agents

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/stretchr/testify/require"
)

func writePinFixture(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func publishPinFixture(t *testing.T, proxy, path, version, module, source string) {
	t.Helper()
	dir := filepath.Join(proxy, filepath.FromSlash(path), "@v")
	writePinFixture(t, filepath.Join(dir, version+".mod"), module)
	info, err := json.Marshal(map[string]string{"Version": version, "Time": "2026-01-01T00:00:00Z"})
	require.NoError(t, err)
	writePinFixture(t, filepath.Join(dir, version+".info"), string(info))
	writePinFixture(t, filepath.Join(dir, "list"), "v0.0.1\nv0.0.2\n")
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	for name, content := range map[string]string{"go.mod": module, "value.go": source} {
		file, err := archive.Create(path + "@" + version + "/" + name)
		require.NoError(t, err)
		_, err = file.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	writePinFixture(t, filepath.Join(dir, version+".zip"), buffer.String())
}

// A changed Core API and its SDK consumer must move together, including the
// generated service's locks. Exercise the real Go resolver and compiler.
func TestDependencyPinUpdatesOwnedLocksTogetherAndRollsBackFailure(t *testing.T) {
	proxy := t.TempDir()
	const sdk = "example.test/sdk"
	for _, version := range []string{"v0.0.1", "v0.0.2"} {
		coreSource := "package core\nfunc Value() int { return 42 }\n"
		sdkSource := "package sdk\nimport core \"" + coreModule + "\"\nfunc Value() int { return core.Value() }\n"
		if version == "v0.0.2" {
			coreSource = "package core\nfunc Value(value int) int { return value }\n"
			sdkSource = "package sdk\nimport core \"" + coreModule + "\"\nfunc Value() int { return core.Value(42) }\n"
		}
		publishPinFixture(t, proxy, coreModule, version, "module "+coreModule+"\ngo 1.23\n", coreSource)
		publishPinFixture(t, proxy, sdk, version, "module "+sdk+"\ngo 1.23\nrequire "+coreModule+" "+version+"\n", sdkSource)
	}
	t.Setenv("GOPROXY", (&url.URL{Scheme: "file", Path: filepath.ToSlash(proxy)}).String())
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "none")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOMODCACHE", t.TempDir())
	root := t.TempDir()
	t.Cleanup(func() { require.NoError(t, runGo(context.Background(), root, "clean", "-modcache")) })
	base := filepath.Join(root, "base", "code")
	template := filepath.Join(root, "templates", "factory", "code")
	rootMod := "module example.test/agent\ngo 1.23\nrequire " + coreModule + " v0.0.1\n"
	baseMod := "module example.test/service\ngo 1.23\nrequire (\n" + coreModule + " v0.0.1\n" + sdk + " v0.0.1\n)\n"
	writePinFixture(t, filepath.Join(root, "go.mod"), rootMod)
	writePinFixture(t, filepath.Join(root, "agent.go"), "package agent\nimport core \""+coreModule+"\"\nvar Value = core.Value\n")
	writePinFixture(t, filepath.Join(base, "go.mod"), baseMod)
	writePinFixture(t, filepath.Join(base, "service.go"), "package service\nimport \""+sdk+"\"\nvar Value = sdk.Value\n")
	writePinFixture(t, filepath.Join(template, "go.mod.tmpl"), "module {{ .Service.Name.DNSCase }}\ngo 1.23\n")
	writePinFixture(t, filepath.Join(template, "go.sum.tmpl"), "")
	ctx, done := common.NewContext()
	defer done()
	require.ErrorContains(t, pinCore(ctx, root, "v0.0.2"), "standalone build of base/code")
	rootBytes, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err)
	require.Equal(t, rootMod, string(rootBytes))
	baseBytes, err := os.ReadFile(filepath.Join(base, "go.mod"))
	require.NoError(t, err)
	require.Equal(t, baseMod, string(baseBytes))
	_, err = os.Stat(filepath.Join(root, "go.sum"))
	require.True(t, os.IsNotExist(err))
	require.NoError(t, pinCore(ctx, root, "v0.0.2", sdk+"@v0.0.2"))
	require.Equal(t, "v0.0.2", goModVersion(root, coreModule))
	require.Equal(t, "v0.0.2", goModVersion(base, coreModule))
	require.Equal(t, "v0.0.2", goModVersion(base, sdk))
	require.Equal(t, "?", goModVersion(root, sdk), "do not add unused dependencies")
	baseBytes, err = os.ReadFile(filepath.Join(base, "go.mod"))
	require.NoError(t, err)
	templateBytes, err := os.ReadFile(filepath.Join(template, "go.mod.tmpl"))
	require.NoError(t, err)
	require.Equal(t, swapModulePath(string(baseBytes), "example.test/service", "{{ .Service.Name.DNSCase }}"), string(templateBytes))
	baseSum, err := os.ReadFile(filepath.Join(base, "go.sum"))
	require.NoError(t, err)
	templateSum, err := os.ReadFile(filepath.Join(template, "go.sum.tmpl"))
	require.NoError(t, err)
	require.Equal(t, baseSum, templateSum)
}

func TestDependencyPinsRejectInvalidOrUnmatchedSelections(t *testing.T) {
	dir := t.TempDir()
	writePinFixture(t, filepath.Join(dir, "go.mod"), "module example.test/agent\ngo 1.23\nrequire example.test/sdk v0.0.1\n")
	for _, selection := range [][]string{
		{"example.test/sdk"}, {"example.test/sdk@"}, {"example.test/sdk@v0.0.2@bad"},
		{coreModule + "@v0.0.2"}, {"example.test/typo@v0.0.2"},
		{"example.test/sdk@v0.0.2", "example.test/sdk@v0.0.1"},
	} {
		_, err := dependencyPins([]string{dir}, selection)
		require.Error(t, err)
	}
}

// A plain `go get` of a dependency bump leaves the old version's go.sum lines
// behind, and the agent CI's `go mod tidy -diff` gate reds the pin PR. The pin
// must leave every owned lock exactly as that gate expects.
func TestPinLeavesEveryOwnedLockTidyAfterStaleBump(t *testing.T) {
	proxy := t.TempDir()
	const sdk = "example.test/sdk"
	for _, version := range []string{"v0.0.1", "v0.0.2"} {
		publishPinFixture(t, proxy, coreModule, version, "module "+coreModule+"\ngo 1.23\n", "package core\nfunc Value() int { return 42 }\n")
		publishPinFixture(t, proxy, sdk, version, "module "+sdk+"\ngo 1.23\nrequire "+coreModule+" "+version+"\n",
			"package sdk\nimport core \""+coreModule+"\"\nfunc Value() int { return core.Value() }\n")
	}
	t.Setenv("GOPROXY", (&url.URL{Scheme: "file", Path: filepath.ToSlash(proxy)}).String())
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "none")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOMODCACHE", t.TempDir())
	root := t.TempDir()
	t.Cleanup(func() { require.NoError(t, runGo(context.Background(), root, "clean", "-modcache")) })
	base := filepath.Join(root, "base", "code")
	template := filepath.Join(root, "templates", "factory", "code")
	source := "import (\n\tcore \"" + coreModule + "\"\n\t\"" + sdk + "\"\n)\nvar Value = core.Value() + sdk.Value()\n"
	for dir, module := range map[string]string{root: "example.test/agent", base: "example.test/service"} {
		writePinFixture(t, filepath.Join(dir, "go.mod"), "module "+module+"\ngo 1.23\nrequire (\n"+coreModule+" v0.0.1\n"+sdk+" v0.0.1\n)\n")
		writePinFixture(t, filepath.Join(dir, "value.go"), "package value\n"+source)
	}
	writePinFixture(t, filepath.Join(template, "go.mod.tmpl"), "module {{ .Service.Name.DNSCase }}\ngo 1.23\n")
	writePinFixture(t, filepath.Join(template, "go.sum.tmpl"), "")
	ctx, done := common.NewContext()
	defer done()
	for _, dir := range []string{root, base} {
		require.NoError(t, runGo(ctx, dir, "mod", "tidy"))
		// The bump as it happens by hand: go.sum keeps the v0.0.1 lines.
		require.NoError(t, runGo(ctx, dir, "get", sdk+"@v0.0.2"))
		require.Error(t, runGo(ctx, dir, "mod", "tidy", "-diff"), "fixture must start untidy")
	}

	require.NoError(t, pinCore(ctx, root, "v0.0.2", sdk+"@v0.0.2"))
	for _, dir := range []string{root, base} {
		require.NoError(t, runGo(ctx, dir, "mod", "tidy", "-diff"))
		sum, err := os.ReadFile(filepath.Join(dir, "go.sum"))
		require.NoError(t, err)
		require.NotContains(t, string(sum), sdk+" v0.0.1 h1:")
	}
	baseSum, err := os.ReadFile(filepath.Join(base, "go.sum"))
	require.NoError(t, err)
	templateSum, err := os.ReadFile(filepath.Join(template, "go.sum.tmpl"))
	require.NoError(t, err)
	require.Equal(t, baseSum, templateSum)
}

// The final gate fails loudly on a lock tidy would rewrite.
func TestVerifyTidyRejectsStaleSum(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	dir := t.TempDir()
	writePinFixture(t, filepath.Join(dir, "go.mod"), "module example.test/agent\ngo 1.23\n")
	writePinFixture(t, filepath.Join(dir, "value.go"), "package value\n")
	ctx, done := common.NewContext()
	defer done()
	require.NoError(t, verifyTidy(ctx, dir, "."))
	writePinFixture(t, filepath.Join(dir, "go.sum"), "example.test/stale v0.0.1 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
	require.ErrorContains(t, verifyTidy(ctx, dir, "."), "the agent module is not tidy")
}
