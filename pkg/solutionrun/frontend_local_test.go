package solutionrun

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// wikiRepository is a repository holding the wiki component, whose remote
// depends on a sibling package, beside another package it does not use.
func wikiRepository(t *testing.T) (string, *resources.Module) {
	t.Helper()
	root := gitRepository(t, map[string]string{
		".gitignore":                   "*.local\nnode_modules/\n",
		"tsconfig.base.json":           "{}",
		"go.mod":                       "module example",
		"CLAUDE.md":                    "notes",
		"packages/shared/package.json": `{"name":"shared"}`,
		"packages/shared/index.ts":     "shared v1",
		"packages/other/index.ts":      "not a dependency",
		"solutions/wiki/services/fe-remote/package.json":                  `{"dependencies":{"shared":"file:../../../../packages/shared"}}`,
		"solutions/wiki/services/fe-remote/package-lock.json":             "{}",
		"solutions/wiki/services/fe-remote/Page.tsx":                      "page v1",
		"solutions/wiki/services/fe-remote/dist/mf-manifest.json":         "tracked stale bundle",
		"solutions/wiki/services/api/main.go":                             "package main",
		"solutions/wiki/services/api/" + FrontendFileName:                 "title: Wiki\n",
		"solutions/wiki/module.codefly.yaml":                              "name: wiki",
		"solutions/wiki/services/fe-remote/.env.production":               "a committed env file is source",
		"solutions/wiki/services/fe-remote/src/nested/Widget.tsx":         "widget",
		"solutions/wiki/services/fe-remote/node_modules/tracked/index.js": "vendored",
	})
	require.NoError(t, os.Symlink("CLAUDE.md", filepath.Join(root, "AGENTS.md")))
	gitCommitAll(t, root)
	module := &resources.Module{Name: "wiki", ServiceEntry: "api"}
	module.WithDir(filepath.Join(root, "solutions/wiki"))
	return root, module
}

