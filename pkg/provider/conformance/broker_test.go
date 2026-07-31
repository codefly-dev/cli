package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	providerv0 "github.com/codefly-dev/core/generated/go/codefly/services/provider/v0"
	"github.com/codefly-dev/core/network/urlguard"
	"github.com/codefly-dev/core/provider/broker"
	"github.com/codefly-dev/core/provider/canonical"
	"github.com/codefly-dev/core/provider/cassette"
	"github.com/codefly-dev/core/provider/credentials"
	"github.com/codefly-dev/core/provider/manifest"
	"github.com/codefly-dev/core/provider/responsepolicy"
	"github.com/stretchr/testify/require"
)

// The bearer secret the host injects into the upstream request. It is distinct
// from the response secret (PoisonSecret) so a leak of either is attributable.
const poisonCredential = "codefly-poison-credential-DO-NOT-LEAK"

const brokerRemoteID = "acct_0001"

// brokerHarness wires the real F2 broker against the reference fixture. Every
// field is host-owned; a provider never sees it.
type brokerHarness struct {
	fixture  *Fixture
	manifest *manifest.Manifest
	vault    *credentials.Vault
	sink     *recordingSink
	admitted *providerv0.AdmittedOrigin
}

func newBrokerHarness(t *testing.T) *brokerHarness {
	t.Helper()
	f := NewFixture()
	t.Cleanup(f.Close)

	raw, err := os.ReadFile(filepath.Join("testdata", "provider.codefly.yaml"))
	require.NoError(t, err)
	m, err := manifest.Load(raw)
	require.NoError(t, err)

	admitted := &providerv0.AdmittedOrigin{
		OriginRuleId:        "api",
		Scheme:              "http",
		Host:                "localhost",
		Port:                8080,
		PrivateNetworkClass: providerv0.PrivateNetworkClass_PRIVATE_NETWORK_CLASS_LOOPBACK,
	}
	digest, err := canonical.AdmittedOriginDigest(admitted)
	require.NoError(t, err)
	admitted.AdmissionDigest = digest

	return &brokerHarness{fixture: f, manifest: m, vault: credentials.NewVault(), sink: &recordingSink{}, admitted: admitted}
}

func (h *brokerHarness) origin() urlguard.Origin {
	return urlguard.Origin{Scheme: "http", Host: "localhost", Port: 8080}
}

