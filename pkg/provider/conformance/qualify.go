package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/network/urlguard"
	"github.com/codefly-dev/core/provider/broker"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/credentials"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/responsepolicy"
	"google.golang.org/protobuf/proto"
)

// qualificationCredential is the host-owned secret the harness mints for every
// probe. It is a planted poison value, so a credential that ever escapes the
// host boundary into evidence is attributable to this harness.
const qualificationCredential = "codefly-poison-qualification-credential-DO-NOT-LEAK" //nolint:gosec // G101: deliberately fake planted poison, never a real credential

// Operation is one owner-declared provider operation. It names a request the
// manifest packages and the exact parameters the host would plan for it. The
// owner never declares host authority: principal, credential, budget, binding
// and plan identity are all supplied by the harness.
type Operation struct {
	Name           string            `yaml:"name"`
	Request        string            `yaml:"request"`
	PathParameters map[string]string `yaml:"path_parameters,omitempty"`
	Query          map[string]string `yaml:"query,omitempty"`
	Body           map[string]string `yaml:"body,omitempty"`
}

// Declaration is the owner-declared provider conformance suite.
type Declaration struct {
	Operations []Operation `yaml:"operations"`
}

// Evidence is the qualification record for one provider release.
type Evidence struct {
	SchemaVersion  int                 `json:"schema_version"`
	Provider       string              `json:"provider"`
	Version        string              `json:"version"`
	ManifestDigest string              `json:"manifest_digest"`
	Operations     []OperationEvidence `json:"operations"`
}

// OperationEvidence records what the host admitted for one declared operation
// and which refusals proved its negative paths.
type OperationEvidence struct {
	Name             string `json:"name"`
	Request          string `json:"request"`
	Method           string `json:"method"`
	ReadOnly         bool   `json:"read_only"`
	DescriptorDigest string `json:"descriptor_digest"`
	RequestDigest    string `json:"request_digest"`
	NegativePaths    string `json:"negative_paths"`
}

// EvidenceVersion is the compatibility version of the emitted Evidence.
const EvidenceVersion = 1

// Qualify runs every owner-declared operation through the real host broker and
// records what the host admitted.
//
// Each probe runs against an exhausted request budget. The broker checks the
// budget only after it has validated the plan action, bound the planned-request
// digest, confirmed the descriptor is packaged with a matching descriptor
// digest, matched the declared credential purposes against the minted handles,
// and applied the read-only rule — so a budget refusal is proof that everything
// upstream of the network admitted the request, obtained without contacting the
// provider's upstream API.
func Qualify(ctx context.Context, providerManifest *manifest.Manifest, declared Declaration) (*Evidence, error) {
	if err := assertOriginRulesAdmitTheirDefaults(providerManifest); err != nil {
		return nil, err
	}
	digest, err := canonical.ManifestDigest(providerManifest)
	if err != nil {
		return nil, fmt.Errorf("provider conformance: %w", err)
	}
	evidence := &Evidence{
		SchemaVersion:  EvidenceVersion,
		Provider:       providerManifest.Agent.Name,
		Version:        providerManifest.Agent.Version,
		ManifestDigest: digest,
	}
	for _, operation := range declared.Operations {
		record, err := qualifyOperation(ctx, providerManifest, operation)
		if err != nil {
			return nil, err
		}
		evidence.Operations = append(evidence.Operations, *record)
	}
	return evidence, nil
}

func qualifyOperation(ctx context.Context, providerManifest *manifest.Manifest, operation Operation) (*OperationEvidence, error) {
	probe, err := newOperationProbe(providerManifest, operation)
	if err != nil {
		return nil, err
	}
	if admitErr := probe.assertAdmitted(ctx); admitErr != nil {
		return nil, admitErr
	}
	refusal, err := probe.assertNegativePaths(ctx)
	if err != nil {
		return nil, err
	}
	return &OperationEvidence{
		Name:             operation.Name,
		Request:          probe.descriptor.ID,
		Method:           probe.descriptor.Method,
		ReadOnly:         probe.descriptor.ReadOnly,
		DescriptorDigest: probe.descriptorDigest,
		RequestDigest:    probe.planned.GetRequestDigest(),
		NegativePaths:    refusal,
	}, nil
}