// What a local frontend build reads is its project's files, tracked or new
// but never a new dotenv file, the sibling packages it depends on, and the
// top-level configuration above it: no other package, no Go code, no bundle,
// no node_modules, no symlink.
func TestFrontendSourcesAreWhatTheBuildReads(t *testing.T) {
	root, module := wikiRepository(t)
	for name, content := range map[string]string{
		"solutions/wiki/services/fe-remote/NewWidget.tsx":    "not yet added",
		"solutions/wiki/services/fe-remote/.env":             "untracked secrets",
		"solutions/wiki/services/fe-remote/.env.development": "untracked secrets",
		"solutions/wiki/services/fe-remote/settings.local":   "ignored",
		"solutions/wiki/services/fe-remote/node_modules/x/x": "installed",
		"packages/shared/added.ts":                           "a new sibling file",
	} {
		full := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	sources, err := selectFrontendSources(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, "solutions/wiki", sources.component)
	require.Equal(t, "solutions/wiki/services/fe-remote", sources.project)
	require.Equal(t, []string{
		".gitignore",
		"CLAUDE.md",
		"packages/shared/added.ts",
		"packages/shared/index.ts",
		"packages/shared/package.json",
		"solutions/wiki/module.codefly.yaml",
		"solutions/wiki/services/fe-remote/.env.production",
		"solutions/wiki/services/fe-remote/NewWidget.tsx",
		"solutions/wiki/services/fe-remote/Page.tsx",
		"solutions/wiki/services/fe-remote/package-lock.json",
		"solutions/wiki/services/fe-remote/package.json",
		"solutions/wiki/services/fe-remote/src/nested/Widget.tsx",
		"tsconfig.base.json",
	}, sources.files)
}

// A project at the component root reads the component's own sources, and
// never its services' code, its Go sources or its Go module files.
func TestFrontendSourcesAtTheComponentRootLeaveTheGoCodeOut(t *testing.T) {
	module := frontendModule(t, map[string]string{
		"package.json":                      "{}",
		"src/App.tsx":                       "app",
		"go.mod":                            "module example",
		"tools.go":                          "package tools",
		"services/api/main.go":              "package main",
		"services/api/service.codefly.yaml": "name: api",
		"services/fe-remote/Remote.tsx":     "remote",
	})
	sources, err := selectFrontendSources(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, []string{"package.json", "services/fe-remote/Remote.tsx", "src/App.tsx"}, sources.files)
}

// A native build reads a staged copy of exactly its sources, rebuilt whenever
// they change: a new untracked file in the project and an edit to a sibling
// package each rebuild, a Go edit does not. The component's own node_modules
// is never touched, and no untracked dotenv file reaches the build.
func TestLocalFrontendRebuildsWhenWhatItReadsChanges(t *testing.T) {
	root, module := wikiRepository(t)
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	require.NoError(t, os.MkdirAll(filepath.Join(remote, "node_modules", "owner"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "node_modules", "owner", "marker"), []byte("the component's own"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(remote, ".env"), []byte("SECRET=1"), 0o600))
	calls := filepath.Join(t.TempDir(), "calls")
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "npm"), []byte(`#!/bin/sh
set -eu
echo "$*" >> `+calls+`
[ ! -f .env ] || { echo 'an untracked .env reached the build' >&2; exit 4; }
case "$*" in
ci) rm -rf node_modules && mkdir -p node_modules/installed ;;
"run build") mkdir -p dist && printf '{"page":"%s","shared":"%s","widget":"%s"}' "$(cat Page.tsx)" "$(cat ../../../../packages/shared/index.ts)" "$(cat NewWidget.tsx 2>/dev/null || true)" > dist/mf-manifest.json ;;
esac
`), 0o700))
	var bundles []string
	build := LocalFrontend{
		StateDir: t.TempDir(),
		Retain: func(_ context.Context, root string) (string, error) {
			data, err := os.ReadFile(filepath.Join(root, FrontendBundleManifest))
			bundles = append(bundles, string(data))
			return "sha256:" + string(rune('a'+len(bundles))), err
		},
		Retained: func(string) bool { return true },
	}
	invocations := func() string {
		data, _ := os.ReadFile(calls)
		_ = os.Remove(calls)
		return strings.Join(strings.Fields(strings.ReplaceAll(string(data), "run build", "build")), " ")
	}
	bundle := func() string {
		t.Helper()
		_, err := build.Bundle(t.Context(), module)
		require.NoError(t, err)
		return bundles[len(bundles)-1]
	}

	require.Contains(t, bundle(), `"shared":"shared v1"`)
	require.Equal(t, "ci build", invocations())
	bundle()
	require.Empty(t, invocations(), "unchanged sources reuse the bundle")
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "api", "main.go"), []byte("package main // edited"), 0o600))
	bundle()
	require.Empty(t, invocations(), "a Go edit builds nothing")

	require.NoError(t, os.WriteFile(filepath.Join(remote, "NewWidget.tsx"), []byte("widget v1"), 0o600))
	require.Contains(t, bundle(), `"widget":"widget v1"`)
	require.Equal(t, "build", invocations(), "a new untracked file rebuilds")
	require.NoError(t, os.WriteFile(filepath.Join(remote, "NewWidget.tsx"), []byte("widget v2"), 0o600))
	require.Contains(t, bundle(), `"widget":"widget v2"`)
	require.Equal(t, "build", invocations(), "an edit to an untracked file rebuilds")
	require.NoError(t, os.WriteFile(filepath.Join(root, "packages/shared/index.ts"), []byte("shared v2"), 0o600))
	require.Contains(t, bundle(), `"shared":"shared v2"`)
	require.Equal(t, "ci build", invocations(), "a sibling package edit reinstalls and rebuilds")
	require.NoError(t, os.Remove(filepath.Join(remote, "NewWidget.tsx")))
	require.Contains(t, bundle(), `"widget":""`, "a deleted source leaves the staged copy too")
	require.Equal(t, "build", invocations())

	require.FileExists(t, filepath.Join(remote, "node_modules", "owner", "marker"), "the component's node_modules is never touched")
	require.NoDirExists(t, filepath.Join(remote, "node_modules", "installed"))

	require.NoError(t, os.Symlink("Page.tsx", filepath.Join(remote, "Alias.tsx")))
	_, err := build.Bundle(t.Context(), module)
	require.ErrorContains(t, err, "Alias.tsx is not a regular file")
}