func (h *brokerHarness) session(t *testing.T, action *providerv0.PlanAction, readOnly bool, opts ...func(*broker.Config)) *broker.Session {
	t.Helper()
	cfg := broker.Config{
		Manifest:    h.manifest,
		Action:      action,
		Binding:     bindingAddress(),
		Budget:      budget(),
		ReadOnly:    readOnly,
		Vault:       h.vault,
		Sink:        h.sink,
		Checkpoints: &fakeCheckpointer{cp: checkpoint("cp1", "idem-1")},
		Deadlines:   urlguard.DefaultDeadlines(),
		ClientFor:   dialClientFor(h.fixture.Addr()),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	session, err := broker.New(cfg)
	require.NoError(t, err)
	return session
}

func (h *brokerHarness) mint(t *testing.T, planned *providerv0.PlannedRequest, method providerv0.HTTPMethod) *providerv0.CredentialHandle {
	t.Helper()
	handle, err := h.vault.Mint(poisonCredential, credentials.Scope{
		Principal:      "user",
		Organization:   "org",
		ArtifactDigest: "sha256:aa",
		Binding:        bindingAddress(),
		PlanID:         "plan1",
		ActionID:       "a1",
		RequestDigest:  planned.GetRequestDigest(),
		Purpose:        providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT,
		Origin:         h.origin(),
		Method:         method,
		Injection:      credentials.Injection{Kind: credentials.InjectBearer},
		MaxUses:        1,
		TTL:            time.Minute,
	})
	require.NoError(t, err)
	return handle
}

func (h *brokerHarness) execute(handle *providerv0.CredentialHandle, planned *providerv0.PlannedRequest) *providerv0.ExecuteRequestRequest {
	return &providerv0.ExecuteRequestRequest{
		Context: &providerv0.ProviderContext{
			Offline:     &providerv0.OfflineProviderContext{Binding: bindingAddress()},
			Credentials: []*providerv0.CredentialHandle{handle},
			Operation:   operation(),
			Budget:      budget(),
		},
		RequestId:         "req-1",
		Request:           planned,
		Origin:            h.admitted,
		CredentialHandles: []*providerv0.CredentialHandle{handle},
	}
}

// TestBrokerCreateCapturesSecretAndForwardsSafe is the central invariant: a
// mutating request through the real broker forwards only the manifest-declared
// safe fields, captures the response secret to the sink, and never lets a
// poison value reach the provider-facing response.
func TestBrokerCreateCapturesSecretAndForwardsSafe(t *testing.T) {
	h := newBrokerHarness(t)
	create := h.createRequest(t)
	session := h.session(t, createAction(t, create), false)

	handle := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	response, err := session.Execute(context.Background(), h.execute(handle, create))
	require.NoError(t, err)

	require.Equal(t, providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED, response.GetDelivery())
	require.Equal(t, uint32(http.StatusOK), response.GetStatusCode())

	// The response secret was captured, not forwarded; metadata.internal is
	// suppressed-with-presence; id and metadata.public are the only safe fields.
	require.Equal(t, []string{PoisonSecret}, h.sink.stored)
	require.Len(t, response.GetCaptures(), 1)
	require.Len(t, response.GetSuppressedPresence(), 1)

	forwarded := forwardedBySelector(response)
	require.Equal(t, map[string]string{
		"$.id":              brokerRemoteID,
		"$.metadata.public": "safe-adjacent",
	}, forwarded)

	// Neither the response secret nor the injected credential is anywhere in the
	// provider-facing response.
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assertNoPoison(t, encoded)
	require.NotContains(t, string(encoded), poisonCredential)

	// Exactly one request reached the fixture.
	require.Equal(t, 1, h.fixture.RequestCount())
}

// TestBrokerReadOnlyContextRejectsMutationWithoutEffect proves a denial upstream
// of the network leaves no side effect: no request reaches the fixture.
func TestBrokerReadOnlyContextRejectsMutationWithoutEffect(t *testing.T) {
	h := newBrokerHarness(t)
	create := h.createRequest(t)
	session := h.session(t, createAction(t, create), true)

	handle := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	_, err := session.Execute(context.Background(), h.execute(handle, create))
	require.ErrorContains(t, err, "read-only")

	require.Empty(t, h.sink.stored)
	require.Equal(t, 0, h.fixture.RequestCount(), "a denied request must not reach the network")
}

// TestBrokerBudgetExhaustionStopsBeforeNetwork proves the request-count budget
// gates the second call before any bytes leave the host.
func TestBrokerBudgetExhaustionStopsBeforeNetwork(t *testing.T) {
	h := newBrokerHarness(t)
	create := h.createRequest(t)
	session := h.session(t, createAction(t, create), false, func(cfg *broker.Config) {
		cfg.Budget = &providerv0.RequestBudget{RequestCount: 1, RequestBytes: 8192, ResponseBytes: 65536}
	})

	handle := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	_, err := session.Execute(context.Background(), h.execute(handle, create))
	require.NoError(t, err)
	require.Equal(t, 1, h.fixture.RequestCount())

	// The vault handle was single-use; the budget is what we assert on, so a fresh
	// handle isolates the budget check from credential exhaustion.
	handle2 := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	_, err = session.Execute(context.Background(), h.execute(handle2, create))
	require.ErrorContains(t, err, "budget")
	require.Equal(t, 1, h.fixture.RequestCount(), "the over-budget request never reaches the network")
}

// TestBrokerCassetteReplayServesWithoutNetwork proves a recorded session replays
// the identical filtered response with no network I/O and no poison in the
// serialized cassette.
func TestBrokerCassetteReplayServesWithoutNetwork(t *testing.T) {
	h := newBrokerHarness(t)
	create := h.createRequest(t)
	action := createAction(t, create)

	recorder := cassette.New(cassette.ModeRecord, "1.0.0")
	recordSession := h.session(t, action, false, func(cfg *broker.Config) { cfg.Cassette = recorder })
	handle := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	live, err := recordSession.Execute(context.Background(), h.execute(handle, create))
	require.NoError(t, err)

	cassetteBytes, err := recorder.Marshal()
	require.NoError(t, err)
	assertNoPoison(t, cassetteBytes)

	// Replay against a dialer that fails if touched, proving no network occurs.
	replayer, err := cassette.Load(cassetteBytes, "1.0.0")
	require.NoError(t, err)
	replaySession := h.session(t, action, false, func(cfg *broker.Config) {
		cfg.Cassette = replayer
		cfg.ClientFor = failingClientFor(t)
	})
	handle2 := h.mint(t, create, providerv0.HTTPMethod_HTTP_METHOD_POST)
	replayed, err := replaySession.Execute(context.Background(), h.execute(handle2, create))
	require.NoError(t, err)

	require.Equal(t, forwardedBySelector(live), forwardedBySelector(replayed))
	encoded, err := json.Marshal(replayed)
	require.NoError(t, err)
	assertNoPoison(t, encoded)
}

// TestBrokerObserveCapturesReadPathSecret proves a secret in a read (GET)
// response is captured to the sink and never forwarded, exercising the fixture's
// retrieve path end to end through the real broker.
func TestBrokerObserveCapturesReadPathSecret(t *testing.T) {
	h := newBrokerHarness(t)
	seeded := h.fixture.Seed("codefly")
	require.Equal(t, brokerRemoteID, seeded)

	observe := h.observeRequest(t, seeded)
	// The observe request is anchored by a create-typed action whose prospective
	// id matches the retrieved resource, mirroring the core broker test recipe.
	session := h.session(t, createAction(t, observe), false)

	handle := h.mint(t, observe, providerv0.HTTPMethod_HTTP_METHOD_GET)
	response, err := session.Execute(context.Background(), h.execute(handle, observe))
	require.NoError(t, err)

	require.Equal(t, providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED, response.GetDelivery())
	require.Equal(t, uint32(http.StatusOK), response.GetStatusCode())

	require.Equal(t, []string{PoisonSecret}, h.sink.stored)
	require.Equal(t, map[string]string{
		"$.id":              seeded,
		"$.metadata.public": "safe-adjacent",
	}, forwardedBySelector(response))

	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	assertNoPoison(t, encoded)
	require.NotContains(t, string(encoded), poisonCredential)
	require.Equal(t, 1, h.fixture.RequestCount())
}

// TestBrokerDeleteRemovesOwnedResource proves a delete of an owned resource
// reaches the fixture, forwards only the safe deletion marker, and removes the
// resource.
func TestBrokerDeleteRemovesOwnedResource(t *testing.T) {
	h := newBrokerHarness(t)
	seeded := h.fixture.Seed("codefly")
	require.Equal(t, 1, h.fixture.ResourceCount())

	del := h.deleteRequest(t, seeded)
	session := h.session(t, deleteAction(t, seeded, del), false)

	handle := h.mint(t, del, providerv0.HTTPMethod_HTTP_METHOD_DELETE)
	response, err := session.Execute(context.Background(), h.execute(handle, del))
	require.NoError(t, err)

	require.Equal(t, providerv0.DeliveryState_DELIVERY_STATE_RESPONSE_RECEIVED, response.GetDelivery())
	require.Equal(t, uint32(http.StatusOK), response.GetStatusCode())

	require.Len(t, response.GetForwarded(), 1)
	require.Equal(t, "$.deleted", response.GetForwarded()[0].GetSelector())
	require.True(t, response.GetForwarded()[0].GetValue().GetBoolValue())

	require.Equal(t, 0, h.fixture.ResourceCount(), "the owned resource was destroyed")
	require.Equal(t, 1, h.fixture.RequestCount())
}

func (h *brokerHarness) createRequest(t *testing.T) *providerv0.PlannedRequest {
	t.Helper()
	request := &providerv0.PlannedRequest{
		RequestDescriptorId:     "account.create",
		RequestDescriptorDigest: descriptorDigest(t, h.manifest, "account.create"),
		Method:                  providerv0.HTTPMethod_HTTP_METHOD_POST,
		AdmittedOriginDigest:    h.admitted.GetAdmissionDigest(),
		Body:                    map[string]*providerv0.PublicValue{"name": pubString(brokerRemoteID)},
		CredentialPurposes:      []providerv0.CredentialPurpose{providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT},
		ResponsePolicyDigest:    fakeDigest("account-response-policy"),
		IdempotencyKey:          "idem-1",
	}
	bound, err := canonical.BindPlannedRequestDigest(request)
	require.NoError(t, err)
	return bound
}

func createAction(t *testing.T, requests ...*providerv0.PlannedRequest) *providerv0.PlanAction {
	t.Helper()
	action := &providerv0.PlanAction{
		ActionId:            "a1",
		Position:            0,
		Type:                providerv0.ActionType_ACTION_TYPE_CREATE,
		ResourceType:        "account",
		ProspectiveRemoteId: brokerRemoteID,
		Ownership:           providerv0.Ownership_OWNERSHIP_OWNED,
		Requests:            requests,
	}
	require.NoError(t, canonical.ValidatePlanAction(action))
	return action
}

func (h *brokerHarness) observeRequest(t *testing.T, accountID string) *providerv0.PlannedRequest {
	t.Helper()
	request := &providerv0.PlannedRequest{
		RequestDescriptorId:     "account.observe",
		RequestDescriptorDigest: descriptorDigest(t, h.manifest, "account.observe"),
		Method:                  providerv0.HTTPMethod_HTTP_METHOD_GET,
		AdmittedOriginDigest:    h.admitted.GetAdmissionDigest(),
		PathParameters:          map[string]*providerv0.PublicValue{"account_id": pubString(accountID)},
		CredentialPurposes:      []providerv0.CredentialPurpose{providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT},
		ResponsePolicyDigest:    fakeDigest("account-response-policy"),
	}
	bound, err := canonical.BindPlannedRequestDigest(request)
	require.NoError(t, err)
	return bound
}

func (h *brokerHarness) deleteRequest(t *testing.T, accountID string) *providerv0.PlannedRequest {
	t.Helper()
	request := &providerv0.PlannedRequest{
		RequestDescriptorId:     "account.delete",
		RequestDescriptorDigest: descriptorDigest(t, h.manifest, "account.delete"),
		Method:                  providerv0.HTTPMethod_HTTP_METHOD_DELETE,
		AdmittedOriginDigest:    h.admitted.GetAdmissionDigest(),
		PathParameters:          map[string]*providerv0.PublicValue{"account_id": pubString(accountID)},
		CredentialPurposes:      []providerv0.CredentialPurpose{providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_MANAGEMENT},
		ResponsePolicyDigest:    fakeDigest("deleted-response-policy"),
		IdempotencyKey:          "idem-1",
	}
	bound, err := canonical.BindPlannedRequestDigest(request)
	require.NoError(t, err)
	return bound
}

func deleteAction(t *testing.T, remoteID string, requests ...*providerv0.PlannedRequest) *providerv0.PlanAction {
	t.Helper()
	action := &providerv0.PlanAction{
		ActionId:       "a1",
		Position:       0,
		Type:           providerv0.ActionType_ACTION_TYPE_DELETE,
		ResourceType:   "account",
		RemoteIdentity: &providerv0.RemoteIdentity{Provider: "conformance", ResourceType: "account", RemoteId: remoteID},
		Ownership:      providerv0.Ownership_OWNERSHIP_OWNED,
		Requests:       requests,
	}
	require.NoError(t, canonical.ValidatePlanAction(action))
	return action
}

// --- read-path secret filtering, exercised through the real response policy ---

// TestResponsePolicyCapturesReadArraySecrets proves the host response filter
// handles secrets nested inside a read (list) array: every secret is captured
// and none reaches the safe projection.
func TestResponsePolicyCapturesReadArraySecrets(t *testing.T) {
	body := `{"object":"list","data":[` +
		`{"id":"acct_0001","secret":"` + PoisonSecret + `","public":"safe-adjacent"},` +
		`{"id":"acct_0002","secret":"` + PoisonSecret + `","public":"safe-adjacent"}` +
		`]}`
	sink := &recordingSink{}
	policy := responsepolicy.Policy{
		Fields: []responsepolicy.Field{
			{Selector: manifest.Selector{Version: "v1", Path: "$.data[*].id"}, Disposition: manifest.ResponseForwardSafe},
			{Selector: manifest.Selector{Version: "v1", Path: "$.data[*].public"}, Disposition: manifest.ResponseForwardSafe},
			{Selector: manifest.Selector{Version: "v1", Path: "$.data[*].secret"}, Disposition: manifest.ResponseCaptureToSink, Purpose: providerv0.CredentialPurpose_CREDENTIAL_PURPOSE_RUNTIME, SinkKey: "account-secret"},
		},
		Limits: responsepolicy.DefaultLimits(),
	}

	result, err := policy.Filter(context.Background(), []byte(body), "", "application/json", sink)
	require.NoError(t, err)

	require.Len(t, result.Captures, 2)
	require.Equal(t, []string{PoisonSecret, PoisonSecret}, sink.stored)
	assertNoPoison(t, result.SafeJSON)
	require.Len(t, result.Forwarded, 4) // id + public, per element
}

// --- shared host-side wiring, mirroring the core broker test recipe ---

func pubString(value string) *providerv0.PublicValue {
	return &providerv0.PublicValue{Kind: &providerv0.PublicValue_StringValue{StringValue: value}}
}

func fakeDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func descriptorDigest(t *testing.T, m *manifest.Manifest, id string) string {
	t.Helper()
	for _, descriptor := range m.Requests {
		if descriptor.ID == id {
			digest, err := manifest.RequestDescriptorDigest(descriptor)
			require.NoError(t, err)
			return digest
		}
	}
	t.Fatalf("descriptor %q not found", id)
	return ""
}

func bindingAddress() *providerv0.BindingAddress {
	return &providerv0.BindingAddress{WorkspaceId: "ws", EnvironmentId: "env", BindingId: "bind"}
}

func operation() *providerv0.OperationIdentity {
	return &providerv0.OperationIdentity{OperationId: "op1", AttemptId: "att1", ActionId: "a1", PlanId: "plan1"}
}

func budget() *providerv0.RequestBudget {
	return &providerv0.RequestBudget{RequestCount: 4, RequestBytes: 8192, ResponseBytes: 65536}
}

func checkpoint(id, idempotencyKey string) *providerv0.ActionCheckpoint {
	return &providerv0.ActionCheckpoint{
		CheckpointId:   id,
		Operation:      operation(),
		Delivery:       providerv0.DeliveryState_DELIVERY_STATE_NOT_SENT,
		IdempotencyKey: idempotencyKey,
	}
}

type fakeCheckpointer struct {
	cp *providerv0.ActionCheckpoint
}

func (c *fakeCheckpointer) Latest(context.Context, *providerv0.OperationIdentity) (*providerv0.ActionCheckpoint, error) {
	return c.cp, nil
}

type recordingSink struct {
	stored []string
}

func (s *recordingSink) Put(_ context.Context, target responsepolicy.SinkTarget, secret string) (*providerv0.OpaqueReference, error) {
	s.stored = append(s.stored, secret)
	return &providerv0.OpaqueReference{Reference: "capture://" + target.Key, Purpose: target.Purpose}, nil
}

// dialClientFor dials the fixture regardless of the request URL, exercising the
// real transport path without depending on DNS or the manifest port.
func dialClientFor(addr string) func(urlguard.Origin, urlguard.Resolution) *http.Client {
	return func(urlguard.Origin, urlguard.Resolution) *http.Client {
		return &http.Client{
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
}

// failingClientFor returns a client whose dialer fails, proving replay performs
// no network I/O.
func failingClientFor(t *testing.T) func(urlguard.Origin, urlguard.Resolution) *http.Client {
	return func(urlguard.Origin, urlguard.Resolution) *http.Client {
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					t.Error("replay dialed the network")
					return nil, net.ErrClosed
				},
			},
		}
	}
}

func forwardedBySelector(response *providerv0.ExecuteRequestResponse) map[string]string {
	out := make(map[string]string, len(response.GetForwarded()))
	for _, field := range response.GetForwarded() {
		out[field.GetSelector()] = field.GetValue().GetStringValue()
	}
	return out
}
