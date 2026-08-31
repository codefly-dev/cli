package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	output "github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/integrity"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func TestModuleSyncPlanOrdersInvalidSourceBeforeOtherConflicts(t *testing.T) {
	var lines []string
	output.SetOutputSink(func(_ wool.Loglevel, msg string) { lines = append(lines, msg) })
	defer output.SetOutputSink(nil)

	printModuleSyncPlan("app", &integrity.BaseSyncPlan{
		SourceRoot:    "/src",
		SourceInvalid: []integrity.InvalidSource{{Path: "a.go", Reason: integrity.SourceUnsafePath}},
		TargetInvalid: []string{"b.go"},
		Modified:      []string{"c.go"},
	}, false, nil)

	joined := strings.Join(lines, "\n")
	source := strings.Index(joined, "INVALID SOURCE")
	target := strings.Index(joined, "INVALID TARGET")
	modified := strings.Index(joined, "MODIFIED BASE")
	if source < 0 || target < 0 || modified < 0 {
		t.Fatalf("plan is missing a conflict section:\n%s", joined)
	}
	if source > target || source > modified {
		t.Fatalf("INVALID SOURCE must precede other conflict sections:\n%s", joined)
	}
}

func TestSourceInvalidReportSurfacesEveryReasonWithDigests(t *testing.T) {
	report := sourceInvalidReport([]integrity.InvalidSource{
		{Path: "z.go", Reason: integrity.SourceInvalidReason("future-reason")},
		{Path: "a.go", Reason: integrity.SourceDigestMismatch, ManifestDigest: "1111222233", ActualDigest: "aaaabbbbcc"},
	})
	joined := strings.Join(report, "\n")

	if !strings.Contains(joined, "manifest 11112222...  actual aaaabbbb...") {
		t.Fatalf("digest mismatch did not print expected vs actual:\n%s", joined)
	}
	// An unrecognized reason still counts toward the blocker, so it must be
	// surfaced rather than silently dropped from the operator's plan.
	if !strings.Contains(joined, "future-reason") || !strings.Contains(joined, "z.go") {
		t.Fatalf("unrecognized reason was dropped:\n%s", joined)
	}
	if strings.Index(joined, "a.go") > strings.Index(joined, "z.go") {
		t.Fatalf("known reasons should be ordered before unknown ones:\n%s", joined)
	}
}

func TestModuleCommandIsPreviewFirstAndRejectsAmbiguousArguments(t *testing.T) {
	if ModuleCmd.RunE == nil || ModuleCmd.Run != nil {
		t.Fatal("sync module command is not exclusively RunE")
	}
	if err := ModuleCmd.Args(ModuleCmd, nil); err == nil {
		t.Fatal("sync module accepted no module name")
	}
	if err := ModuleCmd.Args(ModuleCmd, []string{"one", "two"}); err == nil {
		t.Fatal("sync module accepted multiple module names")
	}
	flag := ModuleCmd.Flags().Lookup("accept-upstream")
	if flag == nil || flag.Value.Type() != "stringArray" {
		t.Fatal("sync module does not expose repeatable --accept-upstream reconciliation")
	}
	if ModuleCmd.Flags().Lookup("restore-code") == nil {
		t.Fatal("sync module does not expose --restore-code")
	}
	if err := restoreComposedModuleCode(context.Background(), nil, &moduleSyncOptions{RestoreCode: true, Apply: true}); err == nil {
		t.Fatal("--restore-code accepted update flags")
	}
}

func TestRestoreCodeUsesPinnedSourceAndPreservesOverlay(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	codePath := "services/api/code/main.go"
	code := "package main\n"
	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, codePath), code)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.3")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	overlayPath := filepath.Join(targetRoot, "services", "api", "overlays", "local.yaml")
	writeSyncTestFile(t, overlayPath, "consumer: true\n")
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	if err := writeModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath), &moduleSourceLock{
		Schema: moduleSourceLockSchema, Repository: remote, Ref: "v1.2.3",
		Commit: runGit(t, repository, "rev-parse", "HEAD"), Subdirectory: "module",
	}); err != nil {
		t.Fatal(err)
	}
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{RestoreCode: true}); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(codePath)), code)
	assertSyncTestFile(t, overlayPath, "consumer: true\n")
}

func TestRestoreCodeBootstrapsLegacyModuleFromExplicitImmutableSource(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	codePath := "services/api/code/main.go"
	code := "package main\n"
	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, codePath), code)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.3")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	overlayPath := filepath.Join(targetRoot, "services", "api", "overlays", "local.yaml")
	writeSyncTestFile(t, overlayPath, "consumer: true\n")
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{
		RestoreCode: true, Source: remote, To: "v1.2.3", Subdirectory: "module",
	}); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(codePath)), code)
	assertSyncTestFile(t, overlayPath, "consumer: true\n")
	lock, err := readModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Repository != remote || lock.Ref != "v1.2.3" || lock.Subdirectory != "module" {
		t.Fatalf("lock = %#v", lock)
	}
}

