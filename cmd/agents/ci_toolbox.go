package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/core/policy"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/toolbox/launch"
	"github.com/codefly-dev/core/toolbox/session"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"
)

const toolboxConformanceEvidenceVersion = 1

// toolboxConformanceFixture is the owner-declared operation suite a toolbox
// release must pass. The split is deliberate: the host owns the principal, the
// policy decision point, the session scope and the catalog admission; the owner
// owns which tools are exercised, with which arguments, and which one the host
// must refuse.
type toolboxConformanceFixture struct {
	Operations []toolboxConformanceOperation `yaml:"operations"`
}

type toolboxConformanceOperation struct {
	Name      string         `yaml:"name"`
	Tool      string         `yaml:"tool"`
	Arguments map[string]any `yaml:"arguments,omitempty"`
	Denied    bool           `yaml:"denied,omitempty"`
}

type toolboxConformanceEvidence struct {
	SchemaVersion int                                   `json:"schema_version"`
	Toolbox       string                                `json:"toolbox"`
	Version       string                                `json:"version"`
	CatalogDigest string                                `json:"catalog_digest"`
	Tools         []string                              `json:"tools"`
	Operations    []toolboxConformanceOperationEvidence `json:"operations"`
}

type toolboxConformanceOperationEvidence struct {
	Name            string `json:"name"`
	Tool            string `json:"tool"`
	Outcome         string `json:"outcome"`
	InvocationID    string `json:"invocation_id,omitempty"`
	AuthorizationID string `json:"authorization_id,omitempty"`
	RequestDigest   string `json:"request_digest,omitempty"`
}

// toolboxConformanceDecider is the host policy decision point for the run. It
// allows every declared operation except the ones the owner declared the host
// must refuse, which is what makes the negative authorization path observable.
type toolboxConformanceDecider struct {
	denied map[string]bool
}

func (d toolboxConformanceDecider) Evaluate(_ context.Context, request *policy.PDPRequest) policy.PDPDecision {
	if d.denied[request.Tool] {
		return policy.PDPDecision{Allow: false, Reason: "conformance policy refuses " + request.Tool}
	}
	return policy.PDPDecision{Allow: true}
}

// toolboxConformanceAudit keeps the ordered lifecycle phases per tool. A denial
// is only evidence of a host refusal when no invocation phase precedes it.
type toolboxConformanceAudit struct {
	phases []session.AuditEvent
}

//nolint:gocritic // hugeParam: session.AuditSink fixes this by-value signature.
func (a *toolboxConformanceAudit) Record(_ context.Context, event session.AuditEvent) error {
	a.phases = append(a.phases, event)
	return nil
}

func (a *toolboxConformanceAudit) recorded(tool string, phase session.AuditPhase) bool {
	for index := range a.phases {
		if a.phases[index].Tool == tool && a.phases[index].Phase == phase {
			return true
		}
	}
	return false
}

func (a *toolboxConformanceAudit) sawPhase(phase session.AuditPhase) bool {
	for index := range a.phases {
		if a.phases[index].Phase == phase {
			return true
		}
	}
	return false
}