// operationProbe holds one fully composed host request for a declared
// operation, plus the host-owned material the broker binds it to.
type operationProbe struct {
	providerManifest *manifest.Manifest
	operation        Operation
	descriptor       *manifest.RequestDescriptor
	descriptorDigest string
	origin           *providerv0.AdmittedOrigin
	planned          *providerv0.PlannedRequest
	vault            *credentials.Vault
	urlOrigin        urlguard.Origin
}

func newOperationProbe(providerManifest *manifest.Manifest, operation Operation) (*operationProbe, error) {
	if strings.TrimSpace(operation.Name) == "" {
		return nil, fmt.Errorf("provider conformance: every operation requires a name")
	}
	descriptor, err := packagedDescriptor(providerManifest, operation)
	if err != nil {
		return nil, err
	}
	descriptorDigest, err := manifest.RequestDescriptorDigest(*descriptor)
	if err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	rule, err := packagedOriginRule(providerManifest, descriptor)
	if err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	urlOrigin, admitted, err := admittedOrigin(rule)
	if err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	method, err := httpMethod(descriptor.Method)
	if err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	purposes, err := credentialPurposes(descriptor)
	if err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	planned := &providerv0.PlannedRequest{
		RequestDescriptorId:     descriptor.ID,
		RequestDescriptorDigest: descriptorDigest,
		Method:                  method,
		AdmittedOriginDigest:    admitted.GetAdmissionDigest(),
		PathParameters:          publicValues(operation.PathParameters),
		Query:                   publicValues(operation.Query),
		Body:                    publicValues(operation.Body),
		CredentialPurposes:      purposes,
		ResponsePolicyDigest:    responsePolicyDigest(descriptor),
		IdempotencyKey:          qualificationIdempotencyKey,
	}
	bound, err := canonical.BindPlannedRequestDigest(planned)
	if err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	if err := canonical.ValidatePlanAction(planAction(descriptor, bound)); err != nil {
		return nil, fmt.Errorf("provider conformance operation %q: %w", operation.Name, err)
	}
	return &operationProbe{
		providerManifest: providerManifest,
		operation:        operation,
		descriptor:       descriptor,
		descriptorDigest: descriptorDigest,
		origin:           admitted,
		planned:          bound,
		vault:            credentials.NewVault(),
		urlOrigin:        urlOrigin,
	}, nil
}

const (
	qualificationTenant         = "agent-ci"
	qualificationActionID       = "provider-conformance"
	qualificationRemoteID       = "provider-conformance-subject"
	qualificationIdempotencyKey = "provider-conformance-idempotency"
	qualificationCheckpointID   = "provider-conformance-checkpoint"
	qualificationPlanID         = "provider-conformance-plan"
)

// assertAdmitted proves the host admits this exact request up to the point
// where bytes would leave, and no further.
func (p *operationProbe) assertAdmitted(ctx context.Context) error {
	_, err := p.execute(ctx, p.planned, false)
	if err == nil {
		return fmt.Errorf("provider conformance operation %q: an exhausted budget still delivered a request", p.operation.Name)
	}
	if !strings.Contains(err.Error(), "budget") {
		return fmt.Errorf("provider conformance operation %q: host refused before the budget: %w", p.operation.Name, err)
	}
	return nil
}

