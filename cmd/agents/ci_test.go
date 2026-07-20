package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/failures"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	civ0 "github.com/codefly-dev/core/generated/go/codefly/ci/v0"
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
		started:      started,
		workspaceRaw: []byte(`{"schema_version":1,"status":"failed"}`),
		report: &civ0.AgentCIReport{
			Status:    "running",
			StartedAt: started.Format(time.RFC3339Nano),
			Summary:   &civ0.AgentCISummary{},
			Stages: []*civ0.AgentCIStage{
				{Name: "manifest", Status: "passed"},
				{Name: "source", Status: "failed"},
				{Name: "build", Status: "pending"},
			},
		},
	}
	report := finalizeAgentCI(state, context.Canceled)
	if report.GetStatus() != "failed" || report.GetSummary().GetPassed() != 1 || report.GetSummary().GetFailed() != 1 || report.GetSummary().GetSkipped() != 1 {
		t.Fatalf("unexpected finalized report: %+v", report)
	}
	if report.GetError() != context.Canceled.Error() {
		t.Fatalf("report error = %q, want %q", report.GetError(), context.Canceled.Error())
	}
	if report.GetFailure().GetCode() != basev0.FailureCode_FAILURE_CODE_CANCELLED || report.GetFailure().GetOperation() != "agent-ci" {
		t.Fatalf("report failure = %+v, want cancelled agent-ci failure", report.GetFailure())
	}
	if report.GetWorkspaceReport().AsMap()["status"] != "failed" {
		t.Fatalf("workspace report = %v, want failed status", report.GetWorkspaceReport())
	}
}

func TestAgentCIStagePreservesTypedFailure(t *testing.T) {
	state := &agentCIState{report: &civ0.AgentCIReport{
		Stages: []*civ0.AgentCIStage{{Name: "source", Status: "pending"}},
	}}
	want := basev0.FailureCode_FAILURE_CODE_VALIDATION_FAILED
	err := state.runStage("source", func() error {
		return failures.Wrap(want, "runtime.test", "source contract failed", nil)
	})
	if err == nil {
		t.Fatal("runStage returned nil, want failure")
	}
	stage := state.stage("source")
	if stage.GetStatus() != "failed" || stage.GetFailure().GetCode() != want {
		t.Fatalf("stage = %+v, want typed validation failure", stage)
	}
	if stage.GetFailure().GetOperation() != "runtime.test" {
		t.Fatalf("stage failure operation = %q, want runtime.test", stage.GetFailure().GetOperation())
	}
}

func TestMarshalAgentCIReportUsesGeneratedProtoFieldNamesAndDefaults(t *testing.T) {
	report := &civ0.AgentCIReport{
		SchemaVersion: 1,
		Command:       "codefly agent ci",
		Agent:         &civ0.AgentCIIdentity{},
		Options:       &civ0.AgentCIOptions{},
		Summary:       &civ0.AgentCISummary{},
		Stages:        []*civ0.AgentCIStage{{Name: "audit", Status: "skipped"}},
	}
	payload, err := marshalAgentCIReport(report)
	if err != nil {
		t.Fatalf("marshalAgentCIReport: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatalf("decode generated report JSON: %v", err)
	}
	if document["schema_version"] != float64(1) || document["duration_ms"] != float64(0) {
		t.Fatalf("generated report numeric fields = %v", document)
	}
	options := document["options"].(map[string]any)
	if options["audit_enabled"] != false {
		t.Fatalf("generated report options = %v", options)
	}
	if artifacts := document["artifacts"].([]any); len(artifacts) != 0 {
		t.Fatalf("generated report artifacts = %v, want empty", artifacts)
	}
}

func TestPersistAgentCIArtifactsRecoversWorkspaceReport(t *testing.T) {
	conformance := t.TempDir()
	payload := []byte(`{"schema_version":1,"status":"passed"}`)
	writeFile(t, filepath.Join(conformance, agentCIReportFilename), string(payload))
	state := &agentCIState{conformance: conformance}
	output := t.TempDir()
	if err := persistAgentCIArtifacts(agentCIOptions{output: output}, state); err != nil {
		t.Fatalf("persistAgentCIArtifacts: %v", err)
	}
	if string(state.workspaceRaw) != string(payload) {
		t.Fatalf("recovered workspace report = %s, want %s", state.workspaceRaw, payload)
	}
	if _, err := os.Stat(filepath.Join(output, "workspace", agentCIReportFilename)); err != nil {
		t.Fatalf("persisted workspace report: %v", err)
	}
}

func TestPersistAgentCIArtifactsSeparatesReleaseTargets(t *testing.T) {
	nativeDir := t.TempDir()
	containerDir := t.TempDir()
	native := filepath.Join(nativeDir, "nextjs__1.0.0")
	container := filepath.Join(containerDir, "nextjs__1.0.0")
	writeFile(t, native, "native")
	writeFile(t, container, "linux")
	state := &agentCIState{
		report: &civ0.AgentCIReport{},
		build:  &agentBuildResult{nativePath: native, containerPath: container},
	}
	output := t.TempDir()
	if err := persistAgentCIArtifacts(agentCIOptions{output: output}, state); err != nil {
		t.Fatalf("persistAgentCIArtifacts: %v", err)
	}
	hostTarget := runtime.GOOS + "/" + runtime.GOARCH
	if hostTarget == "linux/amd64" {
		// Native and container targets coincide: exactly one binary published.
		if len(state.report.Artifacts) != 1 {
			t.Fatalf("persisted artifacts = %v, want one binary on a linux/amd64 host", state.report.Artifacts)
		}
		if state.report.Artifacts[0].GetTarget() != "linux/amd64" {
			t.Fatalf("release artifact target = %q, want linux/amd64", state.report.Artifacts[0].GetTarget())
		}
	} else {
		if len(state.report.Artifacts) != 2 {
			t.Fatalf("persisted artifacts = %v, want two binaries", state.report.Artifacts)
		}
		first, second := state.report.Artifacts[0], state.report.Artifacts[1]
		if first.GetPath() == second.GetPath() || first.GetSha256() == second.GetSha256() {
			t.Fatalf("release artifacts collided: first=%v second=%v", first, second)
		}
		if first.GetTarget() != hostTarget || second.GetTarget() != "linux/amd64" {
			t.Fatalf("release artifact targets: first=%q second=%q", first.GetTarget(), second.GetTarget())
		}
	}
	for _, artifact := range state.report.Artifacts {
		if _, err := os.Stat(filepath.Join(output, filepath.FromSlash(artifact.GetPath()))); err != nil {
			t.Fatalf("persisted %s: %v", artifact.GetPath(), err)
		}
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
