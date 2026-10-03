package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// --- Declared solution presence ---
//
// A solution is present on a host because delivery declared it, not because its
// process announced itself. core's solutionhost package owns that declaration —
// SolutionHostBinding, its invariants and its conformance fixtures — and this
// file renders one per solution instance into the tree Argo already delivers.
// Nothing reconciles these documents yet, so rendering them changes no runtime
// behaviour; what it changes is that the record exists and is checked where it
// was authored.
//
// The document is carried in a ConfigMap rather than written as a bare
// solution-host-binding.codefly.yaml file. That is not a preference: the
// promotable render ruleset refuses any YAML in the owned tree that is not a
// Kubernetes manifest or a Kustomization (see decodeManifest — "Kubernetes
// manifest requires apiVersion and kind"), so a bare document cannot travel
// through this pipeline at all. A ConfigMap is also what "configuration the
// host can mount" means in the only delivery target the CLI has.
//
// The data key is core's own solutionhost.FileName, constant, and not the
// binding ID. A per-binding key would read better in a projected directory, but
// the key of a ConfigMap data map is a configuration name, and the render
// classifies those: an instance named "auth-gateway" or "session-store" would
// produce a key core calls credential-bearing and the render would refuse its
// own output. One document per ConfigMap, under a constant name, has no such
// edge.

// solutionHostBindingDir is the render subdirectory holding the rendered
// binding documents. It sits beside the unit directories and is not a unit, so
// it is not derived from unitDirectory.
//
// It carries the same "<dir>/overlays/<environment>" shape every other
// delivered path in the tree does, and for the same reason: an Argo
// Application is pointed at an overlay, so a manifest written anywhere else in
// the tree is committed to the delivery repository and never applied. The
// overlay holds a kustomization listing the documents, so the promotion's
// derived AppProject authority sees them the way it sees every other resource.
const solutionHostBindingDir = "solution-host-bindings"

// solutionHostBindingOverlay is the delivered overlay of a binding directory
// for one environment.
func solutionHostBindingOverlay(environment string) string {
	return filepath.ToSlash(filepath.Join(solutionHostBindingDir, "overlays", environment))
}

// solutionHostBindingLabel marks a ConfigMap that carries a binding document,
// so a host can select them across the namespaces solutions are isolated into
// instead of being handed a path. bindingLabel and solutionLabel carry the two
// identities a reconciler keys on.
const (
	solutionHostBindingLabel = "codefly.dev/document"
	solutionHostBindingKind  = "solution-host-binding"
	bindingLabel             = "codefly.dev/binding"
	solutionLabel            = "codefly.dev/solution"
	managedByLabel           = "app.kubernetes.io/managed-by"
	managedByCodefly         = "codefly"
	// kindConfigMap is the carrier's kind, named once so the manifest it writes
	// and the tests that read it back cannot drift apart.
	kindConfigMap = "ConfigMap"
)

// rfc1123Label is one lowercase DNS label. Two different things in this file
// must each be one, for two different reasons, and both are narrower than what
// core accepts:
//
//   - A route alias, because a host keys its registry on it as a single URL
//     path segment, and core's namePattern admits dots and slashes.
//   - Every part of a binding ID, because the ID becomes a Kubernetes object
//     name, and core's bindingPattern deliberately admits uppercase and
//     underscores so that a ULID or a UUID can be a binding ID.
//
// Refusing here is the whole point: both would otherwise sail through the
// render and the publish and fail only when ArgoCD applies the manifest — the
// late, opaque error solutionNamespace exists to prevent for namespaces.
var rfc1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// bindingIDMaxLength bounds a binding ID to one Kubernetes label value, which
// is stricter than core's 128. The ID is stamped as a label so a host can find
// the document that declares a binding; an ID that cannot be a label value
// would deliver a document nothing can select.
const bindingIDMaxLength = 63

