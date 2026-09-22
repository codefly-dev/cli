package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/toolbox/conformance"
	"github.com/codefly-dev/core/toolbox/session"
	"github.com/stretchr/testify/require"
)

// The toolbox under test is Core's conformance fixture: a real plugin process
// with a deterministic catalog and an observable effect counter, built from
// source here rather than downloaded, so this gate owns the host boundary and
// depends on no released agent.
const (
	toolboxFixturePublisher = "codefly.dev"
	toolboxFixtureName      = conformance.FixtureName
	toolboxFixtureVersion   = conformance.FixtureVersion
)

const toolboxFixtureManifest = `name: ` + toolboxFixtureName + `
version: ` + toolboxFixtureVersion + `
description: Toolbox conformance host-boundary fixture.
agent:
  kind: codefly:toolbox
  name: ` + toolboxFixtureName + `
  publisher: ` + toolboxFixturePublisher + `
  version: ` + toolboxFixtureVersion + `
sandbox:
  read_paths: []
  write_paths: []
  network: loopback
  unix_sockets: []
permissions:
  required:
    - action: ` + conformance.IdentityTool + `
      reason: Return the deterministic structured fixture identity.
    - action: ` + conformance.DeterministicErrorTool + `
      reason: Return a stable in-band error.
    - action: ` + conformance.EffectIncrementTool + `
      reason: Exercise a bounded observable effect.
    - action: ` + conformance.EffectCountTool + `
      reason: Read the effect counter.
    - action: ` + conformance.WaitTool + `
      reason: Exercise bounded timeout behavior.
    - action: ` + conformance.CrashTool + `
      reason: Terminate the disposable fixture process.
`

const toolboxFixtureOperations = `operations:
  - name: identity
    tool: ` + conformance.IdentityTool + `
    arguments:
      subject: agent-ci
  - name: effect-count
    tool: ` + conformance.EffectCountTool + `
  - name: refused-effect
    tool: ` + conformance.EffectIncrementTool + `
    denied: true
`

const toolboxFixtureAgent = `publisher: ` + toolboxFixturePublisher + `
kind: codefly:toolbox
name: ` + toolboxFixtureName + `
version: ` + toolboxFixtureVersion + `
conformance:
  mode: toolbox-session
  fixture: conformance/operations.yaml
`

// installToolboxFixture stages an agent repository whose built artifact is the
// Core conformance toolbox, installed into an isolated Codefly home exactly
// where agent CI's build stage installs a candidate.
func installToolboxFixture(t *testing.T, operations string) string {
	t.Helper()
	agentDir := t.TempDir()
	writeFile(t, filepath.Join(agentDir, "agent.codefly.yaml"), toolboxFixtureAgent)
	writeFile(t, filepath.Join(agentDir, resources.ToolboxConfigurationName), toolboxFixtureManifest)
	require.NoError(t, os.MkdirAll(filepath.Join(agentDir, "conformance"), 0o755))
	writeFile(t, filepath.Join(agentDir, "conformance", "operations.yaml"), operations)

	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	agent := &resources.Agent{
		Kind:      resources.ToolboxAgent,
		Publisher: toolboxFixturePublisher,
		Name:      toolboxFixtureName,
		Version:   toolboxFixtureVersion,
	}
	target, err := agent.Path(context.Background())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	build := exec.Command("go", "build", "-o", target,
		"github.com/codefly-dev/core/toolbox/conformance/cmd/conformance-toolbox")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "build toolbox fixture: %s", output)
	return agentDir
}

func toolboxCandidate() *agentYAML {
	return &agentYAML{
		Publisher:   toolboxFixturePublisher,
		Kind:        string(resources.ToolboxAgent),
		Name:        toolboxFixtureName,
		Version:     toolboxFixtureVersion,
		Conformance: &agentConformance{Mode: conformanceModeToolboxSession, Fixture: "conformance/operations.yaml"},
	}
}

// TestToolboxConformanceQualifiesTheInstalledRelease is the central invariant:
// the installed artifact is launched through Core's toolbox session under its
// own declared sandbox and permission ceiling, serves every operation the owner
// declared, is refused the one the host policy denies, and releases its process.
func TestToolboxConformanceQualifiesTheInstalledRelease(t *testing.T) {
	agentDir := installToolboxFixture(t, toolboxFixtureOperations)

	payload, conformanceDir, err := runToolboxConformance(context.Background(), t.TempDir(), agentDir, toolboxCandidate())
	require.NoError(t, err)

	var evidence toolboxConformanceEvidence
	require.NoError(t, json.Unmarshal(payload, &evidence))
	require.Equal(t, toolboxFixtureName, evidence.Toolbox)
	require.Equal(t, toolboxFixtureVersion, evidence.Version)
	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, evidence.CatalogDigest)
	require.Contains(t, evidence.Tools, conformance.IdentityTool)

	outcomes := map[string]toolboxConformanceOperationEvidence{}
	for _, operation := range evidence.Operations {
		outcomes[operation.Name] = operation
	}
	require.Equal(t, "invoked", outcomes["identity"].Outcome)
	require.NotEmpty(t, outcomes["identity"].InvocationID)
	require.NotEmpty(t, outcomes["identity"].AuthorizationID)
	require.Equal(t, "invoked", outcomes["effect-count"].Outcome)
	require.Equal(t, "refused", outcomes["refused-effect"].Outcome)
	require.Empty(t, outcomes["refused-effect"].InvocationID, "a refused operation was never invoked")

	persisted, err := os.ReadFile(filepath.Join(conformanceDir, agentCIReportFilename))
	require.NoError(t, err)
	require.JSONEq(t, string(payload), string(persisted))
}

