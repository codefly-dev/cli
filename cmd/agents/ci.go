package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	"github.com/codefly-dev/core/failures"
	civ0 "github.com/codefly-dev/core/generated/go/codefly/ci/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"
)

const (
	agentCIReportSchemaVersion = 1
	agentCIReportFilename      = "report.json"

	// conformanceModeGeneratedService scaffolds a brand-new service through
	// Builder.Create before running the workspace gate. It is the default.
	conformanceModeGeneratedService = "generated-service"
	// conformanceModeAttachSource runs the workspace gate against a fixture
	// workspace the agent ships, for attach-only agents whose Builder.Create
	// intentionally declines to generate a project template.
	conformanceModeAttachSource = "attach-existing-source"
)

var AgentCICmd = &cobra.Command{
	Use:   "ci",
	Short: "Validate an agent against source, release, and generated-service CI gates",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		dir, err := cmd.Flags().GetString("dir")
		if err != nil {
			return err
		}
		if strings.TrimSpace(dir) == "" {
			dir, err = os.Getwd()
			if err != nil {
				return fmt.Errorf("get agent directory: %w", err)
			}
		}
		dir, err = filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("resolve agent directory: %w", err)
		}
		format, err := cmd.Flags().GetString("format")
		if err != nil {
			return err
		}
		format = strings.ToLower(strings.TrimSpace(format))
		if format != "text" && format != "json" {
			return fmt.Errorf("unsupported agent CI format %q (use text or json)", format)
		}
		if format == "json" {
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cli.SuppressOutput()
			cli.SetOutputSink(func(wool.Loglevel, string) {})
			defer func() {
				cli.SetOutputSink(nil)
				cli.RestoreOutput()
			}()
		}

		output, err := cmd.Flags().GetString("output")
		if err != nil {
			return err
		}
		if !filepath.IsAbs(output) {
			output = filepath.Join(dir, output)
		}
		if err := os.MkdirAll(output, 0o755); err != nil {
			return fmt.Errorf("create agent CI output directory: %w", err)
		}
		nativeOnly, err := cmd.Flags().GetBool("native-only")
		if err != nil {
			return err
		}
		skipAudit, err := cmd.Flags().GetBool("skip-audit")
		if err != nil {
			return err
		}
		failOnVuln, err := cmd.Flags().GetBool("fail-on-vuln")
		if err != nil {
			return err
		}
		skipConformance, err := cmd.Flags().GetBool("skip-conformance")
		if err != nil {
			return err
		}

		report, runErr := runAgentCI(ctx, agentCIOptions{
			dir:             dir,
			output:          output,
			nativeOnly:      nativeOnly,
			skipAudit:       skipAudit,
			failOnVuln:      failOnVuln,
			skipConformance: skipConformance,
		})
		payload, marshalErr := marshalAgentCIReport(report)
		if marshalErr == nil {
			marshalErr = atomicWrite(filepath.Join(output, agentCIReportFilename), payload, 0o644)
		}
		runErr = errors.Join(runErr, marshalErr)
		if format == "json" && len(payload) > 0 {
			_, _ = os.Stdout.Write(payload)
			if runErr != nil {
				return machineReadableAgentCIError{error: runErr}
			}
			return nil
		}
		if marshalErr == nil {
			summary := report.GetSummary()
			cli.Header(1, "Codefly agent CI %s: %d passed, %d failed, %d skipped", report.GetStatus(), summary.GetPassed(), summary.GetFailed(), summary.GetSkipped())
			cli.Info("Report: %s", filepath.Join(output, agentCIReportFilename))
		}
		return runErr
	},
}