// SolutionInstance is one solution instance of the resolved composition, as the
// render that delivers its workloads resolved it. Every field is something the
// render already holds: nothing here is re-resolved, and there is no second
// walk of the composition to disagree with the first.
type SolutionInstance struct {
	// Kind is whether the instance is a composed solution or a module. Both
	// are delivered and reconciled the same way; they differ in what may
	// install them, so the document states it rather than letting a host infer.
	Kind solutionhost.Kind
	// Name is the instance's name in the composition — the module name it is
	// composed under. Two instances of one solution are two composed modules
	// with two names, which is what makes their binding IDs distinct.
	Name string
	// Alias is the single route alias a solution instance claims on its host.
	// It is the host's registry key for the solution, so it is one path
	// segment. A module claims no route and leaves it empty.
	Alias string
	// Package and Version are the module package this instance resolved to.
	// Package is "<publisher>/<name>"; Version is an exact semantic version.
	Package string
	Version string
	// ReleaseDigest pins the release: the content digest of the module package
	// the composition materialized, or the artifact digest a packaged
	// solution's executor reports. It is a release digest and never an image
	// or a rendered-bytes digest.
	ReleaseDigest solutionhost.ReleaseDigest
	// Units are the rendered units this generation pins, by tree-relative path,
	// each with the principal its workloads present.
	Units []SolutionArtifactUnit
	// Endpoints are the endpoints the instance exposes, named and not addressed.
	Endpoints []SolutionEndpoint
	// Modules are the effective module pins this render resolved.
	Modules []SolutionModulePin
}

// SolutionArtifactUnit is one rendered unit of an instance: the name it is
// declared under, the tree-relative directory its manifests occupy, whose
// contents are hashed into the artifact's digest, and the principal the
// workloads it renders present. The document names the principal; it never
// carries the credential that proves it.
type SolutionArtifactUnit struct {
	Name    string
	Path    string
	Subject string
}

// SolutionEndpoint is one endpoint a solution instance exposes.
type SolutionEndpoint struct {
	Name       string
	Service    string
	Module     string
	API        string
	Visibility string
}

// SolutionModulePin is one effective module pin.
type SolutionModulePin struct {
	Module  string
	Package string
	Version string
}

