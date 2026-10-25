package gitops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	runcomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/workloadidentity"
)

// Native delivery: a laptop run delivers to the host what a cell's delivery
// Job delivers — the same module manifest, signed with the developer key,
// POSTed to the same route with a delivery credential the run issues — so the
// host's registry, mint and installation run their one code path. What
// differs is where the manifest's facts come from: its workloads are the
// identity services admitted to this run (or its started services when no
// explicit declaration is supplied), each with the account a cell would give
// it. Endpoints carry allocated addresses only for the ready subset.

// NativeIdentity is one workload a native run issues a credential for and
// lists in its module's manifest: a service that declares module-identity,
// the account it would run as in a cell, and the SPIFFE ID the cell's mesh
// issues to that account, which the manifest names.
type NativeIdentity struct {
	Module  string
	Service string
}

// Workload is the identity as the host and the run name it, under trustDomain.
func (identity NativeIdentity) Workload(trustDomain string) workloadidentity.Workload {
	return workloadidentity.Workload{TrustDomain: trustDomain, Module: identity.Module, Service: identity.Service}
}

// ResolveNativeIdentities plans the identities a native run projects, by the
// rule a cell's render names them: every module-identity service of the
// composition, each in its module's namespace. A native run has no cluster
// to require.
func ResolveNativeIdentities(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string]NativeIdentity, error) {
	if env.Host == nil {
		return nil, fmt.Errorf("environment %s declares no host", env.Name)
	}
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, err
	}
	identities := map[string]NativeIdentity{}
	for _, ref := range refs {
		service, loadErr := workspace.LoadService(ctx, ref)
		if loadErr != nil {
			return nil, loadErr
		}
		if !service.ModuleIdentity {
			continue
		}
		identities[resources.ServiceUnique(ref.Module, ref.Name)] = NativeIdentity{Module: ref.Module, Service: ref.Name}
	}
	return identities, nil
}

// NativeWorkload is one module a native run delivers a manifest for: the
// module, its services, admitted identity workloads, and ready routed endpoints.
type NativeWorkload struct {
	Module   *resources.Module
	Services []*resources.Service
	// Declared are the identity workloads admitted to this run's closure.
	// Nil retains the existing started-only delivery behavior. A non-nil set
	// permits authority admission before any workload starts.
	Declared []NativeIdentity
	// Started is the subset whose processes are ready to receive traffic.
	Started []NativeIdentity
	// Carriers are the addresses of the module's routed endpoints, keyed as
	// the gateway reads them.
	Carriers map[string]string
	// FrontendBundle is the identity of the complete frontend bundle this run
	// built and retained in the workspace's artifact cache: what the
	// manifest's frontend payload names, since what runs is what is
	// delivered. Nil when the component declares no page.
	FrontendBundle *solutionhost.FrontendPayload
	// DeferFrontend deliberately omits current frontend publication while
	// admitting workload authority. It removes any previous current page on
	// a replacing generation; it never means "keep the previous frontend".
	DeferFrontend bool
}

// NativeDeliverer renders, signs and delivers a native run's manifests.
type NativeDeliverer struct {
	Workspace *resources.Workspace
	Env       *environments.Environment
	Signer    solutionhost.Signer
	// StateDir keeps the generations reserved so far: changed content is the
	// next generation, unchanged content is replayed.
	StateDir string
	// DeliveryURL is the host's delivery listener (the accounts REST address).
	DeliveryURL string
	// Token issues the delivery account's credential in a namespace.
	Token func(module string) (string, error)
	// ObserverToken belongs to the separate read-only applied-state publisher.
	ObserverToken func() (string, error)
	Client        *http.Client

	// Post, when set, carries a signed manifest to the host instead of the HTTP
	// delivery route: a host that implements codefly.dev/composition receives it
	// through ApplyManifest. Applied, when set, answers the applied state the
	// same way. Both nil is the saas HTTP delivery, the transition's.
	Post DeliveryPost
	// Prepare captures transport-owned content before generation reservation.
	// Its Post must send exactly that captured content with the settled carrier.
	Prepare func(ctx context.Context, module string, removed bool) (*PreparedDelivery, error)
	Applied func(ctx context.Context, binding string) (*AppliedManifest, bool, error)
}

