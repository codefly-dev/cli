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

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// Native delivery: a laptop run delivers to the host what a cell's delivery
// Job delivers — the same presence and authority documents, signed with the
// developer key, POSTed to the same route with a projected token the run's
// own API server reviews — so the host's registry, mint and installation run
// their one code path. What differs is the approved build: a cell pins an OCI
// image manifest; a native run pins the sha256 of the executable it started,
// which the run's API server reports as the pod's image, so the host's
// approved-build check compares what it compares on a cell.

// NativeIdentity is one workload a native run projects a token for and
// delivers presence and authority for: a service that declares
// module-identity, the account it would run as in a cell, and its mesh
// principal — the SPIFFE ID the cell's mesh issues to that account, which the
// presence names and nothing on a laptop presents.
type NativeIdentity struct {
	Module   string
	Service  string
	Account  string
	SPIFFEID string
}

// WorkloadAccount is the ServiceAccount name a service runs as: Core's
// default for its name, the same one a cell's render projects.
func WorkloadAccount(service string) string {
	return workloadAccount(service)
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
		namespace := env.ModuleNamespace(workspace, ref.Module)
		if namespace == "" {
			return nil, fmt.Errorf("module-identity service %s/%s has no resolved namespace: the environment declares none", ref.Module, ref.Name)
		}
		identities[resources.ServiceUnique(ref.Module, ref.Name)] = NativeIdentity{
			Module: ref.Module, Service: ref.Name, Account: workloadAccount(ref.Name),
			SPIFFEID: meshPrincipal(env.Host, namespace, ref.Name),
		}
	}
	return identities, nil
}

// NativeWorkload is one native process the run delivers presence for: the
// module it belongs to, the service that is its serving workload, and the
// identity it runs under.
type NativeWorkload struct {
	Module    *resources.Module
	Service   *resources.Service
	Services  []*resources.Service
	Namespace string
	Account   string
	SPIFFEID  string
	// Subject is the principal the environment declares for the workload
	// (service-identity), the identity the host expects.
	Subject string
	// Executable is the binary the agent started; its digest is the build.
	Executable string
}

// Repository names a native build's image repository: there is no registry,
// so the name states where the build came from.
func (w *NativeWorkload) Repository(workspace string) string {
	return "native/" + strings.ToLower(workspace) + "/" + strings.ToLower(w.Module.Name) + "/" + strings.ToLower(w.Service.Name)
}

// ExecutableDigest is the sha256 of the started binary, in digest form.
func ExecutableDigest(path string) (solutionhost.ImageDigest, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read the started executable: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return solutionhost.ImageDigest("sha256:" + hex.EncodeToString(hash.Sum(nil))), nil
}

// NativeDocuments are what one native workload delivers.
type NativeDocuments struct {
	Presence  *solutionhost.SolutionHostBinding
	Authority *solutionhost.AuthorityDocument
	Digest    solutionhost.ImageDigest
	// Principal, Bindings and Ceilings are what the envelope admits for the
	// module, derived from the same contract the authority was rendered from.
	Principal string
	Bindings  []solutionhost.AuthorityBinding
	Ceilings  []solutionhost.ScopeCeiling
}

// NativeDeliverer renders, signs and delivers a native run's documents.
type NativeDeliverer struct {
	Workspace *resources.Workspace
	Env       *environments.Environment
	Signer    solutionhost.Signer
	// StateDir keeps the generations delivered so far: a rebuilt workload is
	// the next generation, an unchanged one is replayed.
	StateDir string
	// DeliveryURL is the host's delivery listener (the accounts REST address).
	DeliveryURL string
	// Token mints the delivery account's projected token for a namespace.
	Token func(namespace string) (string, error)
	// EnvelopeRevision is the host's current envelope revision, which every
	// document names.
	EnvelopeRevision uint64
	Client           *http.Client
}

