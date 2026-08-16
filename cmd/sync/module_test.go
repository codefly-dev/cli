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

	printModuleSyncPlan("app", integrity.BaseSyncPlan{
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
	if err := writeModuleSourceLock(filepath.Join(targetRoot, moduleSourceLockRelativePath), moduleSourceLock{
		Schema: moduleSourceLockSchema, Repository: remote, Ref: "v1.2.3",
		Commit: runGit(t, repository, "rev-parse", "HEAD"), Subdirectory: "module",
	}); err != nil {
		t.Fatal(err)
	}
	target, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncComposedModule(context.Background(), target, moduleSyncOptions{RestoreCode: true}); err != nil {
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
	if err := syncComposedModule(context.Background(), target, moduleSyncOptions{
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

	_, cleanup, err := resolveModuleSource(context.Background(), t.TempDir(), moduleSyncOptions{
		Source: remote, To: "v1.2.3", Subdirectory: "module",
	})
	cleanup()
	if err == nil {
		t.Fatal("semantic-version branch was accepted as an immutable tag")
	}

	resolved, cleanup, err := resolveModuleSource(context.Background(), t.TempDir(), moduleSyncOptions{
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
	if err := writeModuleSourceLock(path, want); err != nil {
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

func TestSyncModuleWithoutCreateReturnsActionableError(t *testing.T) {
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "consumer", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	loaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = loadSyncTargetModule(context.Background(), loaded, "saas", false)
	if err == nil {
		t.Fatal("syncing a missing module without --create returned success")
	}
	message := err.Error()
	if !strings.Contains(message, "codefly add module saas") || !strings.Contains(message, "--create") {
		t.Fatalf("error is not actionable: %v", err)
	}
}

func TestSyncModuleCreateRegistersMissingModule(t *testing.T) {
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "consumer", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	loaded, err := resources.LoadWorkspaceFromDir(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	module, err := loadSyncTargetModule(context.Background(), loaded, "saas", true)
	if err != nil {
		t.Fatal(err)
	}
	if module.Name != "saas" {
		t.Fatalf("registered module = %q, want saas", module.Name)
	}
	if !loaded.ExistsModule("saas") {
		t.Fatal("module was not registered in the workspace")
	}
	if _, err := os.Stat(filepath.Join(module.Dir(), "module.codefly.yaml")); err != nil {
		t.Fatalf("module directory was not scaffolded: %v", err)
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