// assertNegativePaths proves the host refuses what it must. Every operation is
// probed with a tampered descriptor digest; a mutating one is additionally
// probed in a read-only context, which must be refused before the budget.
func (p *operationProbe) assertNegativePaths(ctx context.Context) (string, error) {
	tampered, err := tamperedDescriptorDigest(p.planned)
	if err != nil {
		return "", fmt.Errorf("provider conformance operation %q: %w", p.operation.Name, err)
	}
	if _, err := p.execute(ctx, tampered, false); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		return "", fmt.Errorf("provider conformance operation %q: a tampered descriptor digest was not refused: %v", p.operation.Name, err)
	}
	if p.descriptor.ReadOnly {
		if _, err := p.execute(ctx, p.planned, true); err == nil || !strings.Contains(err.Error(), "budget") {
			return "", fmt.Errorf("provider conformance operation %q: a read-only context refused a read-only request: %v", p.operation.Name, err)
		}
		return "descriptor-digest", nil
	}
	if _, err := p.execute(ctx, p.planned, true); err == nil || !strings.Contains(err.Error(), "read-only") {
		return "", fmt.Errorf("provider conformance operation %q: a read-only context admitted a mutating request: %v", p.operation.Name, err)
	}
	return "descriptor-digest,read-only", nil
}

// execute composes the complete host-side call and runs it through the real
// broker. The session may deliver no request at all: qualification reads the
// admission decisions the broker reaches before the budget, and a provider's
// own upstream API is never contacted.
func (p *operationProbe) execute(ctx context.Context, planned *providerv0.PlannedRequest, readOnly bool) (*providerv0.ExecuteRequestResponse, error) {
	budget := &providerv0.RequestBudget{
		RequestCount:  0,
		RequestBytes:  p.descriptor.RequestByteBudget,
		ResponseBytes: p.descriptor.ResponseByteBudget,
	}
	action := planAction(p.descriptor, planned)
	session, err := broker.New(broker.Config{
		Manifest:    p.providerManifest,
		Action:      action,
		Binding:     qualificationBinding(),
		Budget:      budget,
		ReadOnly:    readOnly,
		Vault:       p.vault,
		Sink:        &discardingSink{},
		Checkpoints: qualificationCheckpoint{},
	})
	if err != nil {
		return nil, err
	}
	handles := make([]*providerv0.CredentialHandle, 0, len(planned.GetCredentialPurposes()))
	for _, purpose := range planned.GetCredentialPurposes() {
		handle, err := p.vault.Mint(qualificationCredential, credentials.Scope{
			Principal:      qualificationTenant,
			Organization:   qualificationTenant,
			ArtifactDigest: p.descriptorDigest,
			Binding:        qualificationBinding(),
			PlanID:         qualificationPlanID,
			ActionID:       qualificationActionID,
			RequestDigest:  planned.GetRequestDigest(),
			Purpose:        purpose,
			Origin:         p.urlOrigin,
			Method:         planned.GetMethod(),
			Injection:      credentials.Injection{Kind: credentials.InjectBearer},
			MaxUses:        1,
			TTL:            time.Minute,
		})
		if err != nil {
			return nil, err
		}
		handles = append(handles, handle)
	}
	return session.Execute(ctx, &providerv0.ExecuteRequestRequest{
		Context: &providerv0.ProviderContext{
			Offline:     &providerv0.OfflineProviderContext{Binding: qualificationBinding()},
			Credentials: handles,
			Operation: &providerv0.OperationIdentity{
				OperationId: qualificationActionID, AttemptId: "1",
				ActionId: qualificationActionID, PlanId: qualificationPlanID,
			},
			Budget: budget,
		},
		RequestId:         p.operation.Name,
		Request:           planned,
		Origin:            p.origin,
		CredentialHandles: handles,
	})
}

// assertOriginRulesAdmitTheirDefaults rejects a manifest whose own declared
// default origin falls outside the ceiling that same rule imposes. The broker
// would refuse every request through such a rule at runtime.
func assertOriginRulesAdmitTheirDefaults(providerManifest *manifest.Manifest) error {
	for index := range providerManifest.OriginRules {
		rule := &providerManifest.OriginRules[index]
		ceiling := ceilingFor(rule)
		for _, raw := range rule.Defaults {
			if _, err := ceiling.Admit(raw); err != nil {
				return fmt.Errorf("provider conformance: origin rule %q does not admit its own default %q: %w", rule.ID, raw, err)
			}
		}
	}
	return nil
}