func TestPinModuleSourceWritesImmutableLock(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	writeSyncTestFile(t, filepath.Join(repository, "module", "module.codefly.yaml"), "kind: module\nname: app\nservices: []\n")
	writeSyncTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"), `{"files":{}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.3")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices: []\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"), `{"files":{}}`)
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	prepared, err := prepareModuleSource(context.Background(), target.Dir(), &moduleSyncOptions{Source: remote, To: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.Pin(target); err != nil {
		t.Fatal(err)
	}
	lock, err := readModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Repository != remote || lock.Ref != "v1.2.3" || lock.Commit != runGit(t, repository, "rev-parse", "HEAD") || lock.Subdirectory != "module" {
		t.Fatalf("lock = %#v", lock)
	}
}

func TestPreparedModuleSourcePinsInventoryOnlyScaffold(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	codePath := "services/api/code/main.go"
	writeSyncTestFile(t, filepath.Join(repository, "module", codePath), "package canonical\n")
	writeSyncTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest("package canonical\n")+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.3")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	prepared, err := prepareModuleSource(context.Background(), target.Dir(), &moduleSyncOptions{Source: remote, To: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.Pin(target); err != nil {
		t.Fatal(err)
	}
	lock, err := readModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Repository != remote || lock.Ref != "v1.2.3" {
		t.Fatalf("lock = %#v", lock)
	}
}

// A manifest-less scaffold that already carries base-owned code diverging from
// the pinned source is a misbehaving agent's inconsistent output: pin must
// reject it at add time rather than let a broken module register and surface a
// misleading "manifest was lost" error on the first sync. No source lock is
// written when pin refuses.
func TestPreparedModuleSourceRejectsInventoryOnlyScaffoldWithDivergentBaseCode(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	codePath := "services/api/code/main.go"
	writeSyncTestFile(t, filepath.Join(repository, "module", codePath), "package canonical\n")
	writeSyncTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest("package canonical\n")+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.3")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	// The agent left a divergent base file but no base manifest.
	writeSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(codePath)), "package customized\n")
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	prepared, err := prepareModuleSource(context.Background(), target.Dir(), &moduleSyncOptions{Source: remote, To: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.Pin(target); err == nil {
		t.Fatal("pin accepted an inconsistent manifest-less scaffold carrying divergent base code")
	}
	if _, err := os.Stat(filepath.Join(targetRoot, moduleSourceLockRelativePath)); !os.IsNotExist(err) {
		t.Fatalf("source lock was written despite pin failure: %v", err)
	}
}

func TestPreparedModuleSourceRejectsScaffoldFromDifferentBytes(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	codePath := "services/api/code/main.go"
	writeSyncTestFile(t, filepath.Join(repository, "module", "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(repository, "module", codePath), "package canonical\n")
	writeSyncTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest("package canonical\n")+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.3")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, codePath), "package local\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest("package local\n")+`"}}`)
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	prepared, err := prepareModuleSource(context.Background(), target.Dir(), &moduleSyncOptions{Source: remote, To: "v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if err := prepared.Pin(target); err == nil {
		t.Fatal("canonical source was pinned over a scaffold produced from different bytes")
	}
	if _, err := os.Stat(filepath.Join(targetRoot, moduleSourceLockRelativePath)); !os.IsNotExist(err) {
		t.Fatalf("source lock was written after provenance mismatch: %v", err)
	}
}

// A base sync must also refresh the consumer's generated per-service
// service.codefly.yaml so its agent pin tracks the synced module version instead
// of drifting stale (#479). The manifest is a consumer overlay, so the base
// manifest never lists it.
func TestSyncModuleRefreshesGeneratedServiceManifestAgentPin(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	codePath := "services/vault/code/main.go"
	code := "package main\n"
	manifestPath := "services/vault/" + resources.ServiceConfigurationName
	generatedHeader := "# Code generated from deployment/topology.bindings.codefly.yaml. DO NOT EDIT.\n"
	upstreamManifest := generatedHeader + "name: vault\nagent:\n  kind: codefly:service\n  name: vault\n  publisher: codefly.dev\n  version: 0.0.25\n"
	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: vault\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, codePath), code)
	writeSyncTestFile(t, filepath.Join(sourceModule, filepath.FromSlash(manifestPath)), upstreamManifest)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v0.0.42")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: vault\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"), `{"files":{}}`)
	writeSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(manifestPath)),
		generatedHeader+"name: vault\nagent:\n  kind: codefly:service\n  name: vault\n  publisher: codefly.dev\n  version: 0.0.15\n")
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	if err := writeModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath), &moduleSourceLock{
		Schema: moduleSourceLockSchema, Repository: remote, Ref: "v0.0.42",
		Commit: runGit(t, repository, "rev-parse", "HEAD"), Subdirectory: "module",
	}); err != nil {
		t.Fatal(err)
	}
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(manifestPath)), upstreamManifest)
}