func init() {
	AgentCICmd.Flags().String("dir", "", "Agent source directory (default: current directory)")
	AgentCICmd.Flags().String("format", "text", "Final agent CI report format: text or json")
	AgentCICmd.Flags().String("output", ".codefly/agent-ci", "Agent CI report and artifact directory (relative to the agent repository)")
	AgentCICmd.Flags().Bool("native-only", false, "Build only the host agent binary; skip the Linux/amd64 release artifact")
	AgentCICmd.Flags().Bool("skip-audit", false, "Explicitly skip the agent-binary vulnerability audit")
	AgentCICmd.Flags().Bool("fail-on-vuln", true, "Fail on actionable HIGH or CRITICAL agent vulnerabilities")
	AgentCICmd.Flags().Bool("skip-conformance", false, "Explicitly skip fresh generated-service conformance")
}

type machineReadableAgentCIError struct{ error }

func (machineReadableAgentCIError) MachineReadable() bool { return true }

type agentCIOptions struct {
	dir             string
	output          string
	nativeOnly      bool
	skipAudit       bool
	failOnVuln      bool
	skipConformance bool
}

type agentCIState struct {
	report       *civ0.AgentCIReport
	started      time.Time
	manifest     agentYAML
	build        *agentBuildResult
	before       agentWorktreeSnapshot
	temporary    string
	agentHome    string
	sourceHome   string
	conformance  string
	workspaceRaw []byte
}

