package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	solutionmanifest "github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"gopkg.in/yaml.v3"
)

// --- Declared authority ---
//
// A module holds authority on a host because a reviewed, signed document
// granted it to one exact build — never because its process announced itself,
// and never for a build other than the one approved. The render derives that
// document from the module's published contract (module.contract.codefly.yaml):
// the contract is the module's REQUEST, the host's envelope is the ceiling a
// platform administrator writes at runtime, and the authority document is what
// the render derives and the publish pipeline signs. The render does not read
// the envelope: the host holds the document against it at apply, and a request
// wider than the ceiling is refused there, with the binding named.
//
// One document per module-identity service. An authority document is effective
// for ONE approved build — the OCI image manifest digest of the container that
// authenticates — so a module whose two services each mint under the module's
// principal runs two images and holds two documents. The approved build is read
// off the presence workloads of the same render, which is the only place the
// relationship between a service and the image it runs is verified.
//
// The documents are delivered to the platform's authority namespace, which only
// the delivery pipeline may create Jobs in: signed AND isolated, the two halves
// of the writer-trust decision.

const (
	// solutionAuthorityDir is the render subdirectory holding the authority
	// documents, beside the presence documents and the units.
	solutionAuthorityDir = "solution-authority"

	solutionAuthorityKind = "solution-authority"
)

// authorityID names the one authority document granted over a binding. There
// is one per binding and it is named after the binding, because that is what
// the host folds on: a withdrawal over a binding is terminal for every later
// authority ID over it, so an ID that changed with the service presenting the
// authority would turn a module moving its identity to another service into a
// withdrawal nothing can reinstate. Named after the binding, that move is a
// new generation approving a new build.
func authorityID(binding string) string {
	return binding + "-authority"
}

// solutionAuthorityOverlay is the delivered overlay of the authority directory
// for one environment.
func solutionAuthorityOverlay(environment string) string {
	return filepath.ToSlash(filepath.Join(solutionAuthorityDir, "overlays", environment))
}

// AuthorityInstance is one authority document's input: the module and the
// module-identity service it grants to, the unit that renders that service,
// and the module's resolved contract.
type AuthorityInstance struct {
	Module   string
	Service  string
	Unit     SolutionArtifactUnit
	Contract *modulecontract.Resolved
}

// DeclaredAuthority is one rendered authority document: where it was written,
// which authority it declares, for which build.
type DeclaredAuthority struct {
	Path      string `json:"path"`
	Authority string `json:"authority"`
	Build     string `json:"build"`
}

// workspaceValues is the provider the render resolves contract slots from:
// the workspace configurations the environment provides, enumerated per group
// exactly as supplied — every record, in both spellings of a key, a key
// supplied twice, a key classified public here and secret there — and nothing
// decided. Normalisation, ambiguity and secret precedence are core's resolution
// rules, once (modulecontract.Values), so two renderers cannot read one
// composition into two authority documents; core's resolution kit fails a
// provider that selects.
type workspaceValues struct {
	provided *configurations.WorkspaceConfigurations
}

// Records implements modulecontract.Values: a group's records as the
// composition supplies them, across every information that contributes to
// the group.
func (values workspaceValues) Records(group string) ([]modulecontract.Record, error) {
	if values.provided == nil {
		return nil, nil
	}
	var records []modulecontract.Record
	for _, info := range values.provided.Infos {
		if info.GetName() != group {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			records = append(records, modulecontract.Record{Key: value.GetKey(), Value: value.GetValue(), Secret: value.GetSecret()})
		}
	}
	return records, nil
}

