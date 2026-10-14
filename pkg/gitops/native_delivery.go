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
	"github.com/codefly-dev/core/agents/contract"
	agentservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/composition"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// Native delivery: a laptop run delivers to the host what a cell's delivery
// Job delivers — the same presence and authority documents, signed with the
// developer key, POSTed to the same route with a projected token the run's
// own API server reviews — so the host's registry, mint and installation run
// their one code path. What differs is the approved build: a cell pins an OCI
// image manifest; a native run pins the sha256 of what the agent reports it
// started, which the run's API server reports as the pod's image, so the
// host's approved-build check compares what it compares on a cell.

// NativeArtifact is what one agent reported it started, as the run digests it.
// It is the agent's answer (runtime-started-artifact/v1), never a path the run
// derived: the build a native run approves is what actually ran, and only the
// agent that ran it knows where that is.
type NativeArtifact struct {
	// Executable is the program the agent started: the service's own binary
	// for a compiled runtime, the interpreter or launcher for an interpreted
	// one.
	Executable string
	// Sources are the files that, with the executable, identify what is
	// running — an interpreted runtime's entrypoint, the modules it loads and
	// its dependency lock. Empty for a compiled runtime.
	Sources []string
}

// StartedArtifactOf reads what the agent reported it started for a service.
//
// An agent that reported nothing does not implement
// runtime-started-artifact/v1, and that is where a native run stops: the build
// it approves and signs is the one the process is at, so a path guessed from
// some agent family's layout would be a claim about a file the run never saw
// started. The refusal names the agent, because adopting the contract there is
// the only thing that lifts it.
func StartedArtifactOf(service *resources.Service, reported *runtimev0.StartedArtifact) (NativeArtifact, error) {
	artifact, ok := agentservices.StartedArtifact(&runtimev0.StartStatus{StartedArtifact: reported})
	if !ok {
		return NativeArtifact{}, fmt.Errorf(
			"agent %s reports no started artifact for %s: a native run approves the build the agent says it started, so the agent must implement %s",
			agentOf(service), resources.WithUnique(service).Unique(), contract.RuntimeStartedArtifact)
	}
	// The contract says every path is absolute. A relative one would be
	// opened against this process's working directory, which is the user's
	// terminal and has nothing to do with what the agent ran — so it is
	// refused rather than resolved into a file that happens to be there.
	for _, path := range append([]string{artifact.GetExecutable()}, artifact.GetSources()...) {
		if !filepath.IsAbs(path) {
			return NativeArtifact{}, fmt.Errorf(
				"agent %s reports %q as started for %s, which is not an absolute path: %s names the files it ran, and this run resolves none of them",
				agentOf(service), path, resources.WithUnique(service).Unique(), contract.RuntimeStartedArtifact)
		}
	}
	return NativeArtifact{Executable: artifact.GetExecutable(), Sources: artifact.GetSources()}, nil
}

// agentOf names the agent a service runs on for a refusal; a service with no
// agent declaration is named as such rather than as an empty string.
func agentOf(service *resources.Service) string {
	if service == nil || service.Agent == nil {
		return "<none declared>"
	}
	return service.Agent.Identifier()
}

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
	// Artifact is what the agent reported it started; its digest is the build.
	Artifact NativeArtifact
	// FrontendBundle is the identity of the complete frontend bundle this run
	// built and retained in the workspace's artifact cache (its digest, the
	// canonical archive's media type, the artifact name a release would use):
	// what the presence's frontend payload names, since what runs is what is
	// delivered. Nil when the component declares no page or built no bundle.
	FrontendBundle *solutionhost.FrontendPayload
}

// Repository names a native build's image repository: there is no registry,
// so the name states where the build came from.
func (w *NativeWorkload) Repository(workspace string) string {
	return "native/" + strings.ToLower(workspace) + "/" + strings.ToLower(w.Module.Name) + "/" + strings.ToLower(w.Service.Name)
}

// closureManifestDomain opens the manifest an interpreted runtime's build is
// digested from. It separates the two shapes a build digest is taken over: a
// compiled runtime's build is the sha256 of its executable's own bytes, so
// without a domain an executable whose content happened to be a manifest
// would carry a closure's build digest.
const closureManifestDomain = "codefly/native-closure/v1\n"