func TestLockfileStaleDetectsDependencyDrift(t *testing.T) {
	inSync := `{"lockfileVersion":3,"packages":{"":{"name":"frontend","dependencies":{"dep-a":"1.0.0"}}}}`
	cases := []struct {
		name      string
		pkg       string
		lock      string
		wantStale bool
	}{
		{"in-sync", `{"dependencies":{"dep-a":"1.0.0"}}`, inSync, false},
		{"missing dependency", `{"dependencies":{"dep-a":"1.0.0","dep-b":"2.0.0"}}`, inSync, true},
		{"changed specifier", `{"dependencies":{"dep-a":"2.0.0"}}`, inSync, true},
		{"dev dependency drift", `{"dependencies":{"dep-a":"1.0.0"},"devDependencies":{"dep-c":"1.0.0"}}`, inSync, true},
		// A version-only bump is not the drift `npm ci` rejects, so it must not
		// trigger a needless regeneration.
		{"version bump only", `{"version":"9.9.9","dependencies":{"dep-a":"1.0.0"}}`, inSync, false},
		// lockfileVersion 1 has no packages map: npm cannot describe it, so it is
		// regenerated rather than trusted.
		{"legacy lockfile", `{"dependencies":{"dep-a":"1.0.0"}}`, `{"lockfileVersion":1,"dependencies":{"dep-a":{"version":"1.0.0"}}}`, true},
		{"unparseable lockfile", `{"dependencies":{"dep-a":"1.0.0"}}`, `not json`, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			packageJSONPath := filepath.Join(dir, "package.json")
			lockPath := filepath.Join(dir, "package-lock.json")
			writeSyncTestFile(t, packageJSONPath, testCase.pkg)
			writeSyncTestFile(t, lockPath, testCase.lock)
			stale, err := lockfileStale(packageJSONPath, lockPath)
			if err != nil {
				t.Fatal(err)
			}
			if stale != testCase.wantStale {
				t.Fatalf("lockfileStale = %v, want %v", stale, testCase.wantStale)
			}
		})
	}
}