// planAction is the single-action plan the harness admits the request under.
// The broker binds a request to its action by digest, so each probe carries the
// exact request it executes.
func planAction(descriptor *manifest.RequestDescriptor, planned *providerv0.PlannedRequest) *providerv0.PlanAction {
	return &providerv0.PlanAction{
		ActionId:            qualificationActionID,
		Type:                providerv0.ActionType_ACTION_TYPE_CREATE,
		ResourceType:        descriptor.ResourceType,
		ProspectiveRemoteId: qualificationRemoteID,
		Ownership:           providerv0.Ownership_OWNERSHIP_OWNED,
		Requests:            []*providerv0.PlannedRequest{planned},
	}
}

func packagedDescriptor(providerManifest *manifest.Manifest, operation Operation) (*manifest.RequestDescriptor, error) {
	packaged := make([]string, 0, len(providerManifest.Requests))
	for index := range providerManifest.Requests {
		if providerManifest.Requests[index].ID == operation.Request {
			return &providerManifest.Requests[index], nil
		}
		packaged = append(packaged, providerManifest.Requests[index].ID)
	}
	sort.Strings(packaged)
	return nil, fmt.Errorf(
		"provider conformance operation %q names request %q, which the manifest does not package (packaged: %s)",
		operation.Name, operation.Request, strings.Join(packaged, ", "))
}

func packagedOriginRule(providerManifest *manifest.Manifest, descriptor *manifest.RequestDescriptor) (*manifest.OriginRule, error) {
	for index := range providerManifest.OriginRules {
		if providerManifest.OriginRules[index].ID == descriptor.OriginRule {
			return &providerManifest.OriginRules[index], nil
		}
	}
	return nil, fmt.Errorf("origin rule %q is not packaged", descriptor.OriginRule)
}

// admittedOrigin attests the rule's own first default, which is the origin the
// host binds when the operator declares no override.
func admittedOrigin(rule *manifest.OriginRule) (urlguard.Origin, *providerv0.AdmittedOrigin, error) {
	if len(rule.Defaults) == 0 {
		return urlguard.Origin{}, nil, fmt.Errorf("origin rule %q declares no default origin", rule.ID)
	}
	origin, err := ceilingFor(rule).Admit(rule.Defaults[0])
	if err != nil {
		return urlguard.Origin{}, nil, err
	}
	admitted := &providerv0.AdmittedOrigin{
		OriginRuleId:        rule.ID,
		Scheme:              origin.Scheme,
		Host:                origin.Host,
		Port:                origin.Port,
		PrivateNetworkClass: privateNetworkClass(rule),
	}
	digest, err := canonical.AdmittedOriginDigest(admitted)
	if err != nil {
		return urlguard.Origin{}, nil, err
	}
	admitted.AdmissionDigest = digest
	return origin, admitted, nil
}

// ceilingFor mirrors the broker's own rule-to-ceiling derivation so the harness
// attests exactly the origin the broker will re-admit.
func ceilingFor(rule *manifest.OriginRule) urlguard.Ceiling {
	classes := make([]urlguard.NetworkClass, 0, len(rule.PrivateNetworkClasses))
	for _, token := range rule.PrivateNetworkClasses {
		switch token {
		case "loopback":
			classes = append(classes, urlguard.ClassLoopback)
		case "link-local":
			classes = append(classes, urlguard.ClassLinkLocal)
		case "private":
			classes = append(classes, urlguard.ClassPrivate)
		}
	}
	return urlguard.Ceiling{
		Schemes:        append([]string(nil), rule.Schemes...),
		HostPatterns:   append([]string(nil), rule.HostPatterns...),
		Ports:          append([]uint32(nil), rule.Ports...),
		AllowedClasses: classes,
	}
}

func privateNetworkClass(rule *manifest.OriginRule) providerv0.PrivateNetworkClass {
	if len(rule.PrivateNetworkClasses) == 0 {
		return providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_PUBLIC
	}
	switch rule.PrivateNetworkClasses[0] {
	case "loopback":
		return providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_LOOPBACK
	case "link-local":
		return providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_LINK_LOCAL
	case "private":
		return providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_PRIVATE
	default:
		return providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_PUBLIC
	}
}