// runToolboxConformance qualifies the built toolbox through Core's toolbox
// session: the manifest's own sandbox and permission ceiling, a host-owned
// principal and decider, the exact advertised catalog, every owner-declared
// operation, the owner-declared refusal, and session cleanup.
func runToolboxConformance(ctx context.Context, temporary, agentDir string, agent *agentYAML) ([]byte, string, error) {
	conformanceDir := filepath.Join(temporary, "toolbox-conformance")
	fixture, err := loadToolboxConformanceFixture(agentDir, agent.Conformance.Fixture)
	if err != nil {
		return nil, "", err
	}
	manifest, err := resources.LoadToolboxFromDir(ctx, agentDir)
	if err != nil {
		return nil, "", fmt.Errorf("toolbox conformance: %w", err)
	}
	if targetErr := assertToolboxTargetsCandidate(manifest, agent); targetErr != nil {
		return nil, "", targetErr
	}
	workspace := filepath.Join(conformanceDir, "workspace")
	if mkdirErr := os.MkdirAll(workspace, 0o755); mkdirErr != nil {
		return nil, "", mkdirErr
	}

	denied := map[string]bool{}
	for _, operation := range fixture.Operations {
		if operation.Denied {
			denied[operation.Tool] = true
		}
	}
	audit := &toolboxConformanceAudit{}
	opened, err := session.Open(ctx, session.Options{
		Manifest:  manifest,
		Workspace: workspace,
		Principal: &policy.Principal{
			ID:        "agent-ci-conformance",
			Kind:      policy.KindHuman,
			ExpiresAt: time.Now().Add(time.Hour),
		},
		Decider: toolboxConformanceDecider{denied: denied},
		Scope: session.Scope{
			TenantID:    "agent-ci",
			Environment: "conformance",
			ReleaseID:   agent.Publisher + "/" + agent.Name + ":" + agent.Version,
		},
		// The manifest's declared sandbox is always applied. Production
		// admission additionally demands an enforcing OS backend, which a
		// release host is not guaranteed to provide; session.Open still holds
		// the manifest to ValidateForProduction either way, so an undeclared
		// sandbox or permission ceiling fails here.
		Launch:    launch.Options{Admission: launch.AdmissionLocal},
		SessionID: "agent-ci-toolbox-conformance",
		Audit:     audit,
	})
	if err != nil {
		return nil, conformanceDir, fmt.Errorf("toolbox conformance: %w", err)
	}
	catalog := opened.Catalog()
	evidence := toolboxConformanceEvidence{
		SchemaVersion: toolboxConformanceEvidenceVersion,
		Toolbox:       catalog.Identity.GetName(),
		Version:       catalog.Identity.GetVersion(),
		CatalogDigest: catalog.Digest,
		Tools:         catalog.ToolNames(),
	}
	runErr := exerciseToolboxOperations(ctx, opened, manifest, fixture, audit, &evidence)
	if closeErr := closeToolboxSession(ctx, opened, audit); runErr == nil {
		runErr = closeErr
	}
	payload, marshalErr := json.MarshalIndent(evidence, "", "  ")
	if marshalErr != nil {
		return nil, conformanceDir, marshalErr
	}
	payload = append(payload, '\n')
	if err := atomicWrite(filepath.Join(conformanceDir, agentCIReportFilename), payload, 0o644); err != nil {
		return nil, conformanceDir, err
	}
	return payload, conformanceDir, runErr
}

func exerciseToolboxOperations(
	ctx context.Context,
	opened *session.ToolboxSession,
	manifest *resources.Toolbox,
	fixture toolboxConformanceFixture,
	audit *toolboxConformanceAudit,
	evidence *toolboxConformanceEvidence,
) error {
	if err := assertToolboxCatalogIsExact(manifest, evidence.Tools, fixture); err != nil {
		return err
	}
	for _, operation := range fixture.Operations {
		arguments, err := structpb.NewStruct(operation.Arguments)
		if err != nil {
			return fmt.Errorf("toolbox conformance operation %q: arguments: %w", operation.Name, err)
		}
		result, callErr := opened.Call(ctx, session.CallRequest{
			Name:      operation.Tool,
			Arguments: arguments,
			RequestID: operation.Name,
		})
		record := toolboxConformanceOperationEvidence{Name: operation.Name, Tool: operation.Tool}
		if operation.Denied {
			if err := assertToolboxRefusedOperation(operation, callErr, audit); err != nil {
				return err
			}
			record.Outcome = "refused"
			evidence.Operations = append(evidence.Operations, record)
			continue
		}
		if callErr != nil {
			return fmt.Errorf("toolbox conformance operation %q: %w", operation.Name, callErr)
		}
		record.Outcome = "invoked"
		record.InvocationID = result.InvocationID
		record.AuthorizationID = result.AuthorizationID
		record.RequestDigest = result.RequestDigest
		evidence.Operations = append(evidence.Operations, record)
	}
	return nil
}

// assertToolboxRefusedOperation proves the refusal came from the host policy
// decision point before the plugin was reached, not from a tool that answered
// with an error of its own.
func assertToolboxRefusedOperation(operation toolboxConformanceOperation, callErr error, audit *toolboxConformanceAudit) error {
	var refusal *session.CallError
	if !errors.As(callErr, &refusal) || refusal.Code != session.ErrorPolicyDenied {
		return fmt.Errorf("toolbox conformance operation %q: declared refusal returned %v, want a %s session error",
			operation.Name, callErr, session.ErrorPolicyDenied)
	}
	if !audit.recorded(operation.Tool, session.AuditDeny) {
		return fmt.Errorf("toolbox conformance operation %q: refusal recorded no %s audit phase", operation.Name, session.AuditDeny)
	}
	if audit.recorded(operation.Tool, session.AuditInvoke) {
		return fmt.Errorf("toolbox conformance operation %q: refused tool was still invoked", operation.Name)
	}
	return nil
}