// The base sync commits its manifest last, so a package.json a prior interrupted
// run already wrote reappears as Unchanged on the retry. Selection must key off
// on-disk drift, not the plan bucket, or a lockfile a failed regeneration left
// behind would never be healed. An in-sync lockfile is skipped (no needless npm
// run, so npm is required only for genuine drift), a directory with no lockfile
// is never given one, and an npm-shrinkwrap.json is honored like a lock.
func TestStaleLockfilesSelectsDriftedIncludingUnchangedAndShrinkwrap(t *testing.T) {
	sourceRoot := t.TempDir()
	moduleDir := t.TempDir()
	withDep := `{"dependencies":{"dep-a":"1.0.0"}}`
	inSyncLock := `{"lockfileVersion":3,"packages":{"":{"dependencies":{"dep-a":"1.0.0"}}}}`
	emptyLock := `{"lockfileVersion":3,"packages":{"":{}}}`

	// Unchanged (a prior run already wrote it) with a lockfile that never got the
	// new dependency: must be selected on the rerun.
	writeSyncTestFile(t, filepath.Join(sourceRoot, "services", "frontend", "code", "package.json"), withDep)
	writeSyncTestFile(t, filepath.Join(moduleDir, "services", "frontend", "code", "package-lock.json"), emptyLock)
	// Unchanged but the lockfile is already in sync: must be skipped.
	writeSyncTestFile(t, filepath.Join(sourceRoot, "services", "api", "code", "package.json"), withDep)
	writeSyncTestFile(t, filepath.Join(moduleDir, "services", "api", "code", "package-lock.json"), inSyncLock)
	// Updated but the service has no lockfile: never given one.
	writeSyncTestFile(t, filepath.Join(sourceRoot, "services", "vault", "code", "package.json"), withDep)
	// A drifted npm-shrinkwrap.json is honored like a package-lock.json.
	writeSyncTestFile(t, filepath.Join(sourceRoot, "services", "web", "code", "package.json"), withDep)
	writeSyncTestFile(t, filepath.Join(moduleDir, "services", "web", "code", "npm-shrinkwrap.json"), emptyLock)

	stale, err := staleLockfiles(sourceRoot, moduleDir, &integrity.BaseSyncPlan{
		Unchanged: []string{"services/frontend/code/package.json", "services/api/code/package.json", "services/web/code/package.json"},
		Update:    []string{"services/vault/code/package.json"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lockLabels(stale), ",")
	want := "services/frontend/code/package-lock.json,services/web/code/npm-shrinkwrap.json"
	if got != want {
		t.Fatalf("staleLockfiles = %q, want %q", got, want)
	}
}

func TestRegenerateNpmLockfilesRequiresNpmForGenuineDrift(t *testing.T) {
	sourceRoot := t.TempDir()
	moduleDir := t.TempDir()
	writeSyncTestFile(t, filepath.Join(sourceRoot, "services", "frontend", "code", "package.json"), `{"dependencies":{"dep-a":"1.0.0"}}`)
	writeSyncTestFile(t, filepath.Join(moduleDir, "services", "frontend", "code", "package-lock.json"), `{"lockfileVersion":3,"packages":{"":{}}}`)
	// An empty PATH makes npm undiscoverable; a genuinely drifted lockfile must
	// then fail loudly rather than silently leaving the workspace uninstallable.
	t.Setenv("PATH", t.TempDir())
	err := regenerateNpmLockfiles(context.Background(), sourceRoot, moduleDir, &integrity.BaseSyncPlan{
		Update: []string{"services/frontend/code/package.json"},
	})
	if err == nil {
		t.Fatal("expected an error when npm is absent and a lockfile is drifted")
	}
	if !strings.Contains(err.Error(), "npm is not installed") || !strings.Contains(err.Error(), "services/frontend/code/package-lock.json") {
		t.Fatalf("error does not name the missing tool and the lockfile: %v", err)
	}
}

func TestSyncModuleRefreshesManifestsEvenWhenLockRegenFails(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	packagePath := "services/frontend/code/package.json"
	manifestPath := "services/frontend/" + resources.ServiceConfigurationName
	generatedHeader := "# Code generated from deployment/topology.bindings.codefly.yaml. DO NOT EDIT.\n"
	upstreamManifest := generatedHeader + "name: frontend\nagent:\n  kind: codefly:service\n  name: frontend\n  publisher: codefly.dev\n  version: 0.0.25\n"
	newPackage := "{\n  \"dependencies\": {\n    \"dep-a\": \"1.0.0\"\n  }\n}\n"
	oldPackage := "{}\n"
	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: frontend\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, filepath.FromSlash(packagePath)), newPackage)
	writeSyncTestFile(t, filepath.Join(sourceModule, filepath.FromSlash(manifestPath)), upstreamManifest)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+packagePath+`":"`+syncTestDigest(newPackage)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v0.0.44")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: frontend\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"),
		`{"files":{"`+packagePath+`":"`+syncTestDigest(oldPackage)+`"}}`)
	writeSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(packagePath)), oldPackage)
	writeSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(manifestPath)),
		generatedHeader+"name: frontend\nagent:\n  kind: codefly:service\n  name: frontend\n  publisher: codefly.dev\n  version: 0.0.15\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "services", "frontend", "code", "package-lock.json"),
		`{"lockfileVersion":3,"packages":{"":{}}}`)
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	if err := writeModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath), &moduleSourceLock{
		Schema: moduleSourceLockSchema, Repository: remote, Ref: "v0.0.44",
		Commit: runGit(t, repository, "rev-parse", "HEAD"), Subdirectory: "module",
	}); err != nil {
		t.Fatal(err)
	}
	// Shadow npm with a stub that always fails, keeping git on PATH so the source
	// still resolves. The deterministic manifest refresh runs before the fragile
	// regeneration, so an npm failure must not rob it.
	stubBin := t.TempDir()
	writeExecutable(t, filepath.Join(stubBin, "npm"), "#!/bin/sh\necho 'stub npm failure' 1>&2\nexit 1\n")
	t.Setenv("PATH", stubBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	err = syncComposedModule(context.Background(), target, &moduleSyncOptions{Apply: true})
	if err == nil {
		t.Fatal("expected the lockfile regeneration failure to surface")
	}
	if !strings.Contains(err.Error(), "regenerate") {
		t.Fatalf("error is not about lockfile regeneration: %v", err)
	}
	assertSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(manifestPath)), upstreamManifest)
}

