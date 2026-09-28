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

// routeAliasPattern is narrower than core's namePattern on purpose. core admits
// a dotted or slashed alias; a host keys its registry on the alias as a single
// URL path segment, and a slashed alias cannot be one. Rendering an alias the
// host would have to map is worse than refusing it here, where the composition
// that authored it is in hand.
var routeAliasPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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
	// Name is the instance's name in the composition — the module name it is
	// composed under. Two instances of one solution are two composed modules
	// with two names, which is what makes their binding IDs distinct.
	Name string
	// Alias is the single route alias this instance claims on its host. It is
	// the host's registry key for the solution, so it is one path segment.
	Alias string
	// Package and Version are the module package this instance resolved to.
	// Package is "<publisher>/<name>"; Version is an exact semantic version.
	Package string
	Version string
	// Subject is the principal this instance's workload presents, as the
	// environment resolved it. The document names it; it never carries the
	// credential that proves it.
	Subject string
	// Units are the rendered units this generation pins, by tree-relative path.
	Units []SolutionArtifactUnit
	// Endpoints are the endpoints the instance exposes, named and not addressed.
	Endpoints []SolutionEndpoint
	// Modules are the effective module pins this render resolved.
	Modules []SolutionModulePin
}

// SolutionArtifactUnit is one rendered unit of a solution instance: the name it
// is declared under and the tree-relative directory its manifests occupy, whose
// contents are hashed into the artifact's digest.
type SolutionArtifactUnit struct {
	Name string
	Path string
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
func renderSolutionHostBindings(owned, destination string, opts *RenderOptions) ([]string, error) {
	if len(opts.SolutionInstances) == 0 {
		return nil, nil
	}
	if opts.Host == nil {
		// Declared presence names a host. Deriving one from the workspace or the
		// environment would produce a coordinate a host silently refuses at
		// reconcile time, far from the render that invented it.
		return nil, nil
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
		generation, err := nextGeneration(destination, opts.Environment, document)
		if err != nil {
			return nil, err
		}
		document.Generation = generation
		documents = append(documents, document)
	}
	// The renderer's view of the host: no coordinate and nothing applied, so
	// every check that does not need host state still runs and an alias
	// collision is refused where it was authored. A refusal fails the render;
	// a document a host would reject is never written.
	if _, err := (solutionhost.Host{}).Admit(documents...); err != nil {
		return nil, fmt.Errorf("rendered solution host bindings are not admissible: %w", err)
	}
	written := make([]string, 0, len(documents))
	names := make([]string, 0, len(documents))
	for index, document := range documents {
		relative, err := writeSolutionHostBinding(owned, opts, document, instances[index].Alias)
		if err != nil {
			return nil, err
		}
		written = append(written, relative)
		names = append(names, document.Binding+".yaml")
	}
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
		return nil, fmt.Errorf("solution %s: %w", instance.Name, err)
	}
	if !routeAliasPattern.MatchString(instance.Alias) {
		return nil, fmt.Errorf(
			"solution %s claims the route alias %q; a host keys its registry on the alias as one URL path segment, so it must be lowercase letters, digits and dashes, starting and ending alphanumeric",
			instance.Name, instance.Alias)
	}
	if instance.Subject == "" {
		return nil, fmt.Errorf(
			"solution %s declares no workload identity in environment %s; a binding names the principal the host must expect, and this workload authenticates as nothing (declare service-identity for %s)",
			instance.Name, opts.Environment, instance.Name)
	}
	release := solutionhost.Release{
		Publisher: publisher, Name: name, Version: instance.Version,
		// Empty in v1, deliberately. Signed releases do not exist yet, and a
		// digest invented here would pin nothing while looking like a pin.
		Digest: "",
	}
	document := &solutionhost.SolutionHostBinding{
		Schema:     solutionhost.SchemaV1,
		Binding:    id,
		Generation: 1,
		Host:       solutionhost.HostTarget{Coordinate: opts.Host.Coordinate, Component: opts.Host.Component},
		Release:    release,
		// One route, surface backend: the alias fronts the workload artifacts
		// this generation pins, and core refuses a route to a surface the
		// generation renders nothing for. The CLI renders Kubernetes workloads
		// and nothing else, so backend is the only honest surface today.
		Routes:   []solutionhost.Route{{Alias: instance.Alias, Surface: solutionhost.SurfaceBackend}},
		Workload: solutionhost.WorkloadIdentity{Audience: opts.Host.Audience, Subject: instance.Subject},
	}
	for _, unit := range instance.Units {
		digest, digestErr := unitDigest(owned, unit.Path)
		if digestErr != nil {
			return nil, fmt.Errorf("solution %s artifact %s: %w", instance.Name, unit.Name, digestErr)
		}
		document.Artifacts = append(document.Artifacts, solutionhost.Artifact{
			Surface: solutionhost.SurfaceBackend,
			Name:    unit.Name,
			Release: release.Identity(),
			Digest:  digest,
		})
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
	// solution that produced it rather than as "document 0" of a set.
	if err = document.Validate(); err != nil {
		return nil, fmt.Errorf("solution %s declares an invalid host binding: %w", instance.Name, err)
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
		if strings.ContainsAny(part.value, "./ ") {
			return "", fmt.Errorf("solution host binding ID cannot be built from %s %q: it joins its parts on \".\"", part.label, part.value)
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
	carrier := solutionHostBindingConfigMap{
		APIVersion: "v1",
		Kind:       kindConfigMap,
		Metadata: solutionHostBindingConfigMapMeta{
			Name:      solutionHostBindingKind + "-" + document.Binding,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				managedByLabel:           managedByCodefly,
				solutionHostBindingLabel: solutionHostBindingKind,
				bindingLabel:             document.Binding,
				solutionLabel:            alias,
			},
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

// undeclaredSolutions names the solution instances this render delivered
// workloads for without declaring a binding, because the environment names no
// host. An empty SolutionHostBindings list alone cannot say that: a composition
// with no solution produces the same empty list.
func undeclaredSolutions(opts *RenderOptions) []string {
	if opts.Host != nil || len(opts.SolutionInstances) == 0 {
		return nil
	}
	names := make([]string, 0, len(opts.SolutionInstances))
	for index := range opts.SolutionInstances {
		names = append(names, opts.SolutionInstances[index].Name)
	}
	sort.Strings(names)
	return names
}

// solutionInstanceOf resolves the solution instance a module render delivers
// the workloads of, or nil when the module is not one.
//
// A composed solution IS a module: it is rendered by the same module path its
// services are, so the instance is read off the module already in hand — the
// solution manifest it ships, the services the render loaded from it, the
// package it resolved, and the units it assembled. A module that ships no
// solution manifest is not a solution instance and declares nothing.
//
// The instance's name, and therefore its route alias, is the name it is
// composed under. That is what makes a second instance of one solution
// distinct: two instances are two composed modules with two names, delivering
// two trees.
func solutionInstanceOf(
	module *resources.Module,
	services []*resources.Service,
	env *environments.Environment,
	opts *RenderOptions,
) (*SolutionInstance, error) {
	declared, err := solutionrun.ModuleSolution(module)
	if err != nil {
		return nil, fmt.Errorf("read solution manifest of module %s: %w", module.Name, err)
	}
	if declared == nil {
		return nil, nil
	}
	instance := &SolutionInstance{Name: module.Name, Alias: module.Name}
	if opts.Package != nil {
		instance.Package, instance.Version = opts.Package.ID, opts.Package.Version
		instance.Modules = []SolutionModulePin{{
			Module: module.Name, Package: opts.Package.ID, Version: opts.Package.Version,
		}}
	}
	// The entry service is the one that runs the solution, so its principal is
	// what the host must expect the workload to present. A module with no entry
	// has no one workload to name.
	if module.ServiceEntry != "" {
		if identity := env.WorkloadIdentity(module.ServiceEntry); identity != nil {
			instance.Subject = identity.Principal
		}
	}
	for _, unit := range opts.Units {
		// A managed service that rendered no bootstrap bundle has no path and
		// no delivered bytes, so there is nothing to pin a digest to.
		if unit.Path == "" {
			continue
		}
		instance.Units = append(instance.Units, SolutionArtifactUnit{Name: unit.Name, Path: unit.Path})
	}
	for _, service := range services {
		for _, endpoint := range service.Endpoints {
			if endpoint == nil {
				continue
			}
			if endpoint.API == "" {
				return nil, fmt.Errorf(
					"solution %s service %s endpoint %s declares no api; a binding declares the endpoints the host exposes and cannot name one whose protocol is unstated",
					module.Name, service.Name, endpoint.Name)
			}
			instance.Endpoints = append(instance.Endpoints, SolutionEndpoint{
				Name: endpoint.Name, Service: service.Name, Module: module.Name,
				API: endpoint.API, Visibility: endpoint.Visibility,
			})
		}
	}
	return instance, nil
}