// DeliveryPost sends a carrier whose generation has already been settled.
type DeliveryPost func(context.Context, string, *solutionhost.Signed, string) (Receipt, error)

// PreparedDelivery binds generation content to its immutable send operation.
type PreparedDelivery struct {
	Content []byte
	Post    DeliveryPost
}

// Render builds the manifest of a module's admitted identity workloads,
// by the rules a cell's render follows (moduleInstanceOf,
// ModuleManifestOf), at generation 1.
func (d *NativeDeliverer) Render(ctx context.Context, workload *NativeWorkload) (*solutionhost.ModuleManifest, error) {
	declared := workload.Declared
	if declared == nil {
		declared = workload.Started
	}
	if len(declared) == 0 {
		return nil, fmt.Errorf("module %s has no admitted identity workload to deliver", workload.Module.Name)
	}
	manifest, err := composition.LoadPackageManifest(workload.Module.Dir())
	if err != nil {
		return nil, fmt.Errorf("module %s declares no package manifest, so it names no release: %w", workload.Module.Name, err)
	}
	instance, undeclared, err := moduleInstanceOf(ctx, d.Workspace, workload.Module, workload.Services, d.Env,
		&InventoryPackage{ID: manifest.ID, Version: manifest.Version}, manifest.ReleaseArtifacts, nil)
	if err != nil {
		return nil, err
	}
	if instance == nil {
		return nil, errors.New(undeclared)
	}
	// The bundle this run built outranks the release's artifact metadata: a
	// native run delivers what runs, and the host reads the bytes from the
	// cache by the digest named here. A declared page without it is refused:
	// a host admits no frontend without its signed payload.
	switch {
	case workload.DeferFrontend && workload.FrontendBundle != nil:
		return nil, fmt.Errorf("module %s cannot both defer and publish a frontend bundle", workload.Module.Name)
	case workload.DeferFrontend:
		instance.Frontend = nil
	case workload.FrontendBundle != nil && instance.Frontend == nil:
		return nil, fmt.Errorf("module %s built a frontend bundle and declares no frontend; a bundle is the output of a declared page", workload.Module.Name)
	case workload.FrontendBundle != nil:
		payload := *workload.FrontendBundle
		instance.Frontend.Payload = &payload
	case instance.Frontend != nil:
		return nil, fmt.Errorf("module %s declares a frontend and this run built no bundle for it; a native delivery names the bundle it built, and a host admits no frontend without its signed payload", workload.Module.Name)
	}
	identities := map[string]bool{}
	for _, identity := range instance.Identities {
		identities[identity.Service] = true
	}
	declaredServices := map[string]bool{}
	workloads := make([]solutionhost.Workload, 0, len(declared))
	for _, identity := range declared {
		if identity.Module != workload.Module.Name || !identities[identity.Service] {
			return nil, fmt.Errorf("module %s workload %s/%s does not declare module-identity in this module", workload.Module.Name, identity.Module, identity.Service)
		}
		if declaredServices[identity.Service] {
			return nil, fmt.Errorf("module %s declares workload %s more than once", workload.Module.Name, identity.Service)
		}
		declaredServices[identity.Service] = true
		// A native workload declares no cell: the run vouches for (module,
		// service) directly, and a manifest names only what the module is.
		workloads = append(workloads, solutionhost.Workload{Service: identity.Service, Container: identity.Service})
	}
	sort.Slice(workloads, func(i, j int) bool { return workloads[i].Service < workloads[j].Service })
	started := map[string]bool{}
	for _, identity := range workload.Started {
		if identity.Module != workload.Module.Name || !declaredServices[identity.Service] {
			return nil, fmt.Errorf("module %s routes ready workload %s/%s outside its declared membership", workload.Module.Name, identity.Module, identity.Service)
		}
		started[identity.Service] = true
	}
	// An identity workload this run has not started yet routes nothing yet:
	// its endpoints join a later generation once the workload is ready.
	if instance.Endpoints, err = routedEndpoints(workload.Module.Name, workload.Services, workload.Carriers, identities, started); err != nil {
		return nil, err
	}
	binding, err := bindingID(d.Workspace.Name, d.Env.Name, instance.Name)
	if err != nil {
		return nil, err
	}
	return ModuleManifestOf(binding, d.Env.Host, instance, workloads)
}