func TestSyncModuleRegeneratesStalePackageLock(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm is required to regenerate the lockfile")
	}
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	packagePath := "services/frontend/code/package.json"
	localDepPath := "services/frontend/code/local-dep/package.json"
	// A file: dependency resolves from disk, so the regeneration stays offline.
	newPackage := "{\n  \"name\": \"frontend\",\n  \"version\": \"1.0.0\",\n  \"dependencies\": {\n    \"local-dep\": \"file:./local-dep\"\n  }\n}\n"
	oldPackage := "{\n  \"name\": \"frontend\",\n  \"version\": \"1.0.0\"\n}\n"
	localDep := "{\n  \"name\": \"local-dep\",\n  \"version\": \"1.0.0\"\n}\n"
	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: frontend\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, filepath.FromSlash(packagePath)), newPackage)
	writeSyncTestFile(t, filepath.Join(sourceModule, filepath.FromSlash(localDepPath)), localDep)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+packagePath+`":"`+syncTestDigest(newPackage)+`","`+localDepPath+`":"`+syncTestDigest(localDep)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v0.0.44")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: frontend\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-manifest.json"),
		`{"files":{"`+packagePath+`":"`+syncTestDigest(oldPackage)+`"}}`)
	writeSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(packagePath)), oldPackage)
	stalePackageLock := "{\n  \"name\": \"frontend\",\n  \"version\": \"1.0.0\",\n  \"lockfileVersion\": 3,\n  \"packages\": {\n    \"\": {\n      \"name\": \"frontend\",\n      \"version\": \"1.0.0\"\n    }\n  }\n}\n"
	lockPath := filepath.Join(targetRoot, "services", "frontend", "code", "package-lock.json")
	writeSyncTestFile(t, lockPath, stalePackageLock)
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	if err := writeModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath), &moduleSourceLock{
		Schema: moduleSourceLockSchema, Repository: remote, Ref: "v0.0.44",
		Commit: runGit(t, repository, "rev-parse", "HEAD"), Subdirectory: "module",
	}); err != nil {
		t.Fatal(err)
	}
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}

	assertSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(packagePath)), newPackage)
	regenerated, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// The refreshed lockfile now records the dependency the old one was missing,
	// so `npm ci` no longer fails closed.
	if !strings.Contains(string(regenerated), "local-dep") {
		t.Fatalf("package-lock.json was not regenerated with the refreshed dependency:\n%s", regenerated)
	}
	stillStale, err := lockfileStale(filepath.Join(targetRoot, filepath.FromSlash(packagePath)), lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if stillStale {
		t.Fatal("lockfile is still reported stale after regeneration")
	}
}

func TestModuleAgentSourceMatchesAgentReleaseRepository(t *testing.T) {
	options, err := moduleAgentSource(&resources.Agent{
		Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "saas-starter", Version: "0.0.36",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Source != "https://github.com/codefly-dev/module-saas-starter.git" || options.To != "v0.0.36" {
		t.Fatalf("source options = %#v", options)
	}
}

func TestResolveModuleSourceRequiresGitTagAndPinsCommit(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	writeSyncTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"), `{"files":{}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "branch", "v1.2.3")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.2.4")
	remote := (&url.URL{Scheme: "file", Path: repository}).String()

	_, cleanup, err := resolveModuleSource(context.Background(), t.TempDir(), &moduleSyncOptions{
		Source: remote, To: "v1.2.3", Subdirectory: "module",
	})
	cleanup()
	if err == nil {
		t.Fatal("semantic-version branch was accepted as an immutable tag")
	}

	resolved, cleanup, err := resolveModuleSource(context.Background(), t.TempDir(), &moduleSyncOptions{
		Source: remote, To: "v1.2.4", Subdirectory: "module",
	})
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	wantCommit := runGit(t, repository, "rev-parse", "HEAD")
	if resolved.Lock == nil || resolved.Lock.Commit != wantCommit || resolved.Lock.Ref != "v1.2.4" {
		t.Fatalf("resolved lock = %#v, want commit %s", resolved.Lock, wantCommit)
	}
}

func TestModuleSourceLockRejectsMutableRef(t *testing.T) {
	path := filepath.Join(t.TempDir(), moduleSourceLockRelativePath)
	writeSyncTestFile(t, path, `{"schema":"codefly/base-source/v1","repository":"https://example.invalid/starter.git","ref":"main","commit":"0123456789abcdef0123456789abcdef01234567"}`)
	if _, err := readModuleSourceLock(path); err == nil {
		t.Fatal("source lock accepted a mutable branch ref")
	}
}

func TestModuleSourceLockRoundTripsAndLocalSourceIsAutoDetected(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "repository", "module")
	writeSyncTestFile(t, filepath.Join(module, "tools", "base-manifest.json"), `{"files":{}}`)
	located, err := locateModuleRoot(filepath.Join(root, "repository"), "")
	if err != nil {
		t.Fatal(err)
	}
	if located != module {
		t.Fatalf("located %s, want %s", located, module)
	}

	path := filepath.Join(root, moduleSourceLockRelativePath)
	want := moduleSourceLock{
		Schema: moduleSourceLockSchema, Repository: "https://example.invalid/starter.git",
		Ref: "v1.2.3", Commit: "0123456789abcdef0123456789abcdef01234567", Subdirectory: "module",
	}
	if err := writeModuleSourceLock(path, &want); err != nil {
		t.Fatal(err)
	}
	got, err := readModuleSourceLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("lock = %#v, want %#v", got, want)
	}
}