// authorityInstancesOf resolves the authority document a module render
// declares: none when the module publishes no contract, with the reason, or
// one for the service declaring module-identity, with the contract's slots
// resolved from the environment's public configuration.
//
// One service, because one authority document approves one build and the host
// holds one authority record per binding: two services each claiming to be the
// module's identity would be two executions under one principal, of which the
// host could activate at most one, and the render is where the module can be
// told which to keep.
func authorityInstancesOf(
	ctx context.Context,
	workspace *resources.Workspace,
	module *resources.Module,
	services []*resources.Service,
	env *environments.Environment,
	units []SolutionArtifactUnit,
	endpoints []SolutionEndpoint,
) ([]AuthorityInstance, string, error) {
	request, err := authorityRequestOf(module)
	if err != nil {
		return nil, "", err
	}
	if request.undeclared != "" {
		return nil, request.undeclared, nil
	}
	var identities []*resources.Service
	for _, service := range services {
		if service.ModuleIdentity {
			identities = append(identities, service)
		}
	}
	if len(identities) == 0 {
		// The request is the module's ask for authority. With no service
		// declaring module-identity nothing would present it, and the request
		// would be dropped rather than refused: the composition is inconsistent
		// and is told so here, before slot resolution or any document.
		return nil, "", fmt.Errorf("module %s %s, which asks for authority, but no service declares module-identity, so nothing would present it; declare module-identity on the service that presents the module's identity, or withdraw the request", module.Name, request.source)
	}
	if len(identities) > 1 {
		names := make([]string, 0, len(identities))
		for _, service := range identities {
			names = append(names, service.Name)
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("module %s declares module-identity on the services %s; a module presents its authority from one service, because its one authority document approves one build and the host keeps one authority record per binding, so keep module-identity on the service that authenticates as the module", module.Name, strings.Join(names, ", "))
	}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime())
	if err != nil {
		return nil, "", fmt.Errorf("read the workspace configurations the contract of %s resolves against: %w", module.Name, err)
	}
	resolved, err := request.contract.Resolve(workspaceValues{provided: provided})
	if err != nil {
		return nil, "", fmt.Errorf("module %s: %w", module.Name, err)
	}
	if err := refuseUnaddressableConsumption(workspace, module, request, resolved); err != nil {
		return nil, "", err
	}
	// The principal is the module's own name — the contract says so of
	// itself — and it is held to that here, because the contract is written in
	// the module's repository: a module claiming another module's principal
	// would claim that principal's bindings, and nothing downstream ties a
	// principal to the module presenting it.
	if resolved.Principal != module.Name {
		return nil, "", fmt.Errorf("module %s publishes a contract whose principal is %q; the principal every credential of a module is issued to is the module's own name, so the contract declares principal: %s", module.Name, resolved.Principal, module.Name)
	}
	if err := refuseUnrenderedDestinations(module.Name, resolved, endpoints); err != nil {
		return nil, "", err
	}
	instances := make([]AuthorityInstance, 0, len(identities))
	for _, service := range identities {
		index := -1
		for position := range units {
			if units[position].Name == service.Name {
				index = position
				break
			}
		}
		if index < 0 {
			return nil, "", fmt.Errorf("module %s service %s declares module-identity but this render delivers no unit for it, so there is no build to approve", module.Name, service.Name)
		}
		instances = append(instances, AuthorityInstance{Module: module.Name, Service: service.Name, Unit: units[index], Contract: resolved})
	}
	return instances, "", nil
}

// authorityRequest is what a module asks for authority with, from exactly one
// source: its published module contract, or — for a composed solution, which
// declares what it consumes in its solution manifest — the scopes its
// api.consumes entries ask for. undeclared carries the reason when it asks for
// none; source says which file asked, for a refusal.
type authorityRequest struct {
	contract *modulecontract.Contract
	// consumed is the scoped api.consumes entries the contract was built
	// from, when the request is a solution's consumption; nil for a module's
	// own contract.
	consumed   []solutionmanifest.APIDeclaration
	source     string
	undeclared string
}