func httpMethod(method string) (providerv0.HTTPMethod, error) {
	value, ok := providerv0.HTTPMethod_value["HTTP_METHOD_"+strings.ToUpper(strings.TrimSpace(method))]
	if !ok || value == int32(providerv0.HTTPMethod_HTTP_METHOD_UNSPECIFIED) {
		return providerv0.HTTPMethod_HTTP_METHOD_UNSPECIFIED, fmt.Errorf("request method %q is not an admitted HTTP method", method)
	}
	return providerv0.HTTPMethod(value), nil
}

func credentialPurposes(descriptor *manifest.RequestDescriptor) ([]providerv0.CredentialPurpose, error) {
	purposes := make([]providerv0.CredentialPurpose, 0, len(descriptor.CredentialPurposes))
	for _, purpose := range descriptor.CredentialPurposes {
		name := "CREDENTIAL_PURPOSE_" + strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(purpose), "-", "_"))
		value, ok := providerv0.CredentialPurpose_value[name]
		if !ok || value == int32(providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_UNSPECIFIED) {
			return nil, fmt.Errorf("credential purpose %q is not an admitted purpose", purpose)
		}
		purposes = append(purposes, providerv0.CredentialPurpose(value))
	}
	return purposes, nil
}

func publicValues(declared map[string]string) map[string]*providerv0.PublicValue {
	if len(declared) == 0 {
		return nil
	}
	values := make(map[string]*providerv0.PublicValue, len(declared))
	for name, value := range declared {
		values[name] = &providerv0.PublicValue{Kind: &providerv0.PublicValue_StringValue{StringValue: value}}
	}
	return values
}

// responsePolicyDigest binds the planned request to the response schema the
// descriptor selects, the same way a planning host does.
func responsePolicyDigest(descriptor *manifest.RequestDescriptor) string {
	return digestOf("response-policy:" + descriptor.ResponseSchema)
}

// tamperedDescriptorDigest returns the same request carrying a descriptor
// digest that no packaged descriptor can produce.
func tamperedDescriptorDigest(planned *providerv0.PlannedRequest) (*providerv0.PlannedRequest, error) {
	tampered, ok := proto.Clone(planned).(*providerv0.PlannedRequest)
	if !ok {
		return nil, fmt.Errorf("clone planned request")
	}
	tampered.RequestDescriptorDigest = digestOf("tampered:" + planned.GetRequestDescriptorDigest())
	tampered.RequestDigest = ""
	return canonical.BindPlannedRequestDigest(tampered)
}

// digestOf renders a stable sha256 digest in the wire format the provider
// protocol uses for every digest field.
func digestOf(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type qualificationCheckpoint struct{}

func (qualificationCheckpoint) Latest(context.Context, *providerv0.OperationIdentity) (*providerv0.ActionCheckpoint, error) {
	return &providerv0.ActionCheckpoint{
		CheckpointId: qualificationCheckpointID,
		Operation: &providerv0.OperationIdentity{
			OperationId: qualificationActionID, AttemptId: "1",
			ActionId: qualificationActionID, PlanId: qualificationPlanID,
		},
		Delivery:       providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
		IdempotencyKey: qualificationIdempotencyKey,
	}, nil
}

// discardingSink accepts captures without retaining them. Qualification never
// reaches delivery, so nothing is ever offered to it.
type discardingSink struct{}

func (discardingSink) Put(_ context.Context, target responsepolicy.SinkTarget, _ string) (*providerv0.OpaqueReference, error) {
	return &providerv0.OpaqueReference{Reference: "capture://" + target.Key, Purpose: target.Purpose}, nil
}

func qualificationBinding() *providerv0.BindingAddress {
	return &providerv0.BindingAddress{
		WorkspaceId: qualificationTenant, EnvironmentId: "conformance", BindingId: qualificationActionID,
	}
}