func runAgentCI(ctx context.Context, options agentCIOptions) (*civ0.AgentCIReport, error) {
	started := time.Now().UTC()
	state := &agentCIState{
		started: started,
		report: &civ0.AgentCIReport{
			SchemaVersion: uint32(agentCIReportSchemaVersion),
			Command:       "codefly agent ci",
			Options: &civ0.AgentCIOptions{
				NativeOnly:         options.nativeOnly,
				AuditEnabled:       !options.skipAudit,
				FailOnVuln:         options.failOnVuln,
				ConformanceEnabled: !options.skipConformance,
			},
			Status:    "running",
			StartedAt: started.Format(time.RFC3339Nano),
			Summary:   &civ0.AgentCISummary{},
			Stages: []*civ0.AgentCIStage{
				{Name: "manifest", Status: "pending"},
				{Name: "source", Status: "pending"},
				{Name: "build", Status: "pending"},
				{Name: "audit", Status: "pending"},
				{Name: "conformance", Status: "pending"},
				{Name: "drift", Status: "pending"},
			},
		},
	}
	temporary, err := os.MkdirTemp("", "codefly-agent-ci-*")
	if err != nil {
		return finalizeAgentCI(state, err), err
	}
	state.temporary = temporary
	state.agentHome = filepath.Join(temporary, "home")
	state.sourceHome = resources.CodeflyHomeDir()
	defer os.RemoveAll(temporary)

	previousHome, hadHome := os.LookupEnv(resources.CodeflyHomeEnv)
	if err := os.Setenv(resources.CodeflyHomeEnv, state.agentHome); err != nil {
		return finalizeAgentCI(state, err), err
	}
	defer func() {
		if hadHome {
			_ = os.Setenv(resources.CodeflyHomeEnv, previousHome)
		} else {
			_ = os.Unsetenv(resources.CodeflyHomeEnv)
		}
		services.ClearAgents()
	}()

	runStage := func(name string, action func() error) error {
		return state.runStage(name, action)
	}

	if err := runStage("manifest", func() error {
		manifest, err := loadAgentCIManifest(options.dir)
		if err != nil {
			return err
		}
		state.manifest = manifest
		state.report.Agent = &civ0.AgentCIIdentity{Publisher: manifest.Publisher, Kind: manifest.Kind, Name: manifest.Name, Version: manifest.Version}
		state.before, err = snapshotAgentWorktree(ctx, options.dir)
		return err
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if err := runStage("source", func() error {
		return validateAgentSource(ctx, options.dir, state.sourceHome)
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if err := runStage("build", func() error {
		if err := seedAgentCISourcePackager(state.sourceHome, state.agentHome); err != nil {
			return err
		}
		log := &agentLogger{}
		result := compileAgent(ctx, options.dir, log, options.nativeOnly)
		state.build = result
		if result.err != nil {
			diagnostics := strings.TrimSpace(strings.Join(log.lines, "\n"))
			if diagnostics != "" {
				return fmt.Errorf("%w\n%s", result.err, boundedAgentCIOutput([]byte(diagnostics)))
			}
			return result.err
		}
		return nil
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if options.skipAudit {
		state.skipStage("audit")
	} else if err := runStage("audit", func() error {
		return runAudit(ctx, options.dir, state.build.ag, options.failOnVuln)
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if options.skipConformance {
		state.skipStage("conformance")
	} else if err := runStage("conformance", func() error {
		workspaceReport, conformanceDir, err := runAgentConformance(ctx, state.temporary, state.agentHome, options.dir, state.manifest)
		state.workspaceRaw = workspaceReport
		state.conformance = conformanceDir
		return err
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if err := runStage("drift", func() error {
		after, err := snapshotAgentWorktree(ctx, options.dir)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state.before.entries, after.entries) {
			return fmt.Errorf("agent CI changed repository state: before=%v after=%v", state.before.entries, after.entries)
		}
		return nil
	}); err != nil {
		// Snapshot comparison is complete, so persisting evidence cannot mask or
		// create source drift. Keep the successful build/conformance artifacts
		// available even when an external edit or a restoration bug trips this
		// final safety gate.
		persistErr := persistAgentCIArtifacts(options, state)
		err = errors.Join(err, persistErr)
		return finalizeAgentCI(state, err), err
	}
	if err := persistAgentCIArtifacts(options, state); err != nil {
		return finalizeAgentCI(state, err), err
	}
	return finalizeAgentCI(state, nil), nil
}

func (state *agentCIState) runStage(name string, action func() error) error {
	stage := state.stage(name)
	if stage == nil {
		return fmt.Errorf("unknown agent CI stage %q", name)
	}
	started := time.Now().UTC()
	stage.Status = "running"
	startedAt := started.Format(time.RFC3339Nano)
	stage.StartedAt = &startedAt
	err := action()
	finished := time.Now().UTC()
	finishedAt := finished.Format(time.RFC3339Nano)
	stage.FinishedAt = &finishedAt
	stage.DurationMs = agentCIDurationMS(finished.Sub(started))
	if err != nil {
		stage.Status = "failed"
		stageError := err.Error()
		stage.Error = &stageError
		stage.Failure = failures.FromError("agent-ci."+name, err)
		return fmt.Errorf("agent CI stage %s failed: %w", name, err)
	}
	stage.Status = "passed"
	return nil
}

func (state *agentCIState) skipStage(name string) {
	if stage := state.stage(name); stage != nil && stage.Status == "pending" {
		stage.Status = "skipped"
	}
}

func (state *agentCIState) stage(name string) *civ0.AgentCIStage {
	for _, stage := range state.report.Stages {
		if stage.GetName() == name {
			return stage
		}
	}
	return nil
}

func finalizeAgentCI(state *agentCIState, runErr error) *civ0.AgentCIReport {
	for _, stage := range state.report.Stages {
		if stage.GetStatus() == "pending" || stage.GetStatus() == "running" {
			stage.Status = "skipped"
		}
	}
	finished := time.Now().UTC()
	state.report.FinishedAt = finished.Format(time.RFC3339Nano)
	state.report.DurationMs = agentCIDurationMS(finished.Sub(state.started))
	state.report.Status = "passed"
	state.report.Summary.Total = uint32(len(state.report.Stages))
	for _, stage := range state.report.Stages {
		switch stage.GetStatus() {
		case "passed":
			state.report.Summary.Passed++
		case "failed":
			state.report.Summary.Failed++
		case "skipped":
			state.report.Summary.Skipped++
		}
	}
	if runErr != nil {
		state.report.Status = "failed"
		reportError := runErr.Error()
		state.report.Error = &reportError
		state.report.Failure = failures.FromError("agent-ci", runErr)
	}
	if len(state.workspaceRaw) > 0 && json.Valid(state.workspaceRaw) {
		workspaceReport := &structpb.Struct{}
		if err := protojson.Unmarshal(state.workspaceRaw, workspaceReport); err == nil {
			state.report.WorkspaceReport = workspaceReport
		}
	}
	return state.report
}

func marshalAgentCIReport(report *civ0.AgentCIReport) ([]byte, error) {
	payload, err := (protojson.MarshalOptions{
		Indent:            "  ",
		UseProtoNames:     true,
		EmitDefaultValues: true,
	}).Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("encode agent CI report: %w", err)
	}
	return append(payload, '\n'), nil
}

func agentCIDurationMS(duration time.Duration) int32 {
	milliseconds := duration.Milliseconds()
	if milliseconds <= 0 {
		return 0
	}
	const maximum = int64(1<<31 - 1)
	if milliseconds > maximum {
		return int32(maximum)
	}
	return int32(milliseconds)
}

func loadAgentCIManifest(dir string) (agentYAML, error) {
	payload, err := os.ReadFile(filepath.Join(dir, "agent.codefly.yaml"))
	if err != nil {
		return agentYAML{}, fmt.Errorf("read agent.codefly.yaml: %w", err)
	}
	var manifest agentYAML
	if err := yaml.Unmarshal(payload, &manifest); err != nil {
		return agentYAML{}, fmt.Errorf("parse agent.codefly.yaml: %w", err)
	}
	if manifest.Publisher == "" || manifest.Kind == "" || manifest.Name == "" || manifest.Version == "" {
		return agentYAML{}, fmt.Errorf("agent.codefly.yaml must have publisher, kind, name, and version")
	}
	if manifest.Kind != "codefly:service" {
		return agentYAML{}, fmt.Errorf("agent conformance currently requires kind codefly:service, got %s", manifest.Kind)
	}
	mode := conformanceMode(manifest)
	switch mode {
	case conformanceModeGeneratedService:
	case conformanceModeAttachSource:
		if manifest.Conformance == nil || strings.TrimSpace(manifest.Conformance.Fixture) == "" {
			return agentYAML{}, fmt.Errorf("attach-existing-source conformance requires conformance.fixture pointing to a fixture workspace")
		}
	default:
		return agentYAML{}, fmt.Errorf("unsupported conformance mode %q (use %s or %s)", mode, conformanceModeGeneratedService, conformanceModeAttachSource)
	}
	return manifest, nil
}

func conformanceMode(manifest agentYAML) string {
	if manifest.Conformance == nil || strings.TrimSpace(manifest.Conformance.Mode) == "" {
		return conformanceModeGeneratedService
	}
	return strings.TrimSpace(manifest.Conformance.Mode)
}

func validateAgentSource(ctx context.Context, dir, sourceHome string) error {
	if _, err := sourceworkspace.SelectPlugin(dir); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Codefly executable: %w", err)
	}
	command := exec.CommandContext(ctx, executable,
		"--timestamps=false",
		"test", "source",
		"--dir", dir,
		"--runtime-context", "free",
	)
	command.Dir = dir
	command.Env = agentCIChildEnvironment(sourceHome,
		"CI=1",
		"CODEFLY_COLOR=never",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("plugin-owned agent source validation: %w\n%s", err, boundedAgentCIOutput(output))
	}
	return nil
}

func agentCIChildEnvironment(codeflyHome string, additional ...string) []string {
	overrideOrder := []string{}
	overrides := map[string]string{}
	setOverride := func(name, value string) {
		if _, exists := overrides[name]; !exists {
			overrideOrder = append(overrideOrder, name)
		}
		overrides[name] = value
	}
	setOverride(resources.CodeflyHomeEnv, codeflyHome)
	for _, entry := range additional {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name != "GOWORK" {
			setOverride(name, value)
		}
	}
	// Agent CI and release packaging must prove the repository builds from its
	// published module graph. A parent or developer-local go.work can otherwise
	// make the release succeed locally and fail for every downstream consumer.
	setOverride("GOWORK", "off")

	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if _, overridden := overrides[name]; ok && overridden {
			continue
		}
		environment = append(environment, entry)
	}
	for _, name := range overrideOrder {
		environment = append(environment, name+"="+overrides[name])
	}
	return environment
}

type agentWorktreeSnapshot struct {
	root    string
	entries []string
}

func snapshotAgentWorktree(ctx context.Context, dir string) (agentWorktreeSnapshot, error) {
	rootCommand := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	rootOutput, err := rootCommand.CombinedOutput()
	if err != nil {
		return agentWorktreeSnapshot{}, fmt.Errorf("resolve agent Git root: %w: %s", err, boundedAgentCIOutput(rootOutput))
	}
	root := strings.TrimSpace(string(rootOutput))
	statusCommand := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	statusOutput, err := statusCommand.Output()
	if err != nil {
		return agentWorktreeSnapshot{}, fmt.Errorf("snapshot agent Git state: %w", err)
	}
	entries := []string{}
	for entry := range bytes.SplitSeq(statusOutput, []byte{0}) {
		if len(entry) > 0 {
			entries = append(entries, string(entry))
		}
	}
	sort.Strings(entries)
	return agentWorktreeSnapshot{root: root, entries: entries}, nil
}

func runAgentConformance(ctx context.Context, temporary, agentHome, agentDir string, manifest agentYAML) ([]byte, string, error) {
	if conformanceMode(manifest) == conformanceModeAttachSource {
		return runAttachSourceConformance(ctx, temporary, agentHome, agentDir, manifest)
	}
	return runGeneratedServiceConformance(ctx, temporary, agentHome, manifest)
}

func agentConformanceEnvironment(agentHome string) []string {
	return append(os.Environ(),
		resources.CodeflyHomeEnv+"="+agentHome,
		"CODEFLY_AGENT_SOURCE=local",
		"CODEFLY_COLOR=never",
		"CI=1",
	)
}

func runGeneratedServiceConformance(ctx context.Context, temporary, agentHome string, manifest agentYAML) ([]byte, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("resolve Codefly executable: %w", err)
	}
	workspaceName := "agent-conformance"
	workspaceDir := filepath.Join(temporary, workspaceName)
	environment := agentConformanceEnvironment(agentHome)
	setup := []struct {
		label string
		dir   string
		args  []string
	}{
		{label: "create workspace", dir: temporary, args: []string{"--timestamps=false", "init", "workspace", workspaceName, "--layout", "modules", "--default"}},
		{label: "create module", dir: workspaceDir, args: []string{"--timestamps=false", "add", "module", "app", "--yes"}},
		{label: "generate service", dir: workspaceDir, args: []string{"--timestamps=false", "--local-agents", "add", "service", "app/subject", "--agent", manifest.Publisher + "/" + manifest.Name + ":" + manifest.Version, "--default"}},
	}
	for _, item := range setup {
		command := exec.CommandContext(ctx, executable, item.args...)
		command.Dir = item.dir
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			return readGeneratedWorkspaceReport(workspaceDir), filepath.Join(workspaceDir, ".codefly", "ci"), fmt.Errorf("%s: %w\n%s", item.label, err, boundedAgentCIOutput(output))
		}
	}
	return runWorkspaceGate(ctx, executable, workspaceDir, environment)
}

// runAttachSourceConformance exercises the agent against a fixture workspace it
// ships, so attach-only agents whose Builder.Create declines to scaffold a
// project template are still covered end to end. The fixture is copied out of
// the agent repository so the gate never mutates the agent worktree.
func runAttachSourceConformance(ctx context.Context, temporary, agentHome, agentDir string, manifest agentYAML) ([]byte, string, error) {
	fixture := manifest.Conformance.Fixture
	fixtureDir := fixture
	if !filepath.IsAbs(fixtureDir) {
		fixtureDir = filepath.Join(agentDir, fixture)
	}
	if _, err := os.Stat(filepath.Join(fixtureDir, resources.WorkspaceConfigurationName)); err != nil {
		return nil, "", fmt.Errorf("attach-existing-source conformance fixture %q must contain %s: %w", fixture, resources.WorkspaceConfigurationName, err)
	}
	if err := assertFixtureTargetsAgent(fixtureDir, fixture, manifest); err != nil {
		return nil, "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("resolve Codefly executable: %w", err)
	}
	workspaceDir := filepath.Join(temporary, "agent-conformance")
	if err := copyAgentCIDirectory(fixtureDir, workspaceDir); err != nil {
		return nil, "", fmt.Errorf("provision attach-existing-source fixture: %w", err)
	}
	// The gate owns .codefly/ci; discard any copy the fixture happened to ship so
	// the run starts from a clean report directory.
	if err := os.RemoveAll(filepath.Join(workspaceDir, ".codefly")); err != nil {
		return nil, "", fmt.Errorf("reset attach-existing-source fixture state: %w", err)
	}
	return runWorkspaceGate(ctx, executable, workspaceDir, agentConformanceEnvironment(agentHome))
}

// assertFixtureTargetsAgent fails closed when no fixture service exercises the
// agent under test at a version the local build can resolve. Conformance runs
// under CODEFLY_AGENT_SOURCE=local, where "latest" resolves to the just-built
// binary and a pin only resolves when it equals the version under test; any
// other pin would silently exercise nothing, so we reject it up front with a
// targeted message instead of a generic downstream "agent not found".
func assertFixtureTargetsAgent(fixtureDir, fixture string, manifest agentYAML) error {
	referenced := false
	err := filepath.WalkDir(fixtureDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != resources.ServiceConfigurationName {
			return nil
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var declaration struct {
			Agent struct {
				Name      string `yaml:"name"`
				Version   string `yaml:"version"`
				Publisher string `yaml:"publisher"`
			} `yaml:"agent"`
		}
		if err := yaml.Unmarshal(payload, &declaration); err != nil {
			return fmt.Errorf("parse fixture service %s: %w", path, err)
		}
		agent := declaration.Agent
		if agent.Publisher != manifest.Publisher || agent.Name != manifest.Name {
			return nil
		}
		referenced = true
		if agent.Version != "latest" && agent.Version != manifest.Version {
			return fmt.Errorf("attach-existing-source fixture %q pins agent %s/%s at version %q; use \"latest\" or %q so the locally built agent under test is exercised", fixture, manifest.Publisher, manifest.Name, agent.Version, manifest.Version)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !referenced {
		return fmt.Errorf("attach-existing-source fixture %q must declare a service using agent %s/%s", fixture, manifest.Publisher, manifest.Name)
	}
	return nil
}

func runWorkspaceGate(ctx context.Context, executable, workspaceDir string, environment []string) ([]byte, string, error) {
	command := exec.CommandContext(ctx, executable, agentConformanceGateArguments()...)
	command.Dir = workspaceDir
	command.Env = environment
	output, err := command.CombinedOutput()
	conformanceDir := filepath.Join(workspaceDir, ".codefly", "ci")
	report := readGeneratedWorkspaceReport(workspaceDir)
	if len(report) == 0 {
		if candidate := bytes.TrimSpace(output); json.Valid(candidate) {
			report = append([]byte(nil), candidate...)
		}
	}
	if err != nil {
		return report, conformanceDir, fmt.Errorf("run workspace gate: %w\n%s", err, boundedAgentCIOutput(output))
	}
	return report, conformanceDir, nil
}

func agentConformanceGateArguments() []string {
	return []string{
		"--timestamps=false",
		"--local-agents",
		"ci", "run",
		"--all",
		"--format", "json",
		"--output", ".codefly/ci",
		// The preceding agent audit is the release's vulnerability policy gate
		// and fails on actionable HIGH/CRITICAL findings. Conformance still
		// records audit evidence, but must not reinterpret unpatched upstream
		// findings as a second, stricter release failure.
		"--fail-on-vuln=false",
	}
}

func readGeneratedWorkspaceReport(workspaceDir string) []byte {
	payload, _ := os.ReadFile(filepath.Join(workspaceDir, ".codefly", "ci", "report.json"))
	return payload
}

func persistAgentCIArtifacts(options agentCIOptions, state *agentCIState) error {
	if err := os.MkdirAll(options.output, 0o755); err != nil {
		return err
	}
	// The child writes its report atomically as it exits. Recover it from the
	// persisted conformance directory if the immediate post-command read raced
	// that final rename, so the outer report remains self-contained.
	if len(state.workspaceRaw) == 0 && state.conformance != "" {
		payload, err := os.ReadFile(filepath.Join(state.conformance, agentCIReportFilename))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if json.Valid(payload) {
			state.workspaceRaw = payload
		}
	}
	if state.build != nil {
		releaseArtifacts := []struct {
			source string
			target string
			kind   string
		}{
			{source: state.build.nativePath, target: runtime.GOOS + "/" + runtime.GOARCH, kind: "agent-binary"},
			{source: state.build.containerPath, target: "linux/amd64", kind: "agent-binary"},
			{source: state.build.nativePath + ".cdx.json", target: runtime.GOOS + "/" + runtime.GOARCH, kind: "cyclonedx-sbom"},
			{source: state.build.containerPath + ".cdx.json", target: "linux/amd64", kind: "cyclonedx-sbom"},
		}
		// On a linux/amd64 host the native and container builds share a
		// target, so both would map to the same release path; publish the
		// first and skip the duplicate instead of silently overwriting it.
		persisted := make(map[string]bool)
		for _, artifact := range releaseArtifacts {
			if artifact.source == "" || artifact.source == ".cdx.json" {
				continue
			}
			if _, err := os.Stat(artifact.source); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			platformDirectory := strings.ReplaceAll(artifact.target, "/", "-")
			relative := filepath.Join("artifacts", platformDirectory, filepath.Base(artifact.source))
			if persisted[relative] {
				continue
			}
			persisted[relative] = true
			destination := filepath.Join(options.output, relative)
			if err := copyAgentCIFile(artifact.source, destination); err != nil {
				return err
			}
			digest, err := fileSHA256(destination)
			if err != nil {
				return err
			}
			state.report.Artifacts = append(state.report.Artifacts, &civ0.AgentCIArtifact{
				Kind:   artifact.kind,
				Path:   filepath.ToSlash(relative),
				Sha256: "sha256:" + digest,
				Target: artifact.target,
			})
		}
	}
	if state.conformance != "" {
		if err := copyAgentCIDirectory(state.conformance, filepath.Join(options.output, "workspace")); err != nil {
			return err
		}
	}
	return nil
}

// seedAgentCISourcePackager copies the exact installed Go packager into the
// isolated CI home when one is available. Publishing the generic Go agent
// temporarily bumps its own manifest before release CI; that bumped source
// cannot bootstrap the older packager version selected by this CLI. Seeding
// the already-installed exact version breaks that cycle while the isolated
// runner still packages and validates the bumped source under test.
func seedAgentCISourcePackager(sourceHome, agentHome string) error {
	source := sourcePackagerPath(sourceHome)
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect installed source packager: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("installed source packager %s is not an executable file", source)
	}
	if err := copyAgentCIFile(source, sourcePackagerPath(agentHome)); err != nil {
		return fmt.Errorf("seed isolated source packager: %w", err)
	}
	return nil
}

func copyAgentCIDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(destination, relative), 0o755)
		}
		return copyAgentCIFile(path, filepath.Join(destination, relative))
	})
}

func copyAgentCIFile(source, destination string) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(source); err == nil && info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return atomicWrite(destination, payload, mode)
}

func boundedAgentCIOutput(output []byte) string {
	const maximum = 6000
	text := strings.TrimSpace(string(output))
	if len(text) <= maximum {
		return text
	}
	const marker = "\n… output truncated …\n"
	end := maximum / 4
	start := maximum - end - len(marker)
	return text[:start] + marker + text[len(text)-end:]
}