// authorityRequestOf reads a module's request for authority, as a module
// contract either way: the one it publishes, or the one a composed solution's
// scoped api.consumes amounts to (core's ConsumptionContract). The caller
// resolves both through the same Resolve, so a solution's audience comes from
// the composition exactly as a module's does.
//
// The solution manifest is read through core's LoadConsumption: the schema
// version and api.consumes, strictly, and nothing else. An invalid or unknown
// field in a consumption fails the render; a section with nothing to do with
// consumption — an agent block, a section this core does not model — never
// does, because a render of a solution that asks for no authority must not fail
// on what an authority read does not read. The run path's lenient read is
// unchanged.
//
// Both sources at once is refused, by name. A solution that is also consumed
// may publish a contract for its destinations; merging its bindings with a
// derived consumption is a decision about two declarations of one request that
// nothing has made yet, so for now it declares its consumption in one place.
func authorityRequestOf(module *resources.Module) (authorityRequest, error) {
	contract, err := modulecontract.Load(module.Dir())
	switch {
	case errors.Is(err, os.ErrNotExist):
		contract = nil
	case err != nil:
		return authorityRequest{}, err
	}
	var consumption *modulecontract.Contract
	var consumed []solutionmanifest.APIDeclaration
	data, err := os.ReadFile(filepath.Join(module.Dir(), solutionmanifest.FileName))
	switch {
	case module.Dir() == "":
		// A module constructed rather than loaded has nowhere to ship a
		// manifest; reading the working directory instead would be a guess.
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return authorityRequest{}, fmt.Errorf("read the solution manifest of module %s: %w", module.Name, err)
	default:
		declared, loadErr := solutionmanifest.LoadConsumption(data)
		if loadErr != nil {
			return authorityRequest{}, fmt.Errorf("module %s: the api.consumes an authority document is derived from: %w", module.Name, loadErr)
		}
		// A composed solution is a module, and its principal is the name it is
		// composed under, exactly as a module contract's principal must be.
		if consumption, err = declared.ConsumptionContract(module.Name); err != nil {
			return authorityRequest{}, fmt.Errorf("module %s: %w", module.Name, err)
		}
		for index := range declared.API.Consumes {
			if len(declared.API.Consumes[index].Scopes) > 0 {
				consumed = append(consumed, declared.API.Consumes[index])
			}
		}
	}
	switch {
	case contract != nil && consumption != nil && len(consumption.Bindings) > 0:
		return authorityRequest{}, fmt.Errorf("module %s publishes %s and its %s asks for scopes on api.consumes; a module asks for authority from one declaration, so declare the consumption in one of them", module.Name, modulecontract.FileName, solutionmanifest.FileName)
	case contract != nil:
		return authorityRequest{contract: contract, source: "publishes " + modulecontract.FileName}, nil
	case consumption != nil:
		source := "declares admission in its " + solutionmanifest.FileName
		if len(consumption.Bindings) > 0 {
			source = "asks for scopes on api.consumes in its " + solutionmanifest.FileName
		}
		return authorityRequest{contract: consumption, consumed: consumed, source: source}, nil
	}
	return authorityRequest{undeclared: fmt.Sprintf("module %s publishes no %s and asks for no scopes on api.consumes, so it asks for no authority", module.Name, modulecontract.FileName)}, nil
}

// refuseUnaddressableConsumption refuses a solution's request the host could
// never install, here, where its author reads the reason: a scoped entry for a
// module the composition does not compose, and one addressed to the solution
// itself — by name, or by a resolved audience that is its own principal — which
// the host omits from its registry with only a logged reason.
func refuseUnaddressableConsumption(workspace *resources.Workspace, module *resources.Module, request authorityRequest, resolved *modulecontract.Resolved) error {
	if request.consumed == nil {
		return nil
	}
	composed := map[string]bool{}
	for _, reference := range workspace.Modules {
		if reference != nil {
			composed[reference.Name] = true
		}
	}
	for index := range request.consumed {
		declaration := &request.consumed[index]
		if declaration.Module == module.Name {
			return fmt.Errorf("module %s asks for scopes at itself on api.consumes entry %s; a credential is never addressed to its own principal, and the host would never install the binding", module.Name, declaration.ID)
		}
		if !composed[declaration.Module] {
			return fmt.Errorf("module %s asks for scopes at module %s on api.consumes entry %s, which this workspace does not compose; a binding for a module nothing composes can never be installed", module.Name, declaration.Module, declaration.ID)
		}
	}
	for _, binding := range resolved.Bindings {
		if binding.Audience == resolved.Principal {
			return fmt.Errorf("module %s asks for scopes on api.consumes entry %s at audience %q, which is its own principal; a credential is never addressed to its own principal, and the host would never install the binding", module.Name, binding.ID, binding.Audience)
		}
	}
	return nil
}