// A recorded bundle is reused only while it is retained and its inputs hold.
func TestLocalFrontendReusesOnlyARetainedBundleOfTheSameInputs(t *testing.T) {
	module := frontendModule(t, map[string]string{
		"services/fe-remote/package.json":      "{}",
		"services/fe-remote/package-lock.json": "{}",
		"services/fe-remote/Page.tsx":          "v1",
	})
	retained := map[string]bool{}
	builds := 0
	build := LocalFrontend{
		StateDir: t.TempDir(),
		Retain: func(_ context.Context, root string) (string, error) {
			builds++
			digest := "sha256:" + filepath.Base(root) + string(rune('a'+builds))
			retained[digest] = true
			return digest, nil
		},
		Retained: func(digest string) bool { return retained[digest] },
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nmkdir -p node_modules dist\necho '{}' > dist/mf-manifest.json\n"), 0o700))
	first, err := build.Bundle(t.Context(), module)
	require.NoError(t, err)
	again, err := build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.Equal(t, 1, builds)
	delete(retained, first)
	_, err = build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, 2, builds, "a bundle no longer retained is built again")
	require.NoDirExists(t, filepath.Join(module.ServicesDir(), FrontendBundleDir), "the component is never written")
}

// This qualification exercises BuildKit's real content cache through the
// containerized local build. Random output proves the unchanged build script
// was not rerun; a source edit must invalidate that same layer.
func TestLocalFrontendBuildReusesContentCache(t *testing.T) {
	if os.Getenv("CODEFLY_FRONTEND_BUILD_QUALIFY") != "1" {
		t.Skip("set CODEFLY_FRONTEND_BUILD_QUALIFY=1 to qualify Docker content caching")
	}
	module := frontendModule(t, map[string]string{
		"services/fe-remote/package.json":      `{"name":"remote-cache-test","version":"1.0.0","private":true,"scripts":{"build":"node build.cjs"}}`,
		"services/fe-remote/package-lock.json": `{"name":"remote-cache-test","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"remote-cache-test","version":"1.0.0"}}}`,
		"services/fe-remote/build.cjs":         `const fs=require('fs');fs.mkdirSync('dist');fs.writeFileSync('dist/mf-manifest.json',JSON.stringify({input:fs.readFileSync('input','utf8'),build:require('crypto').randomUUID()}));`,
		"services/fe-remote/input":             t.Name() + t.TempDir(),
	})
	build := func() string {
		sources, err := selectFrontendSources(t.Context(), module)
		require.NoError(t, err)
		stage := func(ctx context.Context, _ *resources.Module, scratch string) (string, string, error) {
			destination := filepath.Join(scratch, "source")
			return destination, sources.component, sources.sync(ctx, destination)
		}
		destination := filepath.Join(t.TempDir(), "bundle")
		require.NoError(t, buildFrontendBundle(t.Context(), module, destination, stage))
		data, err := os.ReadFile(filepath.Join(destination, FrontendBundleManifest))
		require.NoError(t, err)
		return string(data)
	}
	first := build()
	require.Equal(t, first, build(), "unchanged content reuses the built layer, including its random marker")
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "fe-remote/input"), []byte("changed local source"), 0o600))
	changed := build()
	require.NotEqual(t, first, changed)
	require.Contains(t, changed, "changed local source")
	require.NoDirExists(t, filepath.Join(module.ServicesDir(), FrontendBundleDir))
}

func gitCommitAll(t *testing.T, root string) {
	t.Helper()
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-q", "-m", "more")
}

func gitRun(t *testing.T, root string, args ...string) {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
	require.NoError(t, err, string(output))
}

