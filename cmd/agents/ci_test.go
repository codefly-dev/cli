package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadAgentCIManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), `publisher: codefly
kind: codefly:service
name: nextjs
version: 1.2.3
`)

	manifest, err := loadAgentCIManifest(dir)
	if err != nil {
		t.Fatalf("loadAgentCIManifest: %v", err)
	}
	if manifest.Publisher != "codefly" || manifest.Kind != "codefly:service" || manifest.Name != "nextjs" || manifest.Version != "1.2.3" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
}

func TestLoadAgentCIManifestRejectsIncompleteAndUnsupported(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "incomplete",
			content: "publisher: codefly\nkind: codefly:service\nname: nextjs\n",
			want:    "must have publisher, kind, name, and version",
		},
		{
			name:    "unsupported kind",
			content: "publisher: codefly\nkind: codefly:application\nname: app\nversion: 1.0.0\n",
			want:    "requires kind codefly:service",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), test.content)
			_, err := loadAgentCIManifest(dir)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadAgentCIManifest error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestBoundedAgentCIOutputPreservesBothEnds(t *testing.T) {
	input := "START-" + strings.Repeat("x", 10_000) + "-END"
	got := boundedAgentCIOutput([]byte(input))
	if len(got) > 6_000 {
		t.Fatalf("bounded output length = %d, want <= 6000", len(got))
	}
	if !strings.HasPrefix(got, "START-") || !strings.HasSuffix(got, "-END") {
		t.Fatal("bounded output did not preserve both ends")
	}
	if !strings.Contains(got, "output truncated") {
		t.Fatal("bounded output missing truncation marker")
	}
}

func TestSnapshotAgentWorktreeTracksDirtyStateWithoutMutatingIt(t *testing.T) {
	dir := t.TempDir()
	runGitForAgentCITest(t, dir, "init")
	runGitForAgentCITest(t, dir, "config", "user.email", "ci@example.com")
	runGitForAgentCITest(t, dir, "config", "user.name", "CI")
	runGitForAgentCITest(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "tracked.txt"), "clean\n")
	runGitForAgentCITest(t, dir, "add", "tracked.txt")
	runGitForAgentCITest(t, dir, "commit", "-m", "initial")
	writeFile(t, filepath.Join(dir, "tracked.txt"), "dirty\n")

	before, err := snapshotAgentWorktree(context.Background(), dir)
	if err != nil {
		t.Fatalf("snapshot before: %v", err)
	}
	after, err := snapshotAgentWorktree(context.Background(), dir)
	if err != nil {
		t.Fatalf("snapshot after: %v", err)
	}
	if !reflect.DeepEqual(before.entries, after.entries) {
		t.Fatalf("unchanged dirty state differed: before=%v after=%v", before.entries, after.entries)
	}
	writeFile(t, filepath.Join(dir, "untracked.txt"), "new\n")
	changed, err := snapshotAgentWorktree(context.Background(), dir)
	if err != nil {
		t.Fatalf("snapshot changed: %v", err)
	}
	if reflect.DeepEqual(before.entries, changed.entries) {
		t.Fatal("snapshot did not detect a newly introduced file")
	}
}

func TestFinalizeAgentCIReport(t *testing.T) {
	started := time.Now().UTC().Add(-time.Second)
	state := &agentCIState{
		started: started,
		report: AgentCIReport{
			Status:    "running",
			StartedAt: started.Format(time.RFC3339Nano),
			Stages: []AgentCIStage{
				{Name: "manifest", Status: "passed"},
				{Name: "source", Status: "failed"},
				{Name: "build", Status: "pending"},
			},
		},
	}
	report := finalizeAgentCI(state, context.Canceled)
	if report.Status != "failed" || report.Summary.Passed != 1 || report.Summary.Failed != 1 || report.Summary.Skipped != 1 {
		t.Fatalf("unexpected finalized report: %+v", report)
	}
	if report.Error != context.Canceled.Error() {
		t.Fatalf("report error = %q, want %q", report.Error, context.Canceled.Error())
	}
}

func runGitForAgentCITest(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