// Render builds a native workload's presence and authority documents.
func (d *NativeDeliverer) Render(ctx context.Context, workload *NativeWorkload) (*NativeDocuments, error) {
	host := d.Env.Host
	if host == nil {
		return nil, errors.New("the environment declares no host")
	}
	// Deliver stamps the revision in force once the envelope is written; a
	// document is rendered before that, and validates against revision 1 as
	// the lowest a host admits.
	revision := d.EnvelopeRevision
	if revision == 0 {
		revision = 1
	}
	digest, err := ExecutableDigest(workload.Executable)
	if err != nil {
		return nil, err
	}
	manifest, err := composition.LoadPackageManifest(workload.Module.Dir())
	if err != nil {
		return nil, fmt.Errorf("module %s declares no package manifest, so it names no release: %w", workload.Module.Name, err)
	}
	declared, err := solutionrun.ModuleSolution(workload.Module)
	if err != nil {
		return nil, fmt.Errorf("read solution manifest of module %s: %w", workload.Module.Name, err)
	}
	instance := &SolutionInstance{Kind: solutionhost.KindModule, Name: workload.Module.Name, Alias: workload.Module.Name, Package: manifest.ID, Version: manifest.Version}
	if declared != nil {
		instance.Kind = solutionhost.KindSolution
		if instance.Frontend, err = solutionrun.DeclaredFrontend(ctx, workload.Module); err != nil {
			return nil, err
		}
	}
	instance.Modules = []SolutionModulePin{{Module: workload.Module.Name, Package: manifest.ID, Version: manifest.Version}}
	if instance.ReleaseDigest, err = releaseDigest(workload.Module.Dir()); err != nil {
		return nil, fmt.Errorf("module %s: %w", workload.Module.Name, err)
	}
	unit := SolutionArtifactUnit{Name: workload.Service.Name, Path: workload.Service.Name, Subject: workload.Subject}
	instance.Units = []SolutionArtifactUnit{unit}
	for _, service := range workload.Services {
		for _, endpoint := range service.Endpoints {
			if endpoint == nil {
				continue
			}
			if endpoint.API == "" {
				return nil, fmt.Errorf("%s %s service %s endpoint %s declares no api", instance.Kind, workload.Module.Name, service.Name, endpoint.Name)
			}
			instance.Endpoints = append(instance.Endpoints, SolutionEndpoint{
				Name: endpoint.Name, Service: service.Name, Module: workload.Module.Name,
				API: endpoint.API, Visibility: endpoint.Visibility, Exposure: endpoint.Exposure,
			})
		}
	}
	id, err := bindingID(d.Workspace.Name, d.Env.Name, instance.Name)
	if err != nil {
		return nil, err
	}
	publisher, name, err := releaseIdentity(instance)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", instance.Kind, instance.Name, err)
	}
	release := solutionhost.Release{Publisher: publisher, Name: name, Version: instance.Version, Digest: instance.ReleaseDigest}
	presence := &solutionhost.SolutionHostBinding{
		Schema: solutionhost.SchemaPresenceV1, Kind: instance.Kind, Binding: id, Generation: 1,
		OwnershipDomain: host.Domain, EnvelopeRevision: revision,
		Host:    solutionhost.HostTarget{Coordinate: host.Coordinate, Component: host.Component},
		Release: release,
	}
	if !rfc1123Label.MatchString(instance.Alias) {
		return nil, fmt.Errorf("%s %s claims the route alias %q, which is not one URL path segment", instance.Kind, instance.Name, instance.Alias)
	}
	presence.Routes = []solutionhost.Route{{Alias: instance.Alias, Surface: solutionhost.SurfaceBackend}}
	presence.Frontend = instance.Frontend
	// The artifact's rendered digest names what the run rendered for the unit
	// — the workload's image reference — and the workload's image digest names
	// the build; core refuses a document that uses one digest for both.
	rendered := sha256.Sum256([]byte("native workload\n" + workload.Repository(d.Workspace.Name) + "@" + string(digest) + "\n"))
	presence.Artifacts = []solutionhost.Artifact{{
		Surface: solutionhost.SurfaceBackend, Name: unit.Name, Release: release.Identity(),
		Digest: solutionhost.RenderedDigest("sha256:" + hex.EncodeToString(rendered[:])),
	}}
	others := []string{}
	presence.Workloads = []solutionhost.Workload{{
		Name: workload.Service.Name, Artifact: unit.Name, Container: workload.Service.Name,
		Image:             solutionhost.Image{Repository: workload.Repository(d.Workspace.Name), Digest: digest},
		Identity:          solutionhost.WorkloadIdentity{Audience: host.Audience, Subject: workload.Subject, SPIFFEID: workload.SPIFFEID},
		NonAuthenticating: &others,
	}}
	for _, endpoint := range instance.Endpoints {
		presence.Endpoints = append(presence.Endpoints, solutionhost.Endpoint{
			Name: endpoint.Name, Service: endpoint.Service, Module: endpoint.Module,
			API: endpoint.API, Visibility: endpoint.Visibility, Exposure: endpoint.Exposure,
		})
	}
	for _, pin := range instance.Modules {
		presence.Modules = append(presence.Modules, solutionhost.ModulePin{Module: pin.Module, Package: pin.Package, Version: pin.Version})
	}
	if err = presence.Validate(); err != nil {
		return nil, fmt.Errorf("%s %s declares an invalid presence document: %w", instance.Kind, instance.Name, err)
	}
	documents := &NativeDocuments{Presence: presence, Digest: digest}

	instances, _, err := authorityInstancesOf(ctx, d.Workspace, workload.Module, workload.Services, d.Env, instance.Units, instance.Endpoints)
	if err != nil {
		return nil, err
	}
	for i := range instances {
		if instances[i].Service != workload.Service.Name {
			continue
		}
		contract := instances[i].Contract
		authority := &solutionhost.AuthorityDocument{
			Schema: solutionhost.SchemaAuthorityV1, Authority: authorityID(id), PresenceBinding: id, Generation: 1,
			Host:            solutionhost.HostTarget{Coordinate: host.Coordinate, Component: host.Component},
			OwnershipDomain: host.Domain, EnvelopeRevision: revision,
			ApprovedBuild: digest, EffectiveFrom: 1,
			Principals:    []solutionhost.PrincipalAuthority{{Principal: contract.Principal, Bindings: authorityBindings(id, contract)}},
			Queues:        declaredStrings(contract.Queues),
			Namespaces:    declaredStrings(contract.Namespaces),
			ScopeCeilings: declaredScopeCeilings(contract.ScopeCeilings),
		}
		if err := authority.Validate(); err != nil {
			return nil, fmt.Errorf("authority of %s/%s is invalid: %w", workload.Module.Name, workload.Service.Name, err)
		}
		documents.Authority = authority
		documents.Principal = contract.Principal
		documents.Bindings = authorityBindings(id, contract)
		documents.Ceilings = declaredScopeCeilings(contract.ScopeCeilings)
	}
	return documents, nil
}