func newSyncTestWorkspace(t *testing.T) (*resources.Workspace, string) {
	t.Helper()
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "consumer", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	loaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return loaded, root
}

// localModuleSource writes a resolvable local module source. Local paths are
// preview-only, so --create must reject them before mutating the workspace.
func localModuleSource(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	writeSyncTestFile(t, filepath.Join(source, "tools", "base-manifest.json"), `{"files":{}}`)
	return source
}

func moduleSaasDir(t *testing.T, workspace *resources.Workspace) string {
	t.Helper()
	return workspace.ModulePath(context.Background(), &resources.ModuleReference{Name: "saas"})
}

// assertModuleAbsent fails if the module leaked into the in-memory workspace,
// the persisted workspace file, or the filesystem.
func assertModuleAbsent(t *testing.T, workspace *resources.Workspace, root string) {
	t.Helper()
	if workspace.ExistsModule("saas") {
		t.Fatal("module leaked into the in-memory workspace")
	}
	reloaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExistsModule("saas") {
		t.Fatal("module leaked into the persisted workspace file")
	}
	if _, err := os.Stat(moduleSaasDir(t, workspace)); !os.IsNotExist(err) {
		t.Fatalf("module directory was left on disk: %v", err)
	}
}

func TestSyncModuleWithoutCreateReturnsActionableError(t *testing.T) {
	loaded, _ := newSyncTestWorkspace(t)
	_, _, err := resolveSyncTarget(context.Background(), loaded, "saas", false, &moduleSyncOptions{})
	if err == nil {
		t.Fatal("syncing a missing module without --create returned success")
	}
	message := err.Error()
	if !strings.Contains(message, "codefly add module saas") || !strings.Contains(message, "--create") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func TestSyncModuleFirstPopulatesRegisteredModuleWithoutBaseManifest(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	codePath := "services/api/code/main.go"
	code := "package main\n"
	writeSyncTestFile(t, filepath.Join(sourceModule, codePath), code)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.0.0")

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices:\n  - name: api\n")
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{
		Source: remote, To: "v1.0.0", Subdirectory: "module", Apply: true,
	}); err != nil {
		t.Fatal(err)
	}
	assertSyncTestFile(t, filepath.Join(targetRoot, filepath.FromSlash(codePath)), code)
	if _, err := os.Stat(filepath.Join(targetRoot, moduleBaseManifestRelativePath)); err != nil {
		t.Fatalf("target manifest was not committed: %v", err)
	}
}

// A --create dry-run describes the initialization for a valid remote source and
// returns without registering or scaffolding anything.
func TestSyncModuleCreateDryRunDescribesWithoutRegistering(t *testing.T) {
	loaded, root := newSyncTestWorkspace(t)
	if err := runModuleSync(context.Background(), loaded, "saas", true, &moduleSyncOptions{
		Source: "https://example.invalid/starter.git", To: "v0.0.8",
	}); err != nil {
		t.Fatalf("dry-run --create returned an error: %v", err)
	}
	assertModuleAbsent(t, loaded, root)
}

// Finding #1: a --create --apply that omits --to must fail its precondition
// before any workspace mutation, never registering then rolling back.
func TestSyncModuleCreateValidatesSourceBeforeRegistering(t *testing.T) {
	loaded, root := newSyncTestWorkspace(t)
	err := runModuleSync(context.Background(), loaded, "saas", true, &moduleSyncOptions{
		Source: "https://example.invalid/starter.git", Apply: true,
	})
	if err == nil {
		t.Fatal("--create --apply without --to was accepted")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Fatalf("error does not point at --to: %v", err)
	}
	assertModuleAbsent(t, loaded, root)
}

// A local source can never initialize a module, and must be rejected before
// mutating the workspace.
func TestSyncModuleCreateRejectsLocalSourceBeforeRegistering(t *testing.T) {
	loaded, root := newSyncTestWorkspace(t)
	err := runModuleSync(context.Background(), loaded, "saas", true, &moduleSyncOptions{
		Source: localModuleSource(t), To: "v1.0.0", Apply: true,
	})
	if err == nil {
		t.Fatal("--create --apply with a local source was accepted")
	}
	if !strings.Contains(err.Error(), "preview-only") {
		t.Fatalf("error does not explain the local-source rejection: %v", err)
	}
	assertModuleAbsent(t, loaded, root)
}