// renderAuthorityDocuments renders the module's authority document into the
// staged tree, approving the build its presenting service's workloads run, and
// returns the tree-relative paths written. A render is one module, and a module
// has one binding and so one authority: two instances would be two documents
// under one ID, the second silently overwriting the first, so the pair is
// refused here as it is where the instances are derived.
func renderAuthorityDocuments(owned string, opts *RenderOptions) ([]DeclaredAuthority, error) {
	tree, err := inspectRenderRoot(owned, opts)
	if err != nil {
		return nil, err
	}
	defer releaseRenderRoot(tree, opts)

	if len(opts.AuthorityInstances) == 0 || opts.Host == nil {
		return nil, nil
	}
	instances := append([]AuthorityInstance(nil), opts.AuthorityInstances...)
	sort.Slice(instances, func(i, j int) bool { return instances[i].Service < instances[j].Service })
	if len(instances) > 1 {
		services := make([]string, 0, len(instances))
		for _, instance := range instances {
			services = append(services, instance.Module+"/"+instance.Service)
		}
		return nil, fmt.Errorf("the render names %s as presenting an authority, and a module presents its one authority document from one service", strings.Join(services, ", "))
	}
	directory := filepath.Join(owned, filepath.FromSlash(solutionAuthorityOverlay(opts.Environment)))
	if err := tree.mkdirAll(directory); err != nil {
		return nil, fmt.Errorf("create authority directory: %w", err)
	}
	declared := make([]DeclaredAuthority, 0, len(instances))
	names := make([]string, 0, len(instances))
	for index := range instances {
		document, err := authorityDocument(owned, opts, &instances[index])
		if err != nil {
			return nil, err
		}
		file, err := writeAuthorityDocumentInTree(tree, directory, document, instances[index].Module)
		if err != nil {
			return nil, err
		}
		declared = append(declared, DeclaredAuthority{
			Path: filepath.ToSlash(filepath.Join(solutionAuthorityOverlay(opts.Environment), file)), Authority: document.Authority, Build: string(document.ApprovedBuild),
		})
		names = append(names, file)
	}
	if err := writeAuthorityKustomizationInTree(tree, owned, opts.Environment, names); err != nil {
		return nil, err
	}
	return declared, nil
}

// authorityDocument derives one instance's document at generation 1, effective
// from presence generation 1. Publish settles both against what was delivered.
func authorityDocument(owned string, opts *RenderOptions, instance *AuthorityInstance) (*solutionhost.AuthorityDocument, error) {
	binding, err := bindingID(opts.Workspace, opts.Environment, instance.Module)
	if err != nil {
		return nil, err
	}
	build, err := servingBuild(owned, opts, instance.Unit)
	if err != nil {
		return nil, fmt.Errorf("authority of %s/%s: %w", instance.Module, instance.Service, err)
	}
	document := &solutionhost.AuthorityDocument{
		Schema:    solutionhost.SchemaAuthorityV1,
		Authority: authorityID(binding),
		// Granted over exactly this instance's presence binding, and over no
		// other: without the target an authority document activated any
		// binding on the host and domain running the same image, a replacement
		// that took a withdrawn alias included.
		PresenceBinding:  binding,
		Generation:       1,
		Host:             solutionhost.HostTarget{Coordinate: opts.Host.Coordinate, Component: opts.Host.Component},
		OwnershipDomain:  opts.Host.Domain,
		EnvelopeRevision: opts.Host.EnvelopeRevision,
		ApprovedBuild:    build,
		EffectiveFrom:    1,
		Principals:       []solutionhost.PrincipalAuthority{{Principal: instance.Contract.Principal, Bindings: authorityBindings(binding, instance.Contract)}},
		// The SUBJECT module's own declarations, written out rather than left
		// to be inferred from the bindings above: the bindings carry at most
		// ONE queue and ONE namespace each, so a module declaring several
		// cannot be reconstructed from them, and a module declaring none is
		// indistinguishable from a renderer that dropped the field. Core
		// requires all three for exactly that reason and takes [] — not an
		// absent key — as "the module declares none", so these are always
		// non-nil.
		Queues:        declaredStrings(instance.Contract.Queues),
		Namespaces:    declaredStrings(instance.Contract.Namespaces),
		ScopeCeilings: declaredScopeCeilings(instance.Contract.ScopeCeilings),
	}
	if err := document.Validate(); err != nil {
		return nil, fmt.Errorf("authority of %s/%s is invalid: %w", instance.Module, instance.Service, err)
	}
	return document, nil
}

// declaredStrings projects a subject module's declared list for an authority
// document. It never returns nil: core distinguishes "the module declares
// none" ([]) from "the renderer dropped the field" (absent), and only the
// first is a document it will accept.
func declaredStrings(declared []string) []string {
	projected := make([]string, 0, len(declared))
	return append(projected, declared...)
}

// declaredScopeCeilings projects the subject module's own permission
// vocabulary. The two types are field-identical and live in different
// packages, so this is a copy rather than a conversion, and it is non-nil for
// the same reason as declaredStrings.
func declaredScopeCeilings(declared []modulecontract.ScopeCeiling) []solutionhost.ScopeCeiling {
	projected := make([]solutionhost.ScopeCeiling, 0, len(declared))
	for _, ceiling := range declared {
		projected = append(projected, solutionhost.ScopeCeiling{
			ResourceKind: ceiling.ResourceKind,
			Actions:      declaredStrings(ceiling.Actions),
		})
	}
	return projected
}

