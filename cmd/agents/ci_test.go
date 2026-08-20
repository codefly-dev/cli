package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/failures"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	civ0 "github.com/codefly-dev/core/generated/go/codefly/ci/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
)

func TestLoadAgentCIManifest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), `publisher: codefly
kind: codefly:service
name: nextjs
version: 1.2.3
`)

	manifest, err := loadAgentCIManifest(dir, false)
	if err != nil {
		t.Fatalf("loadAgentCIManifest: %v", err)
	}
	if manifest.Publisher != "codefly" || manifest.Kind != "codefly:service" || manifest.Name != "nextjs" || manifest.Version != "1.2.3" {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
}

func TestAgentCIChildEnvironmentDisablesParentGoWorkspace(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, "/stale/codefly-home")
	t.Setenv("GOWORK", "/parent/go.work")
	t.Setenv("CODEFLY_AGENT_CI_SENTINEL", "preserved")

	environment := agentCIChildEnvironment(
		"/isolated/codefly-home",
		"CI=1",
		"GOWORK=/caller/go.work",
	)

	values := map[string][]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = append(values[name], value)
		}
	}
	if got := values[resources.CodeflyHomeEnv]; !reflect.DeepEqual(got, []string{"/isolated/codefly-home"}) {
		t.Fatalf("%s = %v, want isolated home only", resources.CodeflyHomeEnv, got)
	}
	if got := values["GOWORK"]; !reflect.DeepEqual(got, []string{"off"}) {
		t.Fatalf("GOWORK = %v, want standalone module mode only", got)
	}
	if got := values["CI"]; !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("CI = %v, want child override", got)
	}
	if got := values["CODEFLY_AGENT_CI_SENTINEL"]; !reflect.DeepEqual(got, []string{"preserved"}) {
		t.Fatalf("sentinel = %v, want inherited environment", got)
	}
}

func TestAgentBuildChildEnvironmentBindsDetectedGoWorkspace(t *testing.T) {
	t.Setenv("GOWORK", "/parent/go.work")

	environment := agentBuildChildEnvironment(
		"/local/codefly-home",
		"/checkout/go.work",
		"GOWORK=/caller/go.work",
	)

	values := map[string][]string{}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			values[name] = append(values[name], value)
		}
	}
	if got := values["GOWORK"]; !reflect.DeepEqual(got, []string{"/checkout/go.work"}) {
		t.Fatalf("GOWORK = %v, want detected source workspace only", got)
	}
}

func TestAgentBuildChildEnvironmentDisablesUnrelatedWorkspaceForStandaloneSource(t *testing.T) {
	t.Setenv("GOWORK", "/parent/go.work")
	environment := agentBuildChildEnvironment("/local/codefly-home", "")

	for _, entry := range environment {
		if entry == "GOWORK=off" {
			return
		}
	}
	t.Fatalf("environment omitted GOWORK=off: %v", environment)
}

func TestAgentConformanceGateRecordsAuditWithoutDuplicatingReleasePolicy(t *testing.T) {
	arguments := agentConformanceGateArguments()
	if !slices.Contains(arguments, "--fail-on-vuln=false") {
		t.Fatalf("conformance arguments = %v, want non-blocking duplicate audit", arguments)
	}
	if !slices.Contains(arguments, "--all") || !slices.Contains(arguments, "--local-agents") {
		t.Fatalf("conformance arguments = %v, want full gate against local agent", arguments)
	}
}

func TestAgentConformanceGateIsolatesPortSpacePerRun(t *testing.T) {
	arguments := agentConformanceGateArguments()
	if !slices.Contains(arguments, "--temporary-ports") {
		t.Fatalf("conformance arguments = %v, want --temporary-ports so every agent's identical app/subject identity cannot collide on one host port", arguments)
	}
}

func TestAgentAdvertisesCapabilityUsesInstalledAgentContract(t *testing.T) {
	info := &agentv0.AgentInformation{Capabilities: []*agentv0.Capability{
		{Type: agentv0.Capability_RUNTIME},
	}}
	if !agentAdvertisesCapability(info, agentv0.Capability_RUNTIME) {
		t.Fatal("runtime capability was not recognized")
	}
	if agentAdvertisesCapability(info, agentv0.Capability_BUILDER) {
		t.Fatal("absent Builder capability was invented")
	}
	if agentAdvertisesCapability(nil, agentv0.Capability_BUILDER) {
		t.Fatal("nil advertisement reported Builder support")
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
			want:    "agent CI supports codefly:service, codefly:module, codefly:toolbox, codefly:provider",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), test.content)
			_, err := loadAgentCIManifest(dir, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("loadAgentCIManifest error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoadAgentCIManifestConformanceModes(t *testing.T) {
	base := "publisher: codefly\nkind: codefly:service\nname: python\nversion: 1.2.3\n"
	tests := []struct {
		name     string
		content  string
		wantErr  string
		wantMode string
	}{
		{
			name:     "default is generated-service",
			content:  base,
			wantMode: conformanceModeGeneratedService,
		},
		{
			name:     "explicit attach-existing-source with fixture",
			content:  base + "conformance:\n  mode: attach-existing-source\n  fixture: ./conformance/fixture\n",
			wantMode: conformanceModeAttachSource,
		},
		{
			name:    "attach-existing-source without fixture is rejected",
			content: base + "conformance:\n  mode: attach-existing-source\n",
			wantErr: "requires conformance.fixture",
		},
		{
			name:    "unknown mode is rejected",
			content: base + "conformance:\n  mode: bring-your-own\n",
			wantErr: "unsupported conformance mode",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), test.content)
			manifest, err := loadAgentCIManifest(dir, false)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("loadAgentCIManifest error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadAgentCIManifest: %v", err)
			}
			if got := conformanceMode(manifest); got != test.wantMode {
				t.Fatalf("conformanceMode = %q, want %q", got, test.wantMode)
			}
		})
	}
}