// renderSolutionHostBindings renders one SolutionHostBinding per solution
// instance into the staged tree, admits the whole rendered set through core's
// Host before writing anything, and returns the tree-relative paths written.
//
// owned is the staged tree; destination is the tree this render replaces, which
// is where the previously delivered generation is read from. It runs before
// validateTree so the ConfigMaps it writes are held to the same promotable
// ruleset as everything else in the tree, and before buildInventory so they are
// hashed into the render digest like any other delivered file.
func renderSolutionHostBindings(owned, destination string, opts *RenderOptions) ([]DeclaredSolutionHostBinding, error) {
	if len(opts.SolutionInstances) == 0 {
		return nil, nil
	}
	if opts.Host == nil {
		// Declared presence names a host. Deriving one from the workspace or the
		// environment would produce a coordinate a host silently refuses at
		// reconcile time, far from the render that invented it — and rendering
		// the instances' workloads with no declaration at all would deliver
		// them present on no host, with only a warning saying so. An
		// environment that composes a module declares the host it runs on.
		names := make([]string, 0, len(opts.SolutionInstances))
		for index := range opts.SolutionInstances {
			names = append(names, opts.SolutionInstances[index].Name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("environment %s composes %s but declares no host block (coordinate, component, domain, audience, trust_domain, envelope_revision, delivery), so nothing declares them present anywhere; declare the host the environment runs on", opts.Environment, strings.Join(names, ", "))
	}
	if err := opts.Host.Validate(); err != nil {
		return nil, fmt.Errorf("environment %s host: %w", opts.Environment, err)
	}
	instances := append([]SolutionInstance(nil), opts.SolutionInstances...)
	sort.Slice(instances, func(i, j int) bool { return instances[i].Name < instances[j].Name })

	documents := make([]*solutionhost.SolutionHostBinding, 0, len(instances))
	for index := range instances {
		document, err := solutionHostBinding(owned, opts, &instances[index])
		if err != nil {
			return nil, err
		}
		// Provisional: publish settles the generation against the delivery
		// repository's base branch (see settlePresenceDelivery). The render
		// still reads the tree it replaces so a local re-render reports the
		// generation it would keep, but the destination is per module and per
		// render, so the number here is a hint and never the settled one.
		generation, err := nextGeneration(destination, opts.Environment, document)
		if err != nil {
			return nil, err
		}
		document.Generation = generation
		documents = append(documents, document)
	}
	// The renderer's share of admission, core's AdmitRendered: every check
	// that needs no host state runs over the parsed set, so an alias collision
	// is refused where it was authored. It takes no Host on purpose — a host's
	// Admit takes documents whose carrier was verified, and nothing here is
	// signed yet (signing happens at publish, and a local qualification
	// publish never signs). A refusal fails the render; a document a host
	// would reject is never written. One delivery speaks for one ownership
	// domain, and that is the renderer's check too: a host's mount is the
	// union of every delivery that reached it, so it cannot be refused there.
	if err := solutionhost.OneDelivery(documents...); err != nil {
		return nil, fmt.Errorf("rendered solution host bindings are not one delivery: %w", err)
	}
	if _, err := solutionhost.AdmitRendered(documents...); err != nil {
		return nil, fmt.Errorf("rendered solution host bindings are not admissible: %w", err)
	}
	written := make([]DeclaredSolutionHostBinding, 0, len(documents))
	names := make([]string, 0, len(documents)+1)
	for index, document := range documents {
		relative, err := writeSolutionHostBinding(owned, opts, document, instances[index].Alias)
		if err != nil {
			return nil, err
		}
		written = append(written, DeclaredSolutionHostBinding{
			Path: relative, Binding: document.Binding, Generation: document.Generation,
		})
		names = append(names, document.Binding+".yaml")
	}
	// The account the delivery Job runs as is rendered here, with the
	// documents; the Job itself is written at publish, once the documents are
	// settled and signed, because a Job that mounts a carrier no render has
	// produced would fail to start.
	account, err := renderDeliveryServiceAccount(filepath.Join(owned, filepath.FromSlash(solutionHostBindingOverlay(opts.Environment))), opts.Namespace)
	if err != nil {
		return nil, err
	}
	names = append(names, account)
	if err := writeSolutionHostBindingKustomization(owned, opts.Environment, names); err != nil {
		return nil, err
	}
	return written, nil
}

// solutionHostBinding builds one instance's document at generation 1. The
// generation is settled separately, against what was delivered before.
func solutionHostBinding(owned string, opts *RenderOptions, instance *SolutionInstance) (*solutionhost.SolutionHostBinding, error) {
	id, err := bindingID(opts.Workspace, opts.Environment, instance.Name)
	if err != nil {
		return nil, err
	}
	publisher, name, err := releaseIdentity(instance)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", instance.Kind, instance.Name, err)
	}
	if instance.ReleaseDigest == "" {
		return nil, fmt.Errorf("%s %s names no release digest; a generation without one can be matched to no authority", instance.Kind, instance.Name)
	}
	release := solutionhost.Release{Publisher: publisher, Name: name, Version: instance.Version, Digest: instance.ReleaseDigest}
	document := &solutionhost.SolutionHostBinding{
		Schema:     solutionhost.SchemaPresenceV2,
		Kind:       instance.Kind,
		Binding:    id,
		Generation: 1,
		// The domain this composition delivers under, stamped from the
		// environment: a host accepts delivery only from the domains it was
		// told about, and this is inside the signed bytes.
		OwnershipDomain:  opts.Host.Domain,
		EnvelopeRevision: opts.Host.EnvelopeRevision,
		Host:             solutionhost.HostTarget{Coordinate: opts.Host.Coordinate, Component: opts.Host.Component},
		Release:          release,
	}
	if instance.Kind == solutionhost.KindSolution {
		if !rfc1123Label.MatchString(instance.Alias) {
			return nil, fmt.Errorf(
				"solution %s claims the route alias %q; a host keys its registry on the alias as one URL path segment, so it must be lowercase letters, digits and dashes, starting and ending alphanumeric",
				instance.Name, instance.Alias)
		}
		// One route, surface backend: the alias fronts the workload artifacts
		// this generation pins, and core refuses a route to a surface the
		// generation renders nothing for. The CLI renders Kubernetes workloads
		// and nothing else, so backend is the only honest surface today.
		document.Routes = []solutionhost.Route{{Alias: instance.Alias, Surface: solutionhost.SurfaceBackend}}
	}
	for _, unit := range instance.Units {
		digest, digestErr := unitDigest(owned, unit.Path)
		if digestErr != nil {
			return nil, fmt.Errorf("%s %s artifact %s: %w", instance.Kind, instance.Name, unit.Name, digestErr)
		}
		document.Artifacts = append(document.Artifacts, solutionhost.Artifact{
			Surface: solutionhost.SurfaceBackend,
			Name:    unit.Name,
			Release: release.Identity(),
			Digest:  solutionhost.RenderedDigest(digest),
		})
		// A backend artifact runs workloads, and the host must know which
		// container of each authenticates, running which build, as whom.
		workloads, workloadErr := presenceWorkloads(owned, opts, unit)
		if workloadErr != nil {
			return nil, fmt.Errorf("%s %s: %w", instance.Kind, instance.Name, workloadErr)
		}
		document.Workloads = append(document.Workloads, workloads...)
	}
	for _, endpoint := range instance.Endpoints {
		document.Endpoints = append(document.Endpoints, solutionhost.Endpoint{
			Name: endpoint.Name, Service: endpoint.Service, Module: endpoint.Module,
			API: endpoint.API, Visibility: endpoint.Visibility,
		})
	}
	for _, pin := range instance.Modules {
		document.Modules = append(document.Modules, solutionhost.ModulePin{
			Module: pin.Module, Package: pin.Package, Version: pin.Version,
		})
	}
	// Validate here, before the generation is settled and before the set is
	// admitted, so a document that is wrong on its own is reported against the
	// instance that produced it rather than as "document 0" of a set.
	if err = document.Validate(); err != nil {
		return nil, fmt.Errorf("%s %s declares an invalid presence document: %w", instance.Kind, instance.Name, err)
	}
	return document, nil
}

// bindingID is the stable ID of one deployment instance: the workspace that
// composed it, the environment it is delivered to, and its instance name in the
// composition.
//
// It is derived from identity alone and from nothing that changes between
// renders, which is what makes it stable: no digest, no generation, no
// timestamp, no resolved address. The instance name is what distinguishes a
// second instance of the same solution — two instances are two composed modules
// under two names — and the workspace and environment are what keep two
// deliveries that land on one host from claiming the same binding.
func bindingID(workspace, environment, instance string) (string, error) {
	for _, part := range []struct{ label, value string }{
		{"workspace", workspace}, {"environment", environment}, {"solution", instance},
	} {
		if part.value == "" {
			return "", fmt.Errorf("solution host binding ID needs the %s name", part.label)
		}
		// Each part must be one lowercase DNS label. core's bindingPattern is
		// wider — it admits uppercase and underscores so a ULID or UUID can be
		// a binding ID — so core's own Validate accepts "My_Workspace.prod.crm"
		// and the render would deliver a ConfigMap named
		// "solution-host-binding-My_Workspace.prod.crm", which is not a legal
		// Kubernetes object name. Nothing downstream refuses that: the tree
		// validates, the publish succeeds, and ArgoCD fails at sync.
		if !rfc1123Label.MatchString(part.value) {
			return "", fmt.Errorf(
				"solution host binding ID cannot be built from %s %q: the ID names a Kubernetes object, so each part must be lowercase letters, digits and dashes, starting and ending alphanumeric",
				part.label, part.value)
		}
	}
	id := strings.Join([]string{workspace, environment, instance}, ".")
	if len(id) > bindingIDMaxLength {
		return "", fmt.Errorf("solution host binding ID %q is %d characters; it is stamped as a Kubernetes label value, so it must be at most %d", id, len(id), bindingIDMaxLength)
	}
	return id, nil
}

// releaseIdentity splits a module package ID ("<publisher>/<name>") into the
// two halves a release names. A package ID that is not of that shape is a
// refusal rather than a guess: Release.Identity() is what every artifact of the
// generation is checked against, so half a name there would make every artifact
// look like it came from another release.
func releaseIdentity(instance *SolutionInstance) (string, string, error) {
	publisher, name, found := strings.Cut(instance.Package, "/")
	if !found || publisher == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("module package %q is not <publisher>/<name>, so the release it declares cannot be named", instance.Package)
	}
	if instance.Version == "" {
		return "", "", fmt.Errorf("module package %s resolved to no version, so the release it declares cannot be pinned", instance.Package)
	}
	return publisher, name, nil
}