// nativeGenerations is the run's record of what it reserved: per binding,
// the generation and the content it carried. It outlives a run, so a restart
// that delivers the same content replays its generation.
type nativeGenerations struct {
	Entries map[string]nativeGeneration `json:"entries"`
}

type nativeGeneration struct {
	Generation uint64 `json:"generation"`
	Content    string `json:"content"`
	Removed    bool   `json:"removed,omitempty"`
}

func (d *NativeDeliverer) generationsPath() string {
	return filepath.Join(d.StateDir, "generations.json")
}

func (d *NativeDeliverer) loadGenerations() (*nativeGenerations, error) {
	record := &nativeGenerations{Entries: map[string]nativeGeneration{}}
	data, err := os.ReadFile(d.generationsPath())
	if errors.Is(err, os.ErrNotExist) {
		return record, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, record); err != nil {
		return nil, fmt.Errorf("read %s: %w", d.generationsPath(), err)
	}
	if record.Entries == nil {
		record.Entries = map[string]nativeGeneration{}
	}
	return record, nil
}

func (d *NativeDeliverer) saveGenerations(ctx context.Context, record *nativeGenerations) error {
	if err := os.MkdirAll(d.StateDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, d.generationsPath(), data, 0o600)
}

// settle assigns a manifest its generation: the one it had when its content is
// unchanged, the next one otherwise. Content includes the canonical Core payload
// at generation 1 and any prepared transport declaration; withdrawals are terminal.
func settle(record *nativeGenerations, key string, content []byte, removed bool) (uint64, error) {
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	entry, known := record.Entries[key]
	if known && entry.Content == digest {
		// Old records did not retain terminality. An exact withdrawal replay
		// can record it without advancing the immutable terminal generation.
		entry.Removed = entry.Removed || removed
		record.Entries[key] = entry
		return entry.Generation, nil
	}
	if entry.Removed {
		return 0, fmt.Errorf("%w: binding %s has a reserved withdrawal; replay it or use a new binding", solutionhost.ErrWithdrawn, key)
	}
	next := uint64(1)
	if known {
		next = entry.Generation + 1
	}
	record.Entries[key] = nativeGeneration{Generation: next, Content: digest, Removed: removed}
	return next, nil
}

// Receipt is the host's answer to one delivered manifest: 200 with the
// disposition (accepted or replayed) and the generation it holds, or a refusal
// with its reason.
type Receipt struct {
	Status      int    `json:"-"`
	Disposition string `json:"disposition"`
	Generation  uint64 `json:"generation"`
	Error       string `json:"error"`
}

// Deliver settles the manifest's generation, signs it and POSTs it to the
// host as the delivery account of namespace — the namespace of the manifest's
// workloads, or for a withdrawal the one its binding was delivered in.
func (d *NativeDeliverer) Deliver(ctx context.Context, manifest *solutionhost.ModuleManifest) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if err := os.MkdirAll(d.StateDir, 0o700); err != nil {
		return Receipt{}, err
	}
	post := d.Post
	// Source preparation and reservation share one short transaction. No
	// credential acquisition, signing or network operation holds this lock.
	err := runcomposition.WithFileLockContext(ctx, d.generationsPath()+".lock", 30*time.Second, func() error {
		record, err := d.loadGenerations()
		if err != nil {
			return err
		}
		manifest.Generation = 1
		content, err := solutionhost.SignedPayload(manifest)
		if err != nil {
			return err
		}
		if d.Prepare != nil {
			prepared, prepareErr := d.Prepare(ctx, manifest.Alias, manifest.Removed)
			if prepareErr != nil {
				return prepareErr
			}
			if prepared == nil || prepared.Post == nil {
				return errors.New("delivery preparation supplied no send operation")
			}
			post = prepared.Post
			// Withdrawals retain their old Core-only identity: a terminal
			// binding cannot accept a migration to a later generation.
			if !manifest.Removed {
				content, err = json.Marshal(struct {
					Version      int
					Core, Policy []byte
				}{1, content, prepared.Content})
				if err != nil {
					return err
				}
			}
		}
		manifest.Generation, err = settle(record, manifest.Binding, content, manifest.Removed)
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// A lost acknowledgement must never permit different content to
		// reuse this generation, including after process restart.
		return d.saveGenerations(ctx, record)
	})
	if err != nil {
		return Receipt{}, err
	}
	if err = ctx.Err(); err != nil {
		return Receipt{}, err
	}
	payload, err := solutionhost.SignedPayload(manifest)
	if err != nil {
		return Receipt{}, err
	}
	return d.post(ctx, manifest.Alias, payload, post)
}