// packTarball packs a package holding index.js exporting content into
// vendorDir with npm, and answers its file name.
func packTarball(t *testing.T, vendorDir, version, content string) string {
	t.Helper()
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"name":"shared","version":"`+version+`","main":"index.js"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "index.js"), []byte("module.exports = "+strconvQuote(content)+";\n"), 0o600))
	require.NoError(t, os.MkdirAll(vendorDir, 0o755))
	command := exec.Command("npm", "pack", "--silent", "--pack-destination", vendorDir, "--cache", t.TempDir())
	command.Dir = source
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return strings.TrimSpace(string(output))
}

func strconvQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// npm can copy file: dependencies rather than link them. A staged source edit
// must reach the installed copy even when its package version and lock stay put.
func directoryDependencyFrontend(t *testing.T, locked bool) *resources.Module {
	t.Helper()
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed: local directory installs are not qualified here")
	}
	module := frontendModule(t, map[string]string{
		"services/schemas/package.json":   `{"name":"schemas","version":"1.0.0","files":["first.json","second.json"]}`,
		"services/schemas/first.json":     `"first"`,
		"services/fe-remote/.npmrc":       "install-links=true\noffline=true\naudit=false\nfund=false\n",
		"services/fe-remote/package.json": `{"name":"remote","version":"1.0.0","scripts":{"build":"node build.cjs"},"dependencies":{"schemas":"file:../schemas"}}`,
		"services/fe-remote/build.cjs":    `const fs=require('fs');fs.mkdirSync('dist',{recursive:true});fs.writeFileSync('dist/mf-manifest.json',JSON.stringify(fs.readdirSync('node_modules/schemas').filter(n=>n.endsWith('.json')&&n!=='package.json').sort().map(n=>JSON.parse(fs.readFileSync('node_modules/schemas/'+n)))));`,
	})
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	if !locked {
		require.NoError(t, os.WriteFile(filepath.Join(remote, ".npmrc"), []byte("install-links=true\npackage-lock=false\noffline=true\naudit=false\nfund=false\n"), 0o600))
		return module
	}
	command := exec.Command("npm", "install", "--package-lock-only", "--cache", t.TempDir())
	command.Dir = remote
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	return module
}

func TestLocalFrontendReinstallsDirectoryDependencies(t *testing.T) {
	for _, locked := range []bool{true, false} {
		t.Run(fmt.Sprintf("locked=%t", locked), func(t *testing.T) {
			testLocalFrontendReinstallsDirectoryDependencies(t, locked)
		})
	}
}

func testLocalFrontendReinstallsDirectoryDependencies(t *testing.T, locked bool) {
	t.Helper()
	module := directoryDependencyFrontend(t, locked)
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	lock, _ := os.ReadFile(filepath.Join(remote, "package-lock.json"))
	t.Setenv("npm_config_cache", t.TempDir())
	var bundles []string
	build := LocalFrontend{
		StateDir: t.TempDir(),
		Retain: func(_ context.Context, root string) (string, error) {
			data, err := os.ReadFile(filepath.Join(root, FrontendBundleManifest))
			bundles = append(bundles, string(data))
			return "sha256:" + string(rune('a'+len(bundles))), err
		},
		Retained: func(string) bool { return true },
	}
	bundle := func(want string) {
		t.Helper()
		_, err := build.Bundle(t.Context(), module)
		require.NoError(t, err)
		require.Equal(t, want, bundles[len(bundles)-1])
	}
	bundle(`["first"]`)
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "schemas/second.json"), []byte(`"second"`), 0o600))
	bundle(`["first","second"]`)
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "schemas/first.json"), []byte(`"changed"`), 0o600))
	bundle(`["changed","second"]`)
	require.NoError(t, os.Remove(filepath.Join(module.ServicesDir(), "schemas/second.json")))
	bundle(`["changed"]`)
	require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "schemas/package.json"), []byte(`{"name":"schemas","version":"1.0.0","files":["second.json"]}`), 0o600))
	bundle(`[]`)
	// A record made by an older install-identity algorithm cannot keep an
	// otherwise matching bundle: its node_modules may contain a stale copy.
	recordPath := filepath.Join(build.StateDir, module.Name+".json")
	data, err := os.ReadFile(recordPath)
	require.NoError(t, err)
	var record localFrontendRecord
	require.NoError(t, json.Unmarshal(data, &record))
	record.Installed = "obsolete-install-identity"
	data, err = json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(recordPath, data, 0o600))
	count := len(bundles)
	bundle(`[]`)
	require.Len(t, bundles, count+1)
	unchangedLock, err := os.ReadFile(filepath.Join(remote, "package-lock.json"))
	if locked {
		require.NoError(t, err)
		require.Equal(t, lock, unchangedLock)
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoDirExists(t, filepath.Join(remote, "node_modules"), "the owner's tree is never installed into")
}