// unitDigest is the digest of one rendered unit's directory: every regular file
// under it, hashed in path order, in the same encoding buildInventory uses for
// the render digest. Two renders of the same desired state produce the same
// digest, and any change to any delivered byte of the unit changes it.
func unitDigest(root, relative string) (string, error) {
	unitRoot := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(unitRoot)
	if err != nil {
		return "", fmt.Errorf("inspect rendered artifact %s: %w", relative, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("rendered artifact %s is not a directory", relative)
	}
	type entry struct {
		path   string
		digest string
		size   int64
	}
	var entries []entry
	err = walkRegularFiles(unitRoot, func(path, name string, info os.FileInfo) error {
		file, openErr := os.Open(path)
		if openErr != nil {
			return fmt.Errorf("open %s: %w", name, openErr)
		}
		defer file.Close()
		hash := sha256.New()
		if _, copyErr := io.Copy(hash, file); copyErr != nil {
			return fmt.Errorf("hash %s: %w", name, copyErr)
		}
		entries = append(entries, entry{
			path: filepath.ToSlash(name), digest: "sha256:" + hex.EncodeToString(hash.Sum(nil)), size: info.Size(),
		})
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("rendered artifact %s holds no files", relative)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	hash := sha256.New()
	for _, item := range entries {
		fmt.Fprintf(hash, "%s\x00%s\x00%d\n", item.path, item.digest, item.size)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// nextGeneration settles the generation of a candidate document against the one
// previously delivered for the same binding ID.
//
// The prior generation is read from the previously delivered document, and not
// from a counter this CLI stores or from a value derived from the content:
//
//   - A content-derived generation is deterministic and a no-change re-render
//     reproduces it, but it is not ORDERED. core's Admit refuses a generation
//     lower than the applied one, and a hash is lower than its predecessor half
//     the time, so a correct render would be rejected as stale at random.
//   - A stored counter is ordered, but it is a second source of truth about what
//     was delivered. Two renderers (a developer and CI) keep two counters and
//     both produce generation N+1 with different contents, which core sees as a
//     rewritten generation — the exact accident ErrRewrittenGeneration exists to
//     catch. A counter also has to be seeded when it is first introduced, and
//     there is nothing to seed it from except the delivered document.
//   - The previously delivered document is ordered, is the only place the
//     current generation is actually recorded, and travels with the thing it
//     describes. The render already reads the tree it replaces (see the dev
//     deployments it reports cleared), so this is not a new dependency.
//
// A re-render with no change must not bump: the candidate is compared at the
// prior generation, through core's own Digest, so "unchanged" is decided by the
// same canonical encoding the host uses to tell a re-read from a rewrite. Equal
// means keep the generation and deliver byte-identical output; different means
// prior + 1.
//
// The failure mode this accepts is a render that cannot see the tree it
// replaces — a fresh checkout that has never pulled the delivery output. It
// emits generation 1, and a host that has applied a higher generation refuses
// it as stale. That is the right failure: it is loud, it is at the host, and it
// is recoverable by rendering from the delivered tree. Inventing a higher
// number to avoid it would be a renderer asserting a history it does not have.
func nextGeneration(destination, environment string, candidate *solutionhost.SolutionHostBinding) (uint64, error) {
	prior, err := priorSolutionHostBinding(destination, environment, candidate.Binding)
	if err != nil {
		return 0, err
	}
	if prior == nil {
		return 1, nil
	}
	at := *candidate
	at.Generation = prior.Generation
	current, err := at.Digest()
	if err != nil {
		return 0, err
	}
	delivered, err := prior.Digest()
	if err != nil {
		return 0, fmt.Errorf("digest the delivered solution host binding %q: %w", candidate.Binding, err)
	}
	if current == delivered {
		return prior.Generation, nil
	}
	return prior.Generation + 1, nil
}

// priorSolutionHostBinding reads the binding document the tree being replaced
// delivered for this binding ID, or nil when it delivered none.
//
// An unreadable document is an error, never a silent fall back to generation 1:
// resetting the generation would deliver a document every host that has applied
// this binding refuses, and it would do so without saying anything.
func priorSolutionHostBinding(destination, environment, binding string) (*solutionhost.SolutionHostBinding, error) {
	path := filepath.Join(destination, filepath.FromSlash(solutionHostBindingOverlay(environment)), binding+".yaml")
	data, err := os.ReadFile(path) //nolint:gosec // a path derived from the render destination and a validated binding ID
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the delivered solution host binding %q: %w", binding, err)
	}
	var carrier solutionHostBindingConfigMap
	if err = yaml.Unmarshal(data, &carrier); err != nil {
		return nil, fmt.Errorf("decode the delivered solution host binding %q: %w", binding, err)
	}
	document, held := carrier.Data[solutionhost.FileName]
	if !held {
		return nil, fmt.Errorf("the delivered solution host binding %q carries no %s; the generation cannot be advanced from it", binding, solutionhost.FileName)
	}
	parsed, err := solutionhost.Parse([]byte(document))
	if err != nil {
		return nil, fmt.Errorf("the delivered solution host binding %q is not a document this Core reads: %w", binding, err)
	}
	if parsed.Binding != binding {
		return nil, fmt.Errorf("the delivered document at %s declares binding %q, not %q", filepath.ToSlash(filepath.Join(solutionHostBindingOverlay(environment), binding+".yaml")), parsed.Binding, binding)
	}
	return parsed, nil
}

// solutionHostBindingConfigMap is the ConfigMap that carries one binding
// document. It is written through a struct rather than a map so the delivered
// YAML has a stable field order and a re-render with no change produces
// identical bytes.
type solutionHostBindingConfigMap struct {
	APIVersion string                           `yaml:"apiVersion"`
	Kind       string                           `yaml:"kind"`
	Metadata   solutionHostBindingConfigMapMeta `yaml:"metadata"`
	Data       map[string]string                `yaml:"data"`
}

type solutionHostBindingConfigMapMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

// writeSolutionHostBinding marshals a validated document into its ConfigMap and
// writes it into the staged tree, returning the tree-relative path.
func writeSolutionHostBinding(owned string, opts *RenderOptions, document *solutionhost.SolutionHostBinding, alias string) (string, error) {
	// Marshal through core so an invalid document is never written; Admit has
	// already validated it, and this keeps that guarantee at the write itself.
	encoded, err := solutionhost.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("marshal solution host binding %q: %w", document.Binding, err)
	}
	labels := map[string]string{
		managedByLabel:           managedByCodefly,
		solutionHostBindingLabel: solutionHostBindingKind,
		bindingLabel:             document.Binding,
	}
	// A solution is selected by its route alias; a module claims none.
	if alias != "" {
		labels[solutionLabel] = alias
	}
	carrier := solutionHostBindingConfigMap{
		APIVersion: "v1",
		Kind:       kindConfigMap,
		Metadata: solutionHostBindingConfigMapMeta{
			Name:      solutionHostBindingKind + "-" + document.Binding,
			Namespace: opts.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{solutionhost.FileName: string(encoded)},
	}
	body, err := yaml.Marshal(carrier)
	if err != nil {
		return "", fmt.Errorf("encode solution host binding %q: %w", document.Binding, err)
	}
	overlay := solutionHostBindingOverlay(opts.Environment)
	directory := filepath.Join(owned, filepath.FromSlash(overlay))
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return "", fmt.Errorf("create solution host binding directory: %w", err)
	}
	relative := filepath.ToSlash(filepath.Join(overlay, document.Binding+".yaml"))
	if err = os.WriteFile(filepath.Join(directory, document.Binding+".yaml"), body, 0o644); err != nil { //nolint:gosec // a delivered manifest, readable beside the rest of the tree
		return "", fmt.Errorf("write solution host binding %q: %w", document.Binding, err)
	}
	return relative, nil
}

// writeSolutionHostBindingKustomization lists the delivered documents so the
// overlay builds. Argo applies what the kustomization names, and the
// promotion's derived AppProject authority reads the same build, so a document
// missing from here is delivered to the repository and to nothing else.
func writeSolutionHostBindingKustomization(owned, environment string, names []string) error {
	sort.Strings(names)
	body, err := yaml.Marshal(kustomizationManifest{
		APIVersion: kustomizeAPIVersion,
		Kind:       kindKustomization,
		Resources:  names,
	})
	if err != nil {
		return fmt.Errorf("encode solution host binding kustomization: %w", err)
	}
	path := filepath.Join(owned, filepath.FromSlash(solutionHostBindingOverlay(environment)), "kustomization.yaml")
	if err = os.WriteFile(path, body, 0o644); err != nil { //nolint:gosec // a delivered manifest, readable beside the rest of the tree
		return fmt.Errorf("write solution host binding kustomization: %w", err)
	}
	return nil
}

// deliveredBindingPath is the render subdirectory the inventory records, derived
// from what this render actually wrote.
//
// It is computed unconditionally, and that is the point. Setting the field only
// when a binding was written leaves a previous render's value in place on a
// reused RenderOptions, and the inventory then claims a delivery path the tree
// does not have. Both readers of that field — generateArgoBootstrap and
// snapshotAuthority — kustomize-build "<path>/overlays/<environment>", so a
// stale value does not degrade to the old behaviour: it refuses the whole
// publication with "build promotion source ...: must build at directory".
func deliveredBindingPath(bindings []DeclaredSolutionHostBinding) string {
	if len(bindings) == 0 {
		return ""
	}
	return solutionHostBindingDir
}

// presenceInstanceOf resolves the instance a module render declares the
// presence of: a composed solution when the module ships a solution manifest,
// the module itself otherwise. It returns the reason when the module declares
// no presence, so the render can say so rather than leave an absence that
// looks like a composition with nothing in it.
//
// A composed solution IS a module: it is rendered by the same module path its
// services are, so the instance is read off the module already in hand — the
// solution manifest it ships, the services the render loaded from it, the
// package it resolved, and the units it assembled. Nothing is looked up again,
// so the document cannot describe a composition the workloads did not come from.
//
// The instance's name, and therefore a solution's route alias, is the name it
// is composed under. That is what makes a second instance of one solution
// distinct: two instances are two composed modules with two names, delivering
// two trees.
func presenceInstanceOf(
	module *resources.Module,
	services []*resources.Service,
	env *environments.Environment,
	opts *RenderOptions,
) (*SolutionInstance, string, error) {
	declared, err := solutionrun.ModuleSolution(module)
	if err != nil {
		return nil, "", fmt.Errorf("read solution manifest of module %s: %w", module.Name, err)
	}
	if opts.Package == nil {
		return nil, fmt.Sprintf("module %s has no package manifest, so it names no release and declares no presence", module.Name), nil
	}
	instance := &SolutionInstance{Kind: solutionhost.KindModule, Name: module.Name}
	if declared != nil {
		instance.Kind, instance.Alias = solutionhost.KindSolution, module.Name
	}
	instance.Package, instance.Version = opts.Package.ID, opts.Package.Version
	instance.Modules = []SolutionModulePin{{Module: module.Name, Package: opts.Package.ID, Version: opts.Package.Version}}
	if instance.ReleaseDigest, err = releaseDigest(module.Dir()); err != nil {
		return nil, "", fmt.Errorf("module %s: %w", module.Name, err)
	}
	for _, unit := range opts.Units {
		// A managed service rendered at most a bootstrap bundle: a Job that
		// prepares the managed resource, mints under no principal and is the
		// cell's to admit, never a presence artifact. One with no path has no
		// delivered bytes at all.
		if unit.Path == "" || unit.Managed {
			continue
		}
		entry := SolutionArtifactUnit{Name: unit.Name, Path: unit.Path}
		if identity := env.WorkloadIdentity(unit.Name); identity != nil {
			entry.Subject = identity.Principal
		}
		instance.Units = append(instance.Units, entry)
	}
	for _, service := range services {
		for _, endpoint := range service.Endpoints {
			if endpoint == nil {
				continue
			}
			if endpoint.API == "" {
				return nil, "", fmt.Errorf(
					"%s %s service %s endpoint %s declares no api; a presence document declares the endpoints the host exposes and cannot name one whose protocol is unstated",
					instance.Kind, module.Name, service.Name, endpoint.Name)
			}
			instance.Endpoints = append(instance.Endpoints, SolutionEndpoint{
				Name: endpoint.Name, Service: service.Name, Module: module.Name,
				API: endpoint.API, Visibility: endpoint.Visibility,
			})
		}
	}
	return instance, "", nil
}