// nativeGenerations is the run's record of what it delivered: per binding or
// authority, the generation and the content it carried.
type nativeGenerations struct {
	Entries map[string]nativeGeneration `json:"entries"`
}

type nativeGeneration struct {
	Generation uint64 `json:"generation"`
	Content    string `json:"content"`
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

func (d *NativeDeliverer) saveGenerations(record *nativeGenerations) error {
	if err := os.MkdirAll(d.StateDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(d.generationsPath(), data, 0o600)
}

// settle assigns a document its generation: the one it had when its content
// is unchanged, the next one otherwise. content is the canonical payload with
// the generation zeroed.
func settle(record *nativeGenerations, key string, content []byte) uint64 {
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	entry, known := record.Entries[key]
	if known && entry.Content == digest {
		return entry.Generation
	}
	next := uint64(1)
	if known {
		next = entry.Generation + 1
	}
	record.Entries[key] = nativeGeneration{Generation: next, Content: digest}
	return next
}

// Receipt is the host's answer to one delivered document.
type Receipt struct {
	Kind        string
	Disposition string `json:"disposition"`
	DocumentID  string `json:"documentId"`
	Generation  uint64 `json:"generation"`
	Signer      string `json:"signer"`
	Error       string `json:"error"`
}

// Deliver signs the documents and POSTs them to the host.
func (d *NativeDeliverer) Deliver(ctx context.Context, documents *NativeDocuments) ([]Receipt, error) {
	record, err := d.loadGenerations()
	if err != nil {
		return nil, err
	}
	// A document's generation is settled from its content with the generation
	// held at 1 — the lowest core admits — so the same content always settles
	// to the same generation, whatever it was delivered as before.
	presence := documents.Presence
	presence.Generation = 1
	content, err := solutionhost.SignedPayload(presence)
	if err != nil {
		return nil, err
	}
	presence.Generation = settle(record, "presence:"+presence.Binding, content)
	var receipts []Receipt
	payload, err := solutionhost.SignedPayload(presence)
	if err != nil {
		return nil, err
	}
	receipt, err := d.post(ctx, "presence", presence.Workloads[0].Identity.SPIFFEID, payload)
	if err != nil {
		return nil, err
	}
	receipts = append(receipts, receipt)
	if documents.Authority != nil {
		authority := documents.Authority
		authority.EffectiveFrom = presence.Generation
		authority.Generation = 1
		content, err = solutionhost.SignedPayloadFor(authority)
		if err != nil {
			return nil, err
		}
		authority.Generation = settle(record, "authority:"+authority.Authority, content)
		payload, err := solutionhost.SignedPayloadFor(authority)
		if err != nil {
			return nil, err
		}
		receipt, err := d.post(ctx, "authority", "", payload)
		if err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	if err := d.saveGenerations(record); err != nil {
		return nil, err
	}
	return receipts, nil
}

// authorityDeliveryNamespace is the fixed namespace a host accepts authority
// from, the same name the host holds.
const authorityDeliveryNamespace = "platform-authority"

func (d *NativeDeliverer) post(ctx context.Context, kind, spiffeID string, payload []byte) (Receipt, error) {
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
	namespace := authorityDeliveryNamespace
	if kind == "presence" {
		identity, parseErr := parseSPIFFENamespace(spiffeID)
		if parseErr != nil {
			return Receipt{}, parseErr
		}
		namespace = identity
	}
	token, err := d.Token(namespace)
	if err != nil {
		return Receipt{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(d.DeliveryURL, "/")+"/platform/_delivery/"+kind, bytes.NewReader(body))
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
		return Receipt{}, fmt.Errorf("deliver %s to the host: %w", kind, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 1<<16))
	receipt := Receipt{Kind: kind}
	_ = json.Unmarshal(answer, &receipt)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		message := receipt.Error
		if message == "" {
			message = strings.TrimSpace(string(answer))
		}
		return receipt, fmt.Errorf("the host refused the %s delivery (%s): %s", kind, response.Status, message)
	}
	return receipt, nil
}

// parseSPIFFENamespace reads the namespace out of spiffe://<domain>/ns/<ns>/sa/<sa>.
func parseSPIFFENamespace(id string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(id, "spiffe://"), "/")
	if len(parts) != 5 || parts[1] != "ns" || parts[3] != "sa" {
		return "", fmt.Errorf("SPIFFE ID %q is not a Kubernetes workload identity", id)
	}
	return parts[2], nil
}

// EnvelopeGrant is the ceiling a composition states for one module principal
// (configurations/<environment>/module-envelope.json): the administrator's
// policy, in the host's own grant shape, kept beside the composition that
// runs the module.
type EnvelopeGrant = json.RawMessage

// EnvelopePrincipal is one principal as the run admits it: what its
// contract asks (bindings, ceilings), the workload that presents it, and the
// composition's grant.
type EnvelopePrincipal struct {
	Prefix    string
	Bindings  []solutionhost.AuthorityBinding
	Ceilings  []solutionhost.ScopeCeiling
	SPIFFEID  string
	Container string
	Grant     EnvelopeGrant
}

// ApprovedBuilds reads the builds an envelope document approves.
func ApprovedBuilds(document []byte) []solutionhost.ImageDigest {
	var envelope struct {
		ApprovedBuilds []solutionhost.ImageDigest `json:"approved_builds"`
	}
	if len(document) == 0 || json.Unmarshal(document, &envelope) != nil {
		return nil
	}
	return envelope.ApprovedBuilds
}

// BuildEnvelope writes the envelope document the host's administrator writes
// on a cell: this revision, the builds approved, and each principal's ceiling.
func BuildEnvelope(revision uint64, builds []solutionhost.ImageDigest, principals []EnvelopePrincipal) ([]byte, error) {
	sorted := append([]solutionhost.ImageDigest(nil), builds...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	entries := map[string]any{}
	document := map[string]any{envelopeRevisionField: revision, "approved_builds": sorted, "principals": entries}
	for _, principal := range principals {
		bindings := make([]map[string]any, 0, len(principal.Bindings))
		for _, binding := range principal.Bindings {
			entry := map[string]any{"id": binding.ID, "revision": binding.Revision, "scope": binding.Scope}
			if binding.Queue != "" {
				entry["queue"] = binding.Queue
			}
			if binding.Namespace != "" {
				entry["namespace"] = binding.Namespace
			}
			if binding.BindingKey != "" {
				entry["binding_key"] = binding.BindingKey
			}
			if binding.LookupMethod != "" {
				entry["lookup_method"] = binding.LookupMethod
			}
			bindings = append(bindings, entry)
		}
		ceilings := make([]map[string]any, 0, len(principal.Ceilings))
		for _, ceiling := range principal.Ceilings {
			ceilings = append(ceilings, map[string]any{"resource_kind": ceiling.ResourceKind, "actions": ceiling.Actions})
		}
		grant := json.RawMessage(`{}`)
		if len(principal.Grant) > 0 {
			grant = principal.Grant
		}
		entries[principal.Prefix] = map[string]any{
			"bindings":            bindings,
			"workload_identities": []map[string]any{{"spiffe_id": principal.SPIFFEID, "container": principal.Container}},
			"scope_ceilings":      ceilings,
			"grant":               grant,
		}
	}
	return json.Marshal(document)
}

// KeepGrantedPrincipals returns next with every principal of current that next
// does not name, carried over exactly as the host holds it.
//
// A run writes the envelope for the workloads it has started so far, and a
// host that kept its store holds principals from earlier runs and from
// workloads this run has not reached yet. Leaving them out would withdraw
// them, and withdrawing is a narrowing: a witnessed act the host refuses from
// a development loop, rightly. So what the host already grants stays, and a
// run only ever adds.
func KeepGrantedPrincipals(next, current []byte) ([]byte, error) {
	if len(current) == 0 {
		return next, nil
	}
	var held struct {
		Principals map[string]json.RawMessage `json:"principals"`
	}
	if err := json.Unmarshal(current, &held); err != nil {
		return nil, fmt.Errorf("read the principals the host's envelope holds: %w", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(next, &document); err != nil {
		return nil, err
	}
	principals := map[string]json.RawMessage{}
	if raw, ok := document["principals"]; ok {
		if err := json.Unmarshal(raw, &principals); err != nil {
			return nil, err
		}
	}
	for prefix, section := range held.Principals {
		if _, named := principals[prefix]; !named {
			principals[prefix] = section
		}
	}
	encoded, err := json.Marshal(principals)
	if err != nil {
		return nil, err
	}
	document["principals"] = encoded
	return json.Marshal(document)
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
		return fmt.Errorf("sign in as the fixture administrator %q (provider %q): %s: %s", w.FixtureToken, w.FixtureProvider, response.Status, strings.TrimSpace(string(answer)))
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
		return 0, nil, fmt.Errorf("read the module envelope: HTTP %d: %s", status, strings.TrimSpace(string(answer)))
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

// Plan reads the envelope in force and reports the revision the documents
// about to be delivered must name, and how to commit it.
//
// When the current revision already carries what `build` produces, that
// revision is the answer and commit writes nothing. Otherwise the answer is
// the next revision and commit writes it, expecting the one that was read.
func (w *EnvelopeWriter) Plan(ctx context.Context, build func(revision uint64, current []byte) ([]byte, error)) (uint64, func(context.Context) error, error) {
	current, document, err := w.Current(ctx)
	if err != nil {
		return 0, nil, err
	}
	if document != nil {
		same, buildErr := build(current, document)
		if buildErr != nil {
			return 0, nil, buildErr
		}
		if equivalentJSON(same, document) {
			return current, func(context.Context) error { return nil }, nil
		}
	}
	next, err := build(current+1, document)
	if err != nil {
		return 0, nil, err
	}
	commit := func(ctx context.Context) error {
		body, _ := json.Marshal(map[string]any{"expectedRevision": current, "document": string(next)})
		status, answer, err := w.do(ctx, http.MethodPut, "/v1/platform/module-envelope", body)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("write module envelope revision %d: HTTP %d: %s", current+1, status, strings.TrimSpace(string(answer)))
		}
		return nil
	}
	return current + 1, commit, nil
}

// Ensure writes the envelope `build` produces for the next revision unless
// the current one already carries the same content, and returns the
// revision in force.
func (w *EnvelopeWriter) Ensure(ctx context.Context, build func(revision uint64, current []byte) ([]byte, error)) (uint64, error) {
	revision, commit, err := w.Plan(ctx, build)
	if err != nil {
		return 0, err
	}
	if err := commit(ctx); err != nil {
		return 0, err
	}
	return revision, nil
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