// A successful bundle records A, but a failed B build may already have installed
// B. Reverting to A and editing only source must not build against that B copy.
func TestLocalFrontendRecoversInstalledDependenciesAfterFailure(t *testing.T) {
	for _, failure := range []string{"install", "build", "retain", "changed inputs"} {
		t.Run(failure, func(t *testing.T) {
			module := directoryDependencyFrontend(t, true)
			t.Setenv("npm_config_cache", t.TempDir())
			t.Setenv("npm_config_ignore_scripts", "false")
			remote := filepath.Join(module.ServicesDir(), "fe-remote")
			dependency := filepath.Join(module.ServicesDir(), "schemas/first.json")
			buildPath := filepath.Join(remote, "build.cjs")
			buildScript, err := os.ReadFile(buildPath)
			require.NoError(t, err)
			// npm runs the project's preinstall after copying its dependencies.
			manifest, err := os.ReadFile(filepath.Join(remote, "package.json"))
			require.NoError(t, err)
			manifest = []byte(strings.Replace(string(manifest), `"build":"node build.cjs"`, `"build":"node build.cjs","preinstall":"node install.cjs"`, 1))
			require.NoError(t, os.WriteFile(filepath.Join(remote, "package.json"), manifest, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(remote, "install.cjs"), []byte(`if(require('fs').existsSync('fail-install'))process.exit(1);`), 0o600))
			failRetain := false
			var bundles []string
			build := LocalFrontend{
				StateDir: t.TempDir(), Retained: func(string) bool { return true },
				Retain: func(_ context.Context, root string) (string, error) {
					if failRetain {
						return "", fmt.Errorf("retention failed")
					}
					data, err := os.ReadFile(filepath.Join(root, FrontendBundleManifest))
					bundles = append(bundles, string(data))
					return "sha256:" + string(rune('a'+len(bundles))), err
				},
			}
			original, err := build.Bundle(t.Context(), module)
			require.NoError(t, err)
			require.Equal(t, `["first"]`, bundles[0])
			require.NoError(t, os.WriteFile(dependency, []byte(`"second"`), 0o600))
			switch failure {
			case "install":
				require.NoError(t, os.WriteFile(filepath.Join(remote, "fail-install"), nil, 0o600))
			case "build":
				require.NoError(t, os.WriteFile(buildPath, []byte("process.exit(1);"), 0o600))
			case "retain":
				failRetain = true
			case "changed inputs":
				target, err := json.Marshal(dependency)
				require.NoError(t, err)
				script := append(slices.Clone(buildScript), []byte("\nfs.writeFileSync("+string(target)+",'\"third\"');")...)
				require.NoError(t, os.WriteFile(buildPath, script, 0o600))
			}
			_, err = build.Bundle(t.Context(), module)
			require.Error(t, err)
			if failure == "changed inputs" {
				require.ErrorIs(t, err, ErrFrontendInputsChanged)
			}
			installed, err := os.ReadFile(filepath.Join(build.stagingDir(module.Name), "source/services/fe-remote/node_modules/schemas/first.json"))
			require.NoError(t, err)
			require.Equal(t, `"second"`, string(installed), "the failed attempt already changed the installed copy")
			failRetain = false
			if failure == "install" {
				require.NoError(t, os.Remove(filepath.Join(remote, "fail-install")))
			}
			require.NoError(t, os.WriteFile(dependency, []byte(`"first"`), 0o600))
			require.NoError(t, os.WriteFile(buildPath, buildScript, 0o600))
			// The old immutable bundle is still reusable even when the mutable
			// staged installation no longer matches it.
			digest, err := build.Bundle(t.Context(), module)
			require.NoError(t, err)
			require.Equal(t, original, digest)
			require.Len(t, bundles, 1)
			require.NoError(t, os.WriteFile(buildPath, append(slices.Clone(buildScript), []byte("\n// source-only edit\n")...), 0o600))
			_, err = build.Bundle(t.Context(), module)
			require.NoError(t, err)
			require.Equal(t, []string{`["first"]`, `["first"]`}, bundles)
		})
	}
}

