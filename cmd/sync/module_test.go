package sync

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	output "github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/integrity"
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