// The one-command first run: --create --apply initializes the module, seeds an
// empty base manifest, and populates it from canonical.
func TestSyncModuleCreatePopulatesNewModuleOnApply(t *testing.T) {
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")
	code := "package main\n"
	codePath := "services/api/code/main.go"
	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: saas\nservices:\n  - name: api\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, codePath), code)
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+syncTestDigest(code)+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.0.0")
	remote := (&url.URL{Scheme: "file", Path: repository}).String()

	loaded, _ := newSyncTestWorkspace(t)
	if err := runModuleSync(context.Background(), loaded, "saas", true, &moduleSyncOptions{
		Source: remote, To: "v1.0.0", Subdirectory: "module", Apply: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !loaded.ExistsModule("saas") {
		t.Fatal("module was not registered")
	}
	assertSyncTestFile(t, filepath.Join(moduleSaasDir(t, loaded), filepath.FromSlash(codePath)), code)
	lock, err := readModuleSourceLock(filepath.Join(moduleSaasDir(t, loaded), moduleSourceLockRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Repository != remote || lock.Ref != "v1.0.0" {
		t.Fatalf("lock = %#v", lock)
	}
}

// An apply that passes preconditions, registers the module, but then fails
// (here: the remote tag cannot be cloned) must not leave the module stranded.
func TestSyncModuleCreateRollsBackWhenApplyFails(t *testing.T) {
	loaded, root := newSyncTestWorkspace(t)
	missingRemote := (&url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "missing-repo.git")}).String()
	err := runModuleSync(context.Background(), loaded, "saas", true, &moduleSyncOptions{
		Source: missingRemote, To: "v1.0.0", Apply: true,
	})
	if err == nil {
		t.Fatal("applying an unreachable remote unexpectedly succeeded")
	}
	assertModuleAbsent(t, loaded, root)
}

// Finding #4: rollback must clean a module directory that already holds
// half-applied upstream files, not just an empty scaffold.
func TestRollbackRemovesPopulatedModuleDir(t *testing.T) {
	loaded, root := newSyncTestWorkspace(t)
	module, err := registerModule(context.Background(), loaded, "saas")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate files written by a partially-completed ApplyBaseSync.
	writeSyncTestFile(t, filepath.Join(module.Dir(), "services", "api", "code", "main.go"), "package main\n")
	writeSyncTestFile(t, filepath.Join(module.Dir(), moduleSourceLockRelativePath),
		`{"schema":"codefly/base-source/v1","repository":"https://example.invalid/x.git","ref":"v1.0.0","commit":"0123456789abcdef0123456789abcdef01234567"}`)
	if err := rollbackRegisteredModule(context.Background(), loaded, "saas"); err != nil {
		t.Fatal(err)
	}
	assertModuleAbsent(t, loaded, root)
}

// maskedUpstreamFixture builds a remote whose allow-listed overlay.txt changed
// between v1.0.0 and v2.0.0, and a target module pinned at v1.0.0 that keeps its
// own copy. A --to v2.0.0 sync therefore masks a real upstream change: it blocks
// by default, and even --keep-local-divergences keeps the local file so the tree
// never reaches v2.0.0. Returns the remote URL and the loaded target module.
func maskedUpstreamFixture(t *testing.T) (string, *resources.Module) {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init", "--quiet")
	runGit(t, repository, "config", "user.email", "module-sync@example.invalid")
	runGit(t, repository, "config", "user.name", "Module Sync Test")
	sourceModule := filepath.Join(repository, "module")

	writeSyncTestFile(t, filepath.Join(sourceModule, "module.codefly.yaml"), "kind: module\nname: app\nservices: []\n")
	writeSyncTestFile(t, filepath.Join(sourceModule, "overlay.txt"), "upstream v1")
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"overlay.txt":"`+syncTestDigest("upstream v1")+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "v1")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.0.0")
	v1Commit := runGit(t, repository, "rev-parse", "v1.0.0^{commit}")

	writeSyncTestFile(t, filepath.Join(sourceModule, "overlay.txt"), "upstream v2")
	writeSyncTestFile(t, filepath.Join(sourceModule, "tools", "base-manifest.json"),
		`{"files":{"overlay.txt":"`+syncTestDigest("upstream v2")+`"}}`)
	runGit(t, repository, "add", ".")
	runGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "v2")
	runGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v2.0.0")

	remote := (&url.URL{Scheme: "file", Path: repository}).String()

	targetRoot := filepath.Join(t.TempDir(), "app")
	writeSyncTestFile(t, filepath.Join(targetRoot, "module.codefly.yaml"), "kind: module\nname: app\nservices: []\n")
	writeSyncTestFile(t, filepath.Join(targetRoot, "overlay.txt"), "local divergence")
	writeSyncTestFile(t, filepath.Join(targetRoot, moduleBaseManifestRelativePath),
		`{"files":{"overlay.txt":"`+syncTestDigest("upstream v1")+`"}}`)
	writeSyncTestFile(t, filepath.Join(targetRoot, "tools", "base-integrity-allow.json"),
		`{"overlay.txt":"product overlay","requiredAdditions":{}}`)
	writeSyncTestFile(t, filepath.Join(targetRoot, moduleSourceLockRelativePath),
		`{"schema":"`+moduleSourceLockSchema+`","repository":"`+remote+`","ref":"v1.0.0","commit":"`+v1Commit+`"}`)

	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	return remote, target
}

