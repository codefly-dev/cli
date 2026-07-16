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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	agentCIReportSchemaVersion = 1
	agentCIReportFilename      = "report.json"
)

var AgentCICmd = &cobra.Command{
	Use:   "ci",
	Short: "Run the Codefly-owned CI and generated-service conformance gate for an agent",
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
		payload, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr == nil {
			payload = append(payload, '\n')
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
			cli.Header(1, "Codefly agent CI %s: %d passed, %d failed, %d skipped", report.Status, report.Summary.Passed, report.Summary.Failed, report.Summary.Skipped)
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

type AgentCIReport struct {
	SchemaVersion int                  `json:"schema_version"`
	Command       string               `json:"command"`
	Agent         AgentCIIdentity      `json:"agent"`
	Status        string               `json:"status"`
	StartedAt     string               `json:"started_at"`
	FinishedAt    string               `json:"finished_at"`
	DurationMS    int64                `json:"duration_ms"`
	Summary       AgentCIReportSummary `json:"summary"`
	Stages        []AgentCIStage       `json:"stages"`
	Artifacts     []AgentCIArtifact    `json:"artifacts,omitempty"`
	Workspace     json.RawMessage      `json:"workspace_report,omitempty"`
	Error         string               `json:"error,omitempty"`
}

type AgentCIIdentity struct {
	Publisher string `json:"publisher"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

type AgentCIStage struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type AgentCIReportSummary struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type AgentCIArtifact struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type agentCIState struct {
	report       AgentCIReport
	started      time.Time
	manifest     agentYAML
	build        *agentBuildResult
	before       agentWorktreeSnapshot
	temporary    string
	agentHome    string
	conformance  string
	workspaceRaw []byte
}

func runAgentCI(ctx context.Context, options agentCIOptions) (AgentCIReport, error) {
	started := time.Now().UTC()
	state := &agentCIState{
		started: started,
		report: AgentCIReport{
			SchemaVersion: agentCIReportSchemaVersion,
			Command:       "codefly agent ci",
			Status:        "running",
			StartedAt:     started.Format(time.RFC3339Nano),
			Stages: []AgentCIStage{
				{Name: "manifest", Status: "pending"},
				{Name: "source", Status: "pending"},
				{Name: "build", Status: "pending"},
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
		state.report.Agent = AgentCIIdentity{Publisher: manifest.Publisher, Kind: manifest.Kind, Name: manifest.Name, Version: manifest.Version}
		state.before, err = snapshotAgentWorktree(ctx, options.dir)
		return err
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if err := runStage("source", func() error { return validateAgentSource(ctx, options.dir) }); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if err := runStage("build", func() error {
		log := &agentLogger{}
		result := compileAgent(ctx, options.dir, log, &sync.Mutex{}, options.nativeOnly)
		state.build = result
		if result.err != nil {
			diagnostics := strings.TrimSpace(strings.Join(log.lines, "\n"))
			if diagnostics != "" {
				return fmt.Errorf("%w\n%s", result.err, boundedAgentCIOutput([]byte(diagnostics)))
			}
			return result.err
		}
		if err := writeReleaseSBOMs(ctx, result); err != nil {
			return err
		}
		if !options.skipAudit {
			return runAudit(ctx, options.dir, result.ag, options.failOnVuln)
		}
		return nil
	}); err != nil {
		return finalizeAgentCI(state, err), err
	}
	if options.skipConformance {
		state.skipStage("conformance")
	} else if err := runStage("conformance", func() error {
		workspaceReport, conformanceDir, err := runGeneratedServiceConformance(ctx, state.temporary, state.agentHome, state.manifest)
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
	stage.StartedAt = started.Format(time.RFC3339Nano)
	err := action()
	finished := time.Now().UTC()
	stage.FinishedAt = finished.Format(time.RFC3339Nano)
	stage.DurationMS = finished.Sub(started).Milliseconds()
	if err != nil {
		stage.Status = "failed"
		stage.Error = err.Error()
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

func (state *agentCIState) stage(name string) *AgentCIStage {
	for index := range state.report.Stages {
		if state.report.Stages[index].Name == name {
			return &state.report.Stages[index]
		}
	}
	return nil
}

func finalizeAgentCI(state *agentCIState, runErr error) AgentCIReport {
	for index := range state.report.Stages {
		if state.report.Stages[index].Status == "pending" || state.report.Stages[index].Status == "running" {
			state.report.Stages[index].Status = "skipped"
		}
	}
	finished := time.Now().UTC()
	state.report.FinishedAt = finished.Format(time.RFC3339Nano)
	state.report.DurationMS = finished.Sub(state.started).Milliseconds()
	state.report.Status = "passed"
	state.report.Summary.Total = len(state.report.Stages)
	for _, stage := range state.report.Stages {
		switch stage.Status {
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
		state.report.Error = runErr.Error()
	}
	if len(state.workspaceRaw) > 0 && json.Valid(state.workspaceRaw) {
		state.report.Workspace = append(json.RawMessage(nil), state.workspaceRaw...)
	}
	return state.report
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
		return agentYAML{}, fmt.Errorf("generated-service conformance currently requires kind codefly:service, got %s", manifest.Kind)
	}
	return manifest, nil
}

func validateAgentSource(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no Codefly source-validation runner is registered for this agent repository")
		}
		return err
	}
	command := exec.CommandContext(ctx, "go", "test", "./...")
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Go agent source validation: %w\n%s", err, boundedAgentCIOutput(output))
	}
	return nil
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
	for _, entry := range bytes.Split(statusOutput, []byte{0}) {
		if len(entry) > 0 {
			entries = append(entries, string(entry))
		}
	}
	sort.Strings(entries)
	return agentWorktreeSnapshot{root: root, entries: entries}, nil
}

func runGeneratedServiceConformance(ctx context.Context, temporary, agentHome string, manifest agentYAML) ([]byte, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("resolve Codefly executable: %w", err)
	}
	workspaceName := "agent-conformance"
	workspaceDir := filepath.Join(temporary, workspaceName)
	environment := append(os.Environ(),
		resources.CodeflyHomeEnv+"="+agentHome,
		"CODEFLY_AGENT_SOURCE=local",
		"CODEFLY_COLOR=never",
		"CI=1",
	)
	commands := []struct {
		label string
		dir   string
		args  []string
	}{
		{label: "create workspace", dir: temporary, args: []string{"--timestamps=false", "init", "workspace", workspaceName, "--layout", "modules", "--default"}},
		{label: "create module", dir: workspaceDir, args: []string{"--timestamps=false", "add", "module", "app", "--yes"}},
		{label: "generate service", dir: workspaceDir, args: []string{"--timestamps=false", "--local-agents", "add", "service", "app/subject", "--agent", manifest.Publisher + "/" + manifest.Name + ":" + manifest.Version, "--default"}},
		{label: "run generated workspace gate", dir: workspaceDir, args: []string{"--timestamps=false", "--local-agents", "ci", "run", "--all", "--format", "json", "--output", ".codefly/ci"}},
	}
	for _, item := range commands {
		command := exec.CommandContext(ctx, executable, item.args...)
		command.Dir = item.dir
		command.Env = environment
		output, err := command.CombinedOutput()
		if err != nil {
			return readGeneratedWorkspaceReport(workspaceDir), filepath.Join(workspaceDir, ".codefly", "ci"), fmt.Errorf("%s: %w\n%s", item.label, err, boundedAgentCIOutput(output))
		}
	}
	return readGeneratedWorkspaceReport(workspaceDir), filepath.Join(workspaceDir, ".codefly", "ci"), nil
}

func readGeneratedWorkspaceReport(workspaceDir string) []byte {
	payload, _ := os.ReadFile(filepath.Join(workspaceDir, ".codefly", "ci", "report.json"))
	return payload
}

func persistAgentCIArtifacts(options agentCIOptions, state *agentCIState) error {
	if err := os.MkdirAll(options.output, 0o755); err != nil {
		return err
	}
	if state.build != nil {
		for _, source := range []string{state.build.nativePath, state.build.containerPath, state.build.nativePath + ".cdx.json", state.build.containerPath + ".cdx.json"} {
			if source == "" {
				continue
			}
			if _, err := os.Stat(source); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return err
			}
			relative := filepath.Join("artifacts", filepath.Base(source))
			destination := filepath.Join(options.output, relative)
			if err := copyAgentCIFile(source, destination); err != nil {
				return err
			}
			digest, err := fileSHA256(destination)
			if err != nil {
				return err
			}
			kind := "agent-binary"
			if strings.HasSuffix(source, ".cdx.json") {
				kind = "cyclonedx-sbom"
			}
			state.report.Artifacts = append(state.report.Artifacts, AgentCIArtifact{Kind: kind, Path: filepath.ToSlash(relative), SHA256: "sha256:" + digest})
		}
	}
	if state.conformance != "" {
		if err := copyAgentCIDirectory(state.conformance, filepath.Join(options.output, "workspace")); err != nil {
			return err
		}
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