// closeToolboxSession proves the session released the plugin it owns: cleanup
// is audited and the closed session refuses further work.
func closeToolboxSession(ctx context.Context, opened *session.ToolboxSession, audit *toolboxConformanceAudit) error {
	if err := opened.Close(); err != nil {
		return fmt.Errorf("toolbox conformance: cleanup: %w", err)
	}
	if !audit.sawPhase(session.AuditCleanup) {
		return fmt.Errorf("toolbox conformance: cleanup recorded no %s audit phase", session.AuditCleanup)
	}
	_, err := opened.Call(ctx, session.CallRequest{Name: "any", Arguments: &structpb.Struct{}})
	var closed *session.CallError
	if !errors.As(err, &closed) || closed.Code != session.ErrorClosed {
		return fmt.Errorf("toolbox conformance: closed session answered %v, want %s", err, session.ErrorClosed)
	}
	return nil
}

// assertToolboxCatalogIsExact closes the half of catalog admission Core cannot.
// Core proves every advertised tool is covered by the reviewed permission
// ceiling; this proves the reviewed ceiling claims no authority the release
// does not actually serve, and that every declared operation names a live tool.
func assertToolboxCatalogIsExact(manifest *resources.Toolbox, advertised []string, fixture toolboxConformanceFixture) error {
	serving := map[string]bool{}
	for _, name := range advertised {
		serving[name] = true
	}
	unserved := []string{}
	for _, declaration := range manifest.Permissions.All() {
		if !serving[declaration.Action] {
			unserved = append(unserved, declaration.Action)
		}
	}
	if len(unserved) > 0 {
		sort.Strings(unserved)
		return fmt.Errorf("%s declares authority the release does not serve: %s",
			resources.ToolboxConfigurationName, strings.Join(unserved, ", "))
	}
	for _, operation := range fixture.Operations {
		if !serving[operation.Tool] {
			return fmt.Errorf("toolbox conformance operation %q names tool %q, which the release does not advertise",
				operation.Name, operation.Tool)
		}
	}
	return nil
}

// assertToolboxTargetsCandidate keeps the session on the artifact agent CI just
// built. A manifest pointing anywhere else would qualify a different release.
func assertToolboxTargetsCandidate(manifest *resources.Toolbox, agent *agentYAML) error {
	target := manifest.Agent
	if target.Publisher != agent.Publisher || target.Name != agent.Name || target.Version != agent.Version {
		return fmt.Errorf("%s selects %s, but the candidate is %s/%s:%s",
			resources.ToolboxConfigurationName, target.Identifier(), agent.Publisher, agent.Name, agent.Version)
	}
	return nil
}

func loadToolboxConformanceFixture(agentDir, fixture string) (toolboxConformanceFixture, error) {
	path := fixture
	if !filepath.IsAbs(path) {
		path = filepath.Join(agentDir, fixture)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return toolboxConformanceFixture{}, fmt.Errorf("read toolbox conformance fixture %q: %w", fixture, err)
	}
	var declared toolboxConformanceFixture
	if err := yaml.Unmarshal(payload, &declared); err != nil {
		return toolboxConformanceFixture{}, fmt.Errorf("parse toolbox conformance fixture %q: %w", fixture, err)
	}
	refusals := 0
	names := map[string]bool{}
	for index, operation := range declared.Operations {
		if strings.TrimSpace(operation.Name) == "" || strings.TrimSpace(operation.Tool) == "" {
			return toolboxConformanceFixture{}, fmt.Errorf("toolbox conformance fixture %q operation %d requires a name and a tool", fixture, index)
		}
		if names[operation.Name] {
			return toolboxConformanceFixture{}, fmt.Errorf("toolbox conformance fixture %q repeats operation %q", fixture, operation.Name)
		}
		names[operation.Name] = true
		if operation.Denied {
			refusals++
		}
	}
	if len(declared.Operations) == refusals {
		return toolboxConformanceFixture{}, fmt.Errorf("toolbox conformance fixture %q declares no operation the host must serve", fixture)
	}
	if refusals == 0 {
		return toolboxConformanceFixture{}, fmt.Errorf("toolbox conformance fixture %q declares no operation the host must refuse", fixture)
	}
	return declared, nil
}