// A blocked apply must not silently leave the base pinned at the old ref: it
// prints an aggregate "base-source NOT advanced" line and leaves the lock intact.
func TestSyncModuleBlockedApplyReportsBaseNotAdvanced(t *testing.T) {
	remote, target := maskedUpstreamFixture(t)

	output.StartCapture()
	defer output.DrainCapture()
	err := syncComposedModule(context.Background(), target, &moduleSyncOptions{
		Source: remote, To: "v2.0.0", Subdirectory: "module", Apply: true,
	})
	captured := output.DrainCapture()
	if err == nil {
		t.Fatal("a masked upstream change must withhold the apply")
	}

	var advanced string
	for _, line := range captured {
		if strings.Contains(line.Message, "base-source NOT advanced") {
			advanced = line.Message
		}
	}
	if advanced == "" {
		t.Fatalf("no base-source NOT advanced line in output: %#v", captured)
	}
	for _, want := range []string{"still v1.0.0", "target v2.0.0", "tree is not v2.0.0"} {
		if !strings.Contains(advanced, want) {
			t.Fatalf("advance warning %q missing %q", advanced, want)
		}
	}

	lock, err := readModuleSourceLock(filepath.Join(target.Dir(), moduleSourceLockRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Ref != "v1.0.0" {
		t.Fatalf("recorded base ref = %q, want v1.0.0 (withheld apply must not advance it)", lock.Ref)
	}
}

// --verify-tag turns the one zero-exit path that still ships a partial tree —
// a re-affirmed apply that advances the base while keeping a masked upstream
// change local — into a non-zero exit for CI, without preventing the apply.
func TestSyncModuleVerifyTagFailsOnReaffirmedMask(t *testing.T) {
	remote, target := maskedUpstreamFixture(t)

	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{
		Source: remote, To: "v2.0.0", Subdirectory: "module", Apply: true, KeepLocalDivergences: true, VerifyTag: true,
	}); err == nil {
		t.Fatal("--verify-tag must fail when a masked upstream change is kept local")
	} else if !strings.Contains(err.Error(), "verify-tag") || !strings.Contains(err.Error(), "v2.0.0") {
		t.Fatalf("error does not explain the verify-tag failure: %v", err)
	}

	// The apply still ran: the local file is kept and the recorded base advances,
	// which is exactly why a human-readable line alone would not fail CI.
	assertSyncTestFile(t, filepath.Join(target.Dir(), "overlay.txt"), "local divergence")
	lock, err := readModuleSourceLock(filepath.Join(target.Dir(), moduleSourceLockRelativePath))
	if err != nil {
		t.Fatal(err)
	}
	if lock.Ref != "v2.0.0" {
		t.Fatalf("recorded base ref = %q, want v2.0.0 (re-affirmed apply advances it)", lock.Ref)
	}
}

// Without --verify-tag the same re-affirmed apply succeeds: keeping an
// intentional divergence local is a zero-exit outcome by design.
func TestSyncModuleReaffirmedMaskSucceedsWithoutVerifyTag(t *testing.T) {
	remote, target := maskedUpstreamFixture(t)

	if err := syncComposedModule(context.Background(), target, &moduleSyncOptions{
		Source: remote, To: "v2.0.0", Subdirectory: "module", Apply: true, KeepLocalDivergences: true,
	}); err != nil {
		t.Fatalf("--keep-local-divergences apply must succeed without --verify-tag: %v", err)
	}
}

func writeSyncTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(bytes.TrimSpace(output))
}

func syncTestDigest(contents string) string {
	digest := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(digest[:])
}

func assertSyncTestFile(t *testing.T, path, want string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != want {
		t.Fatalf("%s = %q, want %q", path, payload, want)
	}
}