// Digest is the build a native run approves for what the agent started.
//
// A compiled runtime starts its own build, so the digest is the sha256 of that
// one executable — the value `shasum -a 256` prints for the file, what this
// has always been, and what keeps a Go workload's build unchanged across this
// contract. An interpreted runtime starts an interpreter that says nothing
// about the service, so the build is the closure the agent named: the digest
// is taken over a manifest listing every file in it with its own sha256, so
// editing a source is a different build.
//
// The manifest is length-prefixed rather than written as text lines, so no
// file's name can encode entries of its own: a source called
// "a\n<digest>  b" would otherwise forge a second entry and let two different
// closures share one approved build.
//
// root makes the manifest's names stable: a file inside the workspace is named
// relative to it, so the same tree digests the same wherever it is checked
// out. Both sides are resolved through their symlinks first, because a
// workspace reached as /var and reported as /private/var is one tree and must
// be one build. A file outside — an interpreter in /usr, a shared virtualenv —
// is named by its absolute path, which is the only name it has, and that path
// is part of the build.
func (a NativeArtifact) Digest(root string) (solutionhost.ImageDigest, error) {
	if a.Executable == "" {
		return "", errors.New("the agent reported no started executable")
	}
	sources := a.startedBeside()
	if len(sources) == 0 {
		return fileDigest(a.Executable)
	}
	root = resolved(root)
	entries := make([]string, 0, len(sources)+1)
	for _, path := range append([]string{a.Executable}, sources...) {
		digest, err := fileDigest(path)
		if err != nil {
			return "", err
		}
		entries = append(entries, manifestEntry(artifactName(root, path), digest))
	}
	// Sorted, so the agent's enumeration order is not part of the build, and
	// de-duplicated, so naming a file twice is not a different build either.
	sort.Strings(entries)
	hash := sha256.New()
	hash.Write([]byte(closureManifestDomain))
	var previous string
	for _, entry := range entries {
		if entry == previous {
			continue
		}
		previous = entry
		hash.Write([]byte(entry))
	}
	return solutionhost.ImageDigest("sha256:" + hex.EncodeToString(hash.Sum(nil))), nil
}

// manifestEntry is one file in a closure manifest: the byte length of its
// name, the name, and the sha256 of its content.
//
// The length comes first so the name is read by count rather than by looking
// for a delimiter, and a file name may contain any byte but NUL — a newline
// included. Written as plain delimited text, one file could spell out entries
// of its own and two different closures would digest alike. Keep the encoding
// self-delimiting: the length, one space, exactly that many bytes of name,
// one space, then the fixed-width digest.
func manifestEntry(name string, digest solutionhost.ImageDigest) string {
	return fmt.Sprintf("%d %s %s\n", len(name), name, digest)
}

// startedBeside is the sources the agent named other than the executable
// itself. An agent that helpfully lists the binary it ran among its sources
// has described the same process as one that leaves the list empty, and the
// two must be one build.
func (a NativeArtifact) startedBeside() []string {
	executable := resolved(a.Executable)
	out := make([]string, 0, len(a.Sources))
	for _, source := range a.Sources {
		if resolved(source) == executable {
			continue
		}
		out = append(out, source)
	}
	return out
}