// TestToolboxConformanceFailsWhenTheDeclaredCeilingIsWiderThanTheRelease proves
// the reviewed manifest cannot claim authority the artifact does not serve.
func TestToolboxConformanceFailsWhenTheDeclaredCeilingIsWiderThanTheRelease(t *testing.T) {
	agentDir := installToolboxFixture(t, toolboxFixtureOperations)
	manifestPath := filepath.Join(agentDir, resources.ToolboxConfigurationName)
	writeFile(t, manifestPath, toolboxFixtureManifest+
		"    - action: fixture.absent.tool\n      reason: Authority the release does not serve.\n")

	_, _, err := runToolboxConformance(context.Background(), t.TempDir(), agentDir, toolboxCandidate())
	require.ErrorContains(t, err, "declares authority the release does not serve")
	require.ErrorContains(t, err, "fixture.absent.tool")
}

// TestToolboxRefusalMustComeFromTheHostPolicy proves the negative path is a
// result, not a formality: a declared refusal that the host served, that the
// tool answered with an error of its own, or that reached the plugin at all,
// fails qualification.
func TestToolboxRefusalMustComeFromTheHostPolicy(t *testing.T) {
	operation := toolboxConformanceOperation{Name: "refused", Tool: conformance.EffectIncrementTool, Denied: true}
	denial := &session.CallError{Code: session.ErrorPolicyDenied, Op: "authorize"}

	refused := &toolboxConformanceAudit{phases: []session.AuditEvent{
		{Phase: session.AuditDeny, Tool: operation.Tool},
	}}
	require.NoError(t, assertToolboxRefusedOperation(operation, denial, refused))

	require.ErrorContains(t,
		assertToolboxRefusedOperation(operation, nil, refused),
		"want a policy_denied session error")

	require.ErrorContains(t,
		assertToolboxRefusedOperation(operation, &session.CallError{Code: session.ErrorTool, Op: "invoke"}, refused),
		"want a policy_denied session error")

	unaudited := &toolboxConformanceAudit{}
	require.ErrorContains(t,
		assertToolboxRefusedOperation(operation, denial, unaudited),
		"recorded no deny audit phase")

	invoked := &toolboxConformanceAudit{phases: []session.AuditEvent{
		{Phase: session.AuditDeny, Tool: operation.Tool},
		{Phase: session.AuditInvoke, Tool: operation.Tool},
	}}
	require.ErrorContains(t,
		assertToolboxRefusedOperation(operation, denial, invoked),
		"refused tool was still invoked")
}

// TestToolboxConformanceFixtureFailsClosed proves a release cannot reach the
// session without an owner-declared suite that covers both directions.
func TestToolboxConformanceFixtureFailsClosed(t *testing.T) {
	for _, test := range []struct{ name, declaration, want string }{
		{"absent", "", "read toolbox conformance fixture"},
		{"empty", "operations: []\n", "no operation the host must serve"},
		{"no refusal", "operations:\n  - name: a\n    tool: t.a\n", "no operation the host must refuse"},
		{"only refusals", "operations:\n  - name: a\n    tool: t.a\n    denied: true\n", "no operation the host must serve"},
		{"unnamed", "operations:\n  - tool: t.a\n", "requires a name and a tool"},
		{"repeated", "operations:\n  - name: a\n    tool: t.a\n  - name: a\n    tool: t.b\n    denied: true\n", "repeats operation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if test.declaration != "" {
				writeFile(t, filepath.Join(dir, "operations.yaml"), test.declaration)
			}
			_, err := loadToolboxConformanceFixture(dir, "operations.yaml")
			require.ErrorContains(t, err, test.want)
		})
	}
}

// TestToolboxConformanceRejectsAManifestTargetingAnotherRelease proves the
// session cannot be pointed away from the artifact agent CI just built.
func TestToolboxConformanceRejectsAManifestTargetingAnotherRelease(t *testing.T) {
	agentDir := installToolboxFixture(t, toolboxFixtureOperations)
	candidate := toolboxCandidate()
	candidate.Version = "9.9.9"

	_, _, err := runToolboxConformance(context.Background(), t.TempDir(), agentDir, candidate)
	require.ErrorContains(t, err, "but the candidate is")
}