func TestLoadAgentCIManifestAllowsModuleOnlyWithoutServiceConformance(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "agent.codefly.yaml"), `publisher: codefly.dev
kind: codefly:module
name: saas-starter
version: 0.0.20
`)

	if _, err := loadAgentCIManifest(dir, false); err == nil || !strings.Contains(err.Error(), "--skip-conformance") {
		t.Fatalf("module manifest with service conformance error = %v", err)
	}
	manifest, err := loadAgentCIManifest(dir, true)
	if err != nil {
		t.Fatalf("module manifest without service conformance: %v", err)
	}
	if manifest.Kind != "codefly:module" || manifest.Name != "saas-starter" {
		t.Fatalf("unexpected module manifest: %+v", manifest)
	}
}

func TestLoadAgentCIManifestAllowsToolboxAndProviderWithSkipConformance(t *testing.T) {
	for _, kind := range []string{"codefly:toolbox", "codefly:provider"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "agent.codefly.yaml"),
				"publisher: codefly.dev\nkind: "+kind+"\nname: subject\nversion: 0.0.1\n")

			if _, err := loadAgentCIManifest(dir, false); err == nil || !strings.Contains(err.Error(), "--skip-conformance") {
				t.Fatalf("%s manifest without --skip-conformance error = %v, want it to require the flag", kind, err)
			}
			manifest, err := loadAgentCIManifest(dir, true)
			if err != nil {
				t.Fatalf("%s manifest with --skip-conformance: %v", kind, err)
			}
			if manifest.Kind != kind || manifest.Name != "subject" {
				t.Fatalf("unexpected %s manifest: %+v", kind, manifest)
			}
		})
	}
}

func TestRunAttachSourceConformanceFailsClosedWithoutFixtureWorkspace(t *testing.T) {
	agentDir := t.TempDir()
	fixture := "conformance/fixture"
	if err := os.MkdirAll(filepath.Join(agentDir, fixture), 0o755); err != nil {
		t.Fatalf("create fixture dir: %v", err)
	}
	manifest := agentYAML{Conformance: &agentConformance{Mode: conformanceModeAttachSource, Fixture: fixture}}
	_, _, err := runAttachSourceConformance(context.Background(), t.TempDir(), t.TempDir(), agentDir, manifest)
	if err == nil || !strings.Contains(err.Error(), "workspace.codefly.yaml") {
		t.Fatalf("runAttachSourceConformance error = %v, want fixture missing workspace.codefly.yaml", err)
	}
}

func TestAssertFixtureTargetsAgent(t *testing.T) {
	manifest := agentYAML{Publisher: "codefly.dev", Name: "python", Version: "1.2.3"}
	service := func(publisher, name, version string) string {
		return "name: subject\nversion: 0.0.1\nagent:\n  kind: codefly:service\n  name: " + name + "\n  version: " + version + "\n  publisher: " + publisher + "\n"
	}
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "latest resolves to the build under test", content: service("codefly.dev", "python", "latest")},
		{name: "exact version under test is accepted", content: service("codefly.dev", "python", "1.2.3")},
		{
			name:    "mismatched pin is rejected",
			content: service("codefly.dev", "python", "0.9.0"),
			wantErr: `use "latest"`,
		},
		{
			name:    "unrelated agent does not satisfy the contract",
			content: service("codefly.dev", "go", "latest"),
			wantErr: "must declare a service using agent codefly.dev/python",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixtureDir := t.TempDir()
			serviceDir := filepath.Join(fixtureDir, "app", "subject")
			if err := os.MkdirAll(serviceDir, 0o755); err != nil {
				t.Fatalf("create service dir: %v", err)
			}
			writeFile(t, filepath.Join(serviceDir, "service.codefly.yaml"), test.content)
			err := assertFixtureTargetsAgent(fixtureDir, "conformance/fixture", manifest)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("assertFixtureTargetsAgent error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("assertFixtureTargetsAgent: %v", err)
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

func TestSeedAgentCISourcePackagerCopiesExactInstalledSeed(t *testing.T) {
	sourceHome := t.TempDir()
	agentHome := t.TempDir()
	source := sourcePackagerPath(sourceHome)
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("exact-packager"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := seedAgentCISourcePackager(sourceHome, agentHome); err != nil {
		t.Fatalf("seedAgentCISourcePackager: %v", err)
	}
	destination := sourcePackagerPath(agentHome)
	payload, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read seeded packager: %v", err)
	}
	if string(payload) != "exact-packager" {
		t.Fatalf("seeded payload = %q", payload)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("seeded packager is not executable: info=%v err=%v", info, err)
	}
}

func TestSeedAgentCISourcePackagerAllowsMissingSeed(t *testing.T) {
	if err := seedAgentCISourcePackager(t.TempDir(), t.TempDir()); err != nil {
		t.Fatalf("missing installed packager should fall back to source bootstrap: %v", err)
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