// artifactName is a file's name in the digest manifest. root is already
// resolved, and the path's DIRECTORY is resolved here, so a tree reached by
// two spellings of the same directory is one build.
//
// The file's own link is deliberately not followed. A virtualenv's
// .venv/bin/python is a symlink to an interpreter outside the workspace, and
// following it would name the build after wherever that interpreter happens
// to live — so a homebrew revision, a pyenv rehash or a second developer's
// machine would each produce a fresh build for a tree nobody changed. What
// the workspace holds is the link, at a stable name, and fileDigest reads
// through it for the content.
func artifactName(root, path string) string {
	path = filepath.Join(resolved(filepath.Dir(path)), filepath.Base(path))
	if root == "" {
		return filepath.ToSlash(path)
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

// resolved is a path with its symlinks followed, or the cleaned path when it
// cannot be followed. The fallback is unreachable from Digest, which digests
// a file's content before naming it and so has already refused a path that
// cannot be read; it is here so a name exists rather than a panic if that
// order ever changes.
func resolved(path string) string {
	if path == "" {
		return ""
	}
	if evaluated, err := filepath.EvalSymlinks(path); err == nil {
		return evaluated
	}
	return filepath.Clean(path)
}

// fileDigest is the sha256 of one started file, in digest form. It reads a
// regular file only: a directory is the agent declining to say what its
// closure contains, which is not for the run to decide.
func fileDigest(path string) (solutionhost.ImageDigest, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s, which the agent reported starting: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the agent reported %s as started, and it is not a regular file: an agent names the files it ran, never a directory for the run to walk", path)
	}
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
	digest, err := workload.Artifact.Digest(d.Workspace.Dir())
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
	}
	if instance.Frontend, err = solutionrun.DeclaredFrontend(ctx, workload.Module); err != nil {
		return nil, err
	}
	if err = stampFrontendPayload(instance, manifest.ReleaseArtifacts); err != nil {
		return nil, err
	}
	// The bundle this run built outranks the release's artifact metadata: a
	// native run delivers what runs, and the host reads the bytes from the
	// cache by the digest named here. A declared page without the bundle this
	// run built is refused: a host admits no new frontend without its signed
	// payload, and a release's digest is not what runs here.
	switch {
	case workload.FrontendBundle != nil && instance.Frontend == nil:
		return nil, fmt.Errorf("module %s built a frontend bundle and declares no frontend; a bundle is the output of a declared page", workload.Module.Name)
	case workload.FrontendBundle != nil:
		payload := *workload.FrontendBundle
		instance.Frontend.Payload = &payload
	case instance.Frontend != nil:
		return nil, fmt.Errorf("module %s declares a frontend and this run built no bundle for it; a native delivery names the bundle it built, and a host admits no frontend without its signed payload", workload.Module.Name)
	}
	instance.Group = solutionGroupOf(d.Workspace, instance)
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
	presence.Group = instance.Group
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

// WithdrawnDocuments is the generation that withdraws what a native run
// delivered for a workload: the presence's tombstone and, when an authority
// was delivered, its tombstone, each the same withdrawal a publication
// settles (removed, the next generation, nothing that describes the module
// left on it). The host withdraws the endpoints with the presence, so none
// travel with it; the envelope is not narrowed, since a run only ever adds to
// it. A withdrawal is terminal for the binding at the host: core refuses the
// binding presented again, so a component withdrawn from a run comes back
// under a new instance name.
func WithdrawnDocuments(delivered *NativeDocuments) (*NativeDocuments, error) {
	if delivered == nil || delivered.Presence == nil {
		return nil, fmt.Errorf("nothing was delivered, so there is nothing to withdraw")
	}
	presence, err := tombstoneOf(delivered.Presence)
	if err != nil {
		return nil, fmt.Errorf("withdraw binding %s: %w", delivered.Presence.Binding, err)
	}
	withdrawn := &NativeDocuments{Presence: presence, Digest: delivered.Digest, Principal: delivered.Principal}
	if delivered.Authority != nil {
		if withdrawn.Authority, err = authorityTombstoneOf(delivered.Authority); err != nil {
			return nil, fmt.Errorf("withdraw authority %s: %w", delivered.Authority.Authority, err)
		}
	}
	return withdrawn, nil
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
	Status      int    `json:"-"`
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
	if kind == "presence" || kind == endpointDeliveryKind {
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
	receipt := Receipt{Kind: kind, Status: response.StatusCode}
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

// endpointDeliveryKind is the host inbox a signed endpoint delivery is posted
// to: the same inbox, credentials and signing path as the presence it joins.
const endpointDeliveryKind = "endpoints"

// endpointDeliveryAttempts and endpointDeliveryInterval bound the wait for
// the presence a delivery joins to be APPLIED: the host answers 503 until its
// reconciler has applied the generation the endpoints name, and nothing else
// is retried.
const (
	endpointDeliveryAttempts = 40
	endpointDeliveryInterval = 1500 * time.Millisecond
)

// DeliverEndpoints posts the canonical endpoint delivery of a presence the
// host now holds: the CLI-resolved carriers of the endpoints the presence
// names, bound to its applied generation and digest, signed exactly as the
// presence was and through the same inbox with the same credentials. The
// host joins it to the APPLIED presence: a 503 says that generation is not
// applied yet and is retried within a bound; a stale, mismatched or
// tombstoned delivery (422) and a different document for an applied
// generation (409) are refused for good. A changed address is a new presence
// generation, never a rewrite. No directory, mount or file carries an
// endpoint anywhere.
func (d *NativeDeliverer) DeliverEndpoints(ctx context.Context, presence *solutionhost.SolutionHostBinding, raw []byte) (Receipt, error) {
	if presence == nil || len(presence.Workloads) == 0 {
		return Receipt{}, fmt.Errorf("an endpoint delivery joins a presence with a workload")
	}
	spiffeID := presence.Workloads[0].Identity.SPIFFEID
	for attempt := 1; ; attempt++ {
		receipt, err := d.post(ctx, endpointDeliveryKind, spiffeID, raw)
		if err == nil || receipt.Status != http.StatusServiceUnavailable {
			return receipt, err
		}
		if attempt >= endpointDeliveryAttempts {
			return receipt, fmt.Errorf("the host has not applied presence %s generation %d after %s: %w", presence.Binding, presence.Generation, time.Duration(endpointDeliveryAttempts)*endpointDeliveryInterval, err)
		}
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-time.After(endpointDeliveryInterval):
		}
	}
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