// authorityBindings derives the units of authority a contract asks for: one per
// (binding, operation), under an ID the host's envelope lists for exact
// inclusion — <presence binding>:<binding>:<operation>, scoped by the presence
// binding so two instances of one module on one host hold distinct units and
// withdrawing one instance's leaves the other's alone — with the operation's
// scope ceiling as one sorted, comma-joined value, the key the host installs
// the binding under and its lookup method when the contract declares them, and
// the module's queue and namespace when it declares exactly one of each.
//
// A unit's queue and namespace are the UNIT's and stay single-valued: a module
// declaring none, or several, grants no queue- or namespace-scoped authority on
// any unit, which is a narrower grant and never every queue. What the module
// itself owns is on the document (Queues, Namespaces, ScopeCeilings), written
// out whole, so several are carried rather than refused.
func authorityBindings(presenceBinding string, contract *modulecontract.Resolved) []solutionhost.AuthorityBinding {
	var queue, namespace string
	if len(contract.Queues) == 1 {
		queue = contract.Queues[0]
	}
	if len(contract.Namespaces) == 1 {
		namespace = contract.Namespaces[0]
	}
	bindings := make([]solutionhost.AuthorityBinding, 0)
	for _, binding := range contract.Bindings {
		var lookupMethod string
		if binding.Lookup != nil {
			lookupMethod = binding.Lookup.Method
		}
		for _, operation := range binding.Operations {
			bindings = append(bindings, solutionhost.AuthorityBinding{
				ID:           presenceBinding + ":" + binding.ID + ":" + operation,
				Revision:     binding.Revision,
				Audience:     binding.Audience,
				Scope:        strings.Join(binding.Scopes[operation], ","),
				Queue:        queue,
				Namespace:    namespace,
				BindingKey:   binding.BindingKey,
				LookupMethod: lookupMethod,
			})
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	return bindings
}

// errUnrenderedDestination is the refusal of a contract the reader accepted
// whose destination this render does not deliver: the renderer's verdict past
// the reader, named so a conformance run can tell it from any other.
var errUnrenderedDestination = errors.New("the contract declares a destination this render does not deliver")

// refuseUnrenderedDestinations holds each destination the contract declares
// against the endpoints the presence document lists for this instance. A
// destination is NOT restated in the authority document (core#735): the
// service and endpoint are already the presence document's, signed once so
// two readers cannot disagree, and the destination's kind is the vocabulary of
// the host's envelope, which a document never carries. What the render owes
// is that every destination it is asked to expose is one it actually renders;
// a destination naming an endpoint the presence does not list would be a
// declaration the host can never route, so it is refused by name.
func refuseUnrenderedDestinations(module string, contract *modulecontract.Resolved, endpoints []SolutionEndpoint) error {
	rendered := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint.Module == module {
			rendered[endpoint.Service+"/"+endpoint.Name] = struct{}{}
		}
	}
	var missing []string
	for _, destination := range contract.Destinations {
		if _, ok := rendered[destination.Service+"/"+destination.Endpoint]; !ok {
			missing = append(missing, fmt.Sprintf("%s (%s/%s)", destination.ID, destination.Service, destination.Endpoint))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: module %s declares the destinations %s, and its presence document lists no such endpoint; a destination is held to the endpoints the module renders, so declare the endpoint on the service or withdraw the destination", errUnrenderedDestination, module, strings.Join(missing, ", "))
}

// solutionAuthorityConfigMap carries one authority document, written through a
// struct so the delivered YAML has a stable field order.
type solutionAuthorityConfigMap struct {
	APIVersion string                           `yaml:"apiVersion"`
	Kind       string                           `yaml:"kind"`
	Metadata   solutionHostBindingConfigMapMeta `yaml:"metadata"`
	Data       map[string]string                `yaml:"data"`
}

// authorityConfigMapName is the ConfigMap's object name for an authority ID.
// The ID is derived from a binding ID the render already holds to one object
// name label, so it is an object name as it stands.
func authorityConfigMapName(authority string) string {
	return solutionAuthorityKind + "-" + authority
}

// writeAuthorityDocument writes one document's ConfigMap into the authority
// namespace and returns its file name; carrier is the signed carrier when there
// is one.
func writeAuthorityDocument(directory string, document *solutionhost.AuthorityDocument, module string, carrier ...[]byte) (string, error) {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return "", err
	}
	defer tree.Close()
	return writeAuthorityDocumentInTree(tree, directory, document, module, carrier...)
}

func writeAuthorityDocumentInTree(tree *renderRoot, directory string, document *solutionhost.AuthorityDocument, module string, carrier ...[]byte) (string, error) {
	encoded, err := solutionhost.MarshalAuthority(document)
	if err != nil {
		return "", fmt.Errorf("marshal authority %q: %w", document.Authority, err)
	}
	configMap := solutionAuthorityConfigMap{
		APIVersion: "v1",
		Kind:       kindConfigMap,
		Metadata: solutionHostBindingConfigMapMeta{
			Name:      authorityConfigMapName(document.Authority),
			Namespace: authorityNamespace,
			// Labelled with the binding it is granted over, as the presence
			// document is: there is one authority per binding, so the binding
			// is what a host selects an authority document by — and it is the
			// one of the two IDs the render holds to a label value's length.
			Labels: map[string]string{
				managedByLabel:           managedByCodefly,
				solutionHostBindingLabel: solutionAuthorityKind,
				bindingLabel:             document.PresenceBinding,
				solutionLabel:            module,
			},
		},
		Data: map[string]string{solutionhost.AuthorityFileName: string(encoded)},
	}
	if len(carrier) > 0 && len(carrier[0]) > 0 {
		configMap.Data[authorityCarrierKey] = string(carrier[0])
	}
	body, err := yaml.Marshal(configMap)
	if err != nil {
		return "", fmt.Errorf("encode authority %q: %w", document.Authority, err)
	}
	file := document.Authority + ".yaml"
	if err := tree.write(directory, file, body); err != nil {
		return "", fmt.Errorf("write authority %q: %w", document.Authority, err)
	}
	return file, nil
}

// writeAuthorityKustomization lists the delivered documents so the overlay
// builds, exactly as the presence overlay does.
func writeAuthorityKustomization(owned, environment string, names []string) error {
	tree, err := newRenderRoot(owned)
	if err != nil {
		return err
	}
	defer tree.Close()
	return writeAuthorityKustomizationInTree(tree, owned, environment, names)
}

func writeAuthorityKustomizationInTree(tree *renderRoot, owned, environment string, names []string) error {
	sort.Strings(names)
	body, err := yaml.Marshal(kustomizationManifest{APIVersion: kustomizeAPIVersion, Kind: kindKustomization, Resources: names})
	if err != nil {
		return fmt.Errorf("encode authority kustomization: %w", err)
	}
	path := filepath.Join(owned, filepath.FromSlash(solutionAuthorityOverlay(environment)), kustomizationFile)
	if err := tree.write(filepath.Dir(path), filepath.Base(path), body); err != nil {
		return fmt.Errorf("write authority kustomization: %w", err)
	}
	return nil
}

// deliveredAuthorityPath is the render subdirectory the inventory records,
// derived from what this render actually wrote, for the same reason
// deliveredBindingPath is.
func deliveredAuthorityPath(declared []DeclaredAuthority) string {
	if len(declared) == 0 {
		return ""
	}
	return solutionAuthorityDir
}

// servingBuild is the one build an authority document approves for a unit: the
// image of the authenticating container of the unit's serving workloads — its
// Deployments, StatefulSets and DaemonSets. A bootstrap Job of the same unit
// runs its own image and is declared in the presence document like every pod
// the host runs, but it is not what mints under the module's principal, so it
// does not decide the approved build. A unit whose serving workloads run two
// distinct builds cannot be approved by one document and is refused.
func servingBuild(owned string, opts *RenderOptions, unit SolutionArtifactUnit) (solutionhost.ImageDigest, error) {
	tree, err := inspectRenderRoot(owned, opts)
	if err != nil {
		return "", err
	}
	defer releaseRenderRoot(tree, opts)

	rendered, err := renderedWorkloadsInTree(tree, filepath.Join(owned, filepath.FromSlash(unit.Path)), opts.Environment)
	if err != nil {
		return "", err
	}
	builds := map[solutionhost.ImageDigest]struct{}{}
	for index := range rendered {
		workload := &rendered[index]
		switch workload.Kind {
		case kindDeployment, kindStatefulSet, kindDaemonSet:
		default:
			continue
		}
		authenticating, _, err := authenticatingContainer(unit.Name, workload)
		if err != nil {
			return "", fmt.Errorf("workload %s: %w", workload.Name, err)
		}
		builds[solutionhost.ImageDigest(authenticating.Image.Digest)] = struct{}{}
	}
	if len(builds) != 1 {
		return "", fmt.Errorf("its unit runs %d distinct serving builds, and an authority document approves exactly one", len(builds))
	}
	for digest := range builds {
		return digest, nil
	}
	return "", nil
}