func TestLocalFrontendReinstallsAfterCanceledInstall(t *testing.T) {
	root, module := wikiRepository(t)
	remote := filepath.Join(module.ServicesDir(), "fe-remote")
	started := filepath.Join(t.TempDir(), "started")
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// exec replaces the shell: canceling the command kills the sole sleeping
	// process, after it has left a partial installation behind.
	require.NoError(t, os.WriteFile(filepath.Join(bin, "npm"), []byte(`#!/bin/sh
set -eu
case "$*" in
ci) mkdir -p node_modules
    cp ../../../../packages/shared/index.ts node_modules/value
    if [ -f interrupt-install ]; then
        touch '`+started+`'
        exec sleep 60
    fi ;;
"run build") mkdir -p dist; cp node_modules/value dist/mf-manifest.json ;;
esac
`), 0o700))
	var bundle string
	build := LocalFrontend{
		StateDir: t.TempDir(), Retained: func(string) bool { return true },
		Retain: func(_ context.Context, root string) (string, error) {
			data, err := os.ReadFile(filepath.Join(root, FrontendBundleManifest))
			bundle = string(data)
			return "sha256:" + bundle, err
		},
	}
	_, err := build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, "shared v1", bundle)
	require.NoError(t, os.WriteFile(filepath.Join(root, "packages/shared/index.ts"), []byte("shared v2"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "interrupt-install"), nil, 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := build.Bundle(ctx, module); done <- err }()
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 5*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled install did not exit")
	}
	require.NoError(t, os.Remove(filepath.Join(remote, "interrupt-install")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "packages/shared/index.ts"), []byte("shared v1"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "NewWidget.tsx"), []byte("a new source"), 0o600))
	_, err = build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, "shared v1", bundle)
}

func TestFrontendInstallIdentityIncludesTransitivePackagesAndConfiguration(t *testing.T) {
	root, module := wikiRepository(t)
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
	write("packages/shared/package.json", `{"name":"shared","dependencies":{"nested":"file:../nested"}}`)
	write("packages/nested/package.json", `{"name":"nested"}`)
	write("packages/nested/index.js", "first")
	write("solutions/wiki/services/fe-remote/.npmrc", "install-links=true")
	digest := func() string {
		t.Helper()
		sources, err := selectFrontendSources(t.Context(), module)
		require.NoError(t, err)
		_, install, err := sources.digest(module, false)
		require.NoError(t, err)
		return install
	}
	for _, change := range []struct{ name, content string }{
		{"packages/nested/index.js", "second"},
		{"solutions/wiki/services/fe-remote/.npmrc", "install-links=false"},
		{"solutions/wiki/services/fe-remote/package.json", `{"dependencies":{"shared":"file:../../../../packages/shared"},"optionalDependencies":{}}`},
	} {
		before := digest()
		write(change.name, change.content)
		require.NotEqual(t, before, digest(), change.name)
	}
}