func (d *NativeDeliverer) post(ctx context.Context, module string, payload []byte, post DeliveryPost) (Receipt, error) {
	envelope, err := solutionhost.SignEnvelope(payload, d.Signer)
	if err != nil {
		return Receipt{}, err
	}
	signed, err := solutionhost.Carrier(payload, envelope)
	if err != nil {
		return Receipt{}, err
	}
	body, err := solutionhost.MarshalSigned(signed)
	if err != nil {
		return Receipt{}, err
	}
	token, err := d.Token(module)
	if err != nil {
		return Receipt{}, err
	}
	if post != nil {
		return post(ctx, module, signed, token)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.DeliveryURL, "/")+manifestDeliveryPath, bytes.NewReader(body))
	if err != nil {
		return Receipt{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return Receipt{}, fmt.Errorf("deliver the module manifest to the host: %w", err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	receipt := Receipt{Status: response.StatusCode}
	_ = json.Unmarshal(answer, &receipt)
	if response.StatusCode != http.StatusOK {
		message := receipt.Error
		if message == "" {
			message = strings.TrimSpace(string(answer))
		}
		return receipt, &HostStatusError{Status: response.StatusCode, Message: fmt.Sprintf("the host refused the module manifest (%s): %s", response.Status, message)}
	}
	return receipt, nil
}

// HostStatusError is an answer the host or its gateway gave with a status
// other than success. The status is kept, not only printed, so the caller
// that retries can tell a host that is not serving yet from one that refused
// for good (a policy refusal, a stale or mismatched document). Gateway marks
// a request made through the host's gateway (the fixture sign-in, the module
// envelope), where a 404 is a route the gateway has not exposed yet; a 404
// from the host's own delivery endpoint is a wrong endpoint. Envelope marks a
// read or write of the module envelope, where a 409 is a lost revision race.
type HostStatusError struct {
	Status   int
	Gateway  bool
	Envelope bool
	Message  string
}

func (e *HostStatusError) Error() string { return e.Message }

// EnvelopeGrant is what the administrator grants one module principal, in the
// host's own grant shape, as a composition states it beside itself
// (configurations/<environment>/module-envelope.json, "principals").
type EnvelopeGrant = json.RawMessage

// BuildEnvelope writes the envelope document the host's administrator writes
// on a cell: this revision, and each module principal's grant.
func BuildEnvelope(revision uint64, grants map[string]EnvelopeGrant) ([]byte, error) {
	principals := make(map[string]any, len(grants))
	for principal, grant := range grants {
		principals[principal] = map[string]json.RawMessage{"grant": grant}
	}
	return json.Marshal(map[string]any{envelopeRevisionField: revision, "principals": principals})
}

const (
	// envelopeRevisionField is the envelope document's revision key.
	envelopeRevisionField = "revision"
	// fixtureTokenField names the fixture user in the host's sign-in body.
	fixtureTokenField = "token"
)

// EnvelopeWriter writes the envelope through the host's administrator API,
// signed in as the fixture administrator a local run selects.
type EnvelopeWriter struct {
	// GatewayURL is the host gateway's REST origin.
	GatewayURL string
	// FixtureProvider and FixtureToken sign the administrator in: the fixture
	// user the environment's fixture declares as super admin.
	FixtureProvider string
	FixtureToken    string
	Client          *http.Client
	accessToken     string
}

func (w *EnvelopeWriter) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (w *EnvelopeWriter) signIn(ctx context.Context) error {
	if w.accessToken != "" {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"provider":    w.FixtureProvider,
		"device_info": "codefly run",
		"fixture":     map[string]string{fixtureTokenField: w.FixtureToken},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(w.GatewayURL, "/")+"/v1/auth/authenticate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := w.client().Do(request)
	if err != nil {
		return fmt.Errorf("sign in as the fixture administrator: %w", err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	if response.StatusCode != http.StatusOK {
		return &HostStatusError{Status: response.StatusCode, Gateway: true, Message: fmt.Sprintf("sign in as the fixture administrator (provider %q): %s: %s", w.FixtureProvider, response.Status, strings.TrimSpace(string(answer)))}
	}
	var session struct {
		AccessToken string `json:"accessToken"`
		MFARequired bool   `json:"mfaRequired"`
	}
	if err := json.Unmarshal(answer, &session); err != nil || session.AccessToken == "" {
		return fmt.Errorf("the fixture administrator's sign-in answered no access token (mfa required: %v)", session.MFARequired)
	}
	w.accessToken = session.AccessToken
	return nil
}

func (w *EnvelopeWriter) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	status, answer, err := w.doAuthenticated(ctx, method, path, body)
	if err != nil || status != http.StatusUnauthorized {
		return status, answer, err
	}
	// A long-running development session can outlive the fixture access
	// token. Reauthenticate on demand and replay once, with the same body
	// and expected revision. A forbidden or ambiguous write is not retried.
	w.accessToken = ""
	return w.doAuthenticated(ctx, method, path, body)
}

func (w *EnvelopeWriter) doAuthenticated(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	if err := w.signIn(ctx); err != nil {
		return 0, nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(w.GatewayURL, "/")+path, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+w.accessToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := w.client().Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, answer, nil
}

type envelopeAnswer struct {
	Envelope struct {
		Revision json.Number `json:"revision"`
		Document string      `json:"document"`
	} `json:"envelope"`
}

// Current returns the host's current envelope revision and document; 0 and
// nil when none was written.
func (w *EnvelopeWriter) Current(ctx context.Context) (uint64, []byte, error) {
	status, answer, err := w.do(ctx, http.MethodGet, "/v1/platform/module-envelope", nil)
	if err != nil {
		return 0, nil, err
	}
	if status == http.StatusNotFound {
		return 0, nil, nil
	}
	if status != http.StatusOK {
		return 0, nil, &HostStatusError{Status: status, Gateway: true, Envelope: true, Message: fmt.Sprintf("read the module envelope: HTTP %d: %s", status, strings.TrimSpace(string(answer)))}
	}
	var parsed envelopeAnswer
	if err = json.Unmarshal(answer, &parsed); err != nil {
		return 0, nil, err
	}
	if parsed.Envelope.Document == "" {
		return 0, nil, nil
	}
	revision, err := strconv.ParseUint(parsed.Envelope.Revision.String(), 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("the module envelope's revision %q is not a revision: %w", parsed.Envelope.Revision, err)
	}
	return revision, []byte(parsed.Envelope.Document), nil
}

// Ensure writes the envelope `build` produces for the next revision unless the
// current one already carries the same content, and returns the revision in
// force.
func (w *EnvelopeWriter) Ensure(ctx context.Context, build func(revision uint64) ([]byte, error)) (uint64, error) {
	current, document, err := w.Current(ctx)
	if err != nil {
		return 0, err
	}
	if document != nil {
		same, buildErr := build(current)
		if buildErr != nil {
			return 0, buildErr
		}
		if equivalentJSON(same, document) {
			return current, nil
		}
	}
	next, err := build(current + 1)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{"expectedRevision": current, "document": string(next)})
	status, answer, err := w.do(ctx, http.MethodPut, "/v1/platform/module-envelope", body)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, &HostStatusError{Status: status, Gateway: true, Envelope: true, Message: fmt.Sprintf("write module envelope revision %d: HTTP %d: %s", current+1, status, strings.TrimSpace(string(answer)))}
	}
	return current + 1, nil
}

func equivalentJSON(a, b []byte) bool {
	var left, right any
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	normalizedA, _ := json.Marshal(left)
	normalizedB, _ := json.Marshal(right)
	return bytes.Equal(normalizedA, normalizedB)
}