// A project depending on a vendored tarball beside another component — as
// every solution depending on the wiki's vendored UI does — reads it: the
// tarball is a source at the path npm resolves, so an empty npm cache installs
// it from the staged copy; replacing it changes the digest, and a vendor bump
// rebuilds with the new tarball.
func TestAVendoredTarballDependencyIsASource(t *testing.T) {
	root := gitRepository(t, map[string]string{
		"solutions/wiki/services/fe-remote/package.json":  `{"name":"wiki"}`,
		"solutions/other/services/fe-remote/package.json": `{"name":"remote","version":"1.0.0","private":true,"dependencies":{"shared":"file:../../../wiki/services/fe-remote/vendor/shared-1.0.0.tgz"}}`,
	})
	vendor := filepath.Join(root, "solutions/wiki/services/fe-remote/vendor")
	module := &resources.Module{Name: "other", ServiceEntry: "api"}
	module.WithDir(filepath.Join(root, "solutions/other"))
	remote := filepath.Join(module.ServicesDir(), "fe-remote")

	require.NoError(t, os.MkdirAll(vendor, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(vendor, "shared-1.0.0.tgz"), []byte("v1"), 0o600))
	sources, err := selectFrontendSources(t.Context(), module)
	require.NoError(t, err)
	require.Contains(t, sources.files, "solutions/wiki/services/fe-remote/vendor/shared-1.0.0.tgz")
	first, _, err := sources.digest(module, false)
	require.NoError(t, err)
	staged := t.TempDir()
	require.NoError(t, sources.sync(t.Context(), staged))
	require.FileExists(t, filepath.Join(staged, "solutions/wiki/services/fe-remote/vendor/shared-1.0.0.tgz"))
	require.NoError(t, os.WriteFile(filepath.Join(vendor, "shared-1.0.0.tgz"), []byte("v2"), 0o600))
	replaced, _, err := sources.digest(module, false)
	require.NoError(t, err)
	require.NotEqual(t, first, replaced, "replacing the tarball changes what the build reads")

	require.NoError(t, os.WriteFile(filepath.Join(remote, "package.json"), []byte(`{"name":"remote","dependencies":{"shared":"file:../missing.tgz"}}`), 0o600))
	_, err = selectFrontendSources(t.Context(), module)
	require.ErrorContains(t, err, "names nothing readable")
	require.NoError(t, os.WriteFile(filepath.Join(remote, "package.json"), []byte(`{"name":"remote","dependencies":{"shared":"file:../../../../../outside.tgz"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(root), "outside.tgz"), []byte("x"), 0o600))
	_, err = selectFrontendSources(t.Context(), module)
	require.ErrorContains(t, err, "is outside")

	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is not installed: the cold-cache install is not qualified here")
	}
	require.NoError(t, os.Remove(filepath.Join(vendor, "shared-1.0.0.tgz")))
	depend := func(tarball string) {
		require.NoError(t, os.WriteFile(filepath.Join(remote, "package.json"), []byte(`{"name":"remote","version":"1.0.0","private":true,"scripts":{"build":"node build.cjs"},"dependencies":{"shared":"file:../../../wiki/services/fe-remote/vendor/`+tarball+`"}}`), 0o600))
	}
	depend(packTarball(t, vendor, "1.0.0", "shared v1"))
	require.NoError(t, os.WriteFile(filepath.Join(remote, "build.cjs"), []byte(`const fs=require('fs');fs.mkdirSync('dist',{recursive:true});fs.writeFileSync('dist/mf-manifest.json',JSON.stringify({shared:require('shared')}));`), 0o600))
	relock := func() {
		command := exec.Command("npm", "install", "--package-lock-only", "--offline", "--no-audit", "--no-fund", "--cache", t.TempDir())
		command.Dir = remote
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
	}
	relock()
	t.Setenv("npm_config_cache", t.TempDir()) // cold: nothing but the staged copy holds the tarball
	t.Setenv("npm_config_offline", "true")
	var bundles []string
	build := LocalFrontend{
		StateDir: t.TempDir(),
		Retain: func(_ context.Context, root string) (string, error) {
			data, err := os.ReadFile(filepath.Join(root, FrontendBundleManifest))
			bundles = append(bundles, string(data))
			return "sha256:" + string(rune('a'+len(bundles))), err
		},
		Retained: func(string) bool { return true },
	}
	_, err = build.Bundle(t.Context(), module)
	require.NoError(t, err, "a cold-cache install reads the vendored tarball from the staged copy")
	require.Equal(t, `{"shared":"shared v1"}`, bundles[0])
	// A vendor bump, as one lands: a new tarball, the dependency and the
	// lockfile moved to it.
	require.NoError(t, os.Remove(filepath.Join(vendor, "shared-1.0.0.tgz")))
	depend(packTarball(t, vendor, "1.0.1", "shared v2"))
	relock()
	_, err = build.Bundle(t.Context(), module)
	require.NoError(t, err)
	require.Len(t, bundles, 2, "a vendored tarball bump rebuilds")
	require.Equal(t, `{"shared":"shared v2"}`, bundles[1])
}

// Staged copies are bounded: past the bound, the least recently built copies
// of other modules are removed, never the one just built; Forget and ForgetAll
// remove copies, their node_modules included, and keep the bundle records.
func TestStagedFrontendCopiesAreBoundedAndForgotten(t *testing.T) {
	state := t.TempDir()
	stage := func(module string, size int, age time.Duration) {
		dir := filepath.Join(state, module, "source", "node_modules", "dep")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "blob"), make([]byte, size), 0o600))
		record := filepath.Join(state, module+".json")
		require.NoError(t, os.WriteFile(record, []byte(`{}`), 0o600))
		when := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(record, when, when))
	}
	stage("oldest", 400, 3*time.Hour)
	stage("older", 400, 2*time.Hour)
	stage("recent", 400, time.Hour)
	stage("built", 400, 0)
	build := LocalFrontend{StateDir: state, Bound: 1000}
	require.NoError(t, build.prune(t.Context(), "built"))
	for module, kept := range map[string]bool{"oldest": false, "older": false, "recent": true, "built": true} {
		_, err := os.Stat(filepath.Join(state, module))
		require.Equal(t, kept, err == nil, module)
		require.FileExists(t, filepath.Join(state, module+".json"), "the record of %s stays", module)
	}
	require.NoError(t, build.Forget(t.Context(), "recent"))
	require.NoDirExists(t, filepath.Join(state, "recent"))
	require.NoError(t, build.Forget(t.Context(), "never-built"))
	require.NoError(t, build.ForgetAll(t.Context()))
	require.NoDirExists(t, filepath.Join(state, "built"))
	require.FileExists(t, filepath.Join(state, "built.json"))
	require.NoError(t, LocalFrontend{StateDir: filepath.Join(state, "absent")}.ForgetAll(t.Context()))
}

// TestInstallCommandFallsBackWhereThereIsNoLock. `npm ci` cannot run without a
// lockfile — "loadVirtual requires existing shrinkwrap file" — and a freshly
// scaffolded page remote has none. The solution booted and its backend
// answered, and delivery still reported the frontend unbuildable, for a
// project whose only fault was that nobody had installed it yet.
func TestInstallCommandFallsBackWhereThereIsNoLock(t *testing.T) {
	project := t.TempDir()
	if got := installCommand(project); !slices.Equal(got, []string{"npm", "install"}) {
		t.Fatalf("a project with no lockfile must be installed with npm install, got %v", got)
	}
	for _, lock := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, lock), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := installCommand(dir); !slices.Equal(got, []string{"npm", "ci"}) {
			t.Fatalf("a project with %s must be installed with npm ci, got %v", lock, got)
		}
	}
}

// TestTheRegistryCredentialReachesTheBuild. A solution's page depends on the
// @codefly-dev kits from GitHub Packages, and the .npmrc the agent writes
// beside it reads ${NODE_AUTH_TOKEN}. The build environment is an allow-list,
// so dropping that name made the file impossible to satisfy: every install
// failed "401 Unauthorized ... unauthenticated" whatever the operator
// exported, and nothing said the CLI had removed it.
func TestTheRegistryCredentialReachesTheBuild(t *testing.T) {
	kept := nativeBuildEnvironment([]string{
		"PATH=/usr/bin",
		"NODE_AUTH_TOKEN=secret",
		"AWS_SECRET_ACCESS_KEY=must-not-leak",
	})
	if !slices.Contains(kept, "NODE_AUTH_TOKEN=secret") {
		t.Fatalf("the registry credential must reach npm, got %v", kept)
	}
	if slices.Contains(kept, "AWS_SECRET_ACCESS_KEY=must-not-leak") {
		t.Fatal("the allow-list must still drop everything it does not name")
	}
}
