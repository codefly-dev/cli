package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/solutionhost/modulecontract"
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

// workspaceValues resolves contract slots from the workspace configurations
// the environment provides, matching keys in either spelling core accepts.
type workspaceValues struct {
	provided *configurations.WorkspaceConfigurations
}

// Value implements modulecontract.Values over the configurations the
// composition provides: the key is matched in either spelling core accepts,
// and a group supplying it in two spellings is an error, never a choice —
// the same contract would otherwise resolve to whichever a reader met first.
func (values workspaceValues) Value(group, key string) (string, bool, bool, error) {
	if values.provided == nil {
		return "", false, false, nil
	}
	normalized := strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
	var spellings []string
	var foundValue string
	var foundSecret, found bool
	for _, info := range values.provided.Infos {
		if info.Name != group {
			continue
		}
		for _, value := range info.ConfigurationValues {
			if value.Key == key || strings.ToUpper(strings.ReplaceAll(value.Key, "-", "_")) == normalized {
				if !slices.Contains(spellings, value.Key) {
					spellings = append(spellings, value.Key)
				}
				if !found {
					foundValue, foundSecret, found = value.Value, value.Secret, true
				}
			}
		}
	}
	if len(spellings) > 1 {
		sort.Strings(spellings)
		return "", false, false, fmt.Errorf("group %q supplies it as %s", group, strings.Join(spellings, " and "))
	}
	return foundValue, foundSecret, found, nil
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
) ([]AuthorityInstance, string, error) {
	contract, err := modulecontract.Load(module.Dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Sprintf("module %s publishes no %s, so it asks for no authority", module.Name, modulecontract.FileName), nil
	}
	if err != nil {
		return nil, "", err
	}
	var identities []*resources.Service
	for _, service := range services {
		if service.ModuleIdentity {
			identities = append(identities, service)
		}
	}
	if len(identities) == 0 {
		// The contract is the module's request for authority. With no service
		// declaring module-identity nothing would present it, and the request
		// would be dropped rather than refused: the composition is inconsistent
		// and is told so here, before slot resolution or any document.
		return nil, "", fmt.Errorf("module %s publishes %s, which asks for authority, but no service declares module-identity, so nothing would present it; declare module-identity on the service that presents the module's identity, or withdraw the contract", module.Name, modulecontract.FileName)
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
	resolved, err := contract.Resolve(workspaceValues{provided: provided})
	if err != nil {
		return nil, "", fmt.Errorf("module %s: %w", module.Name, err)
	}
	// The principal is the module's own name — the contract says so of
	// itself — and it is held to that here, because the contract is written in
	// the module's repository: a module claiming another module's principal
	// would claim that principal's bindings, and nothing downstream ties a
	// principal to the module presenting it.
	if resolved.Principal != module.Name {
		return nil, "", fmt.Errorf("module %s publishes a contract whose principal is %q; the principal every credential of a module is issued to is the module's own name, so the contract declares principal: %s", module.Name, resolved.Principal, module.Name)
	}
	if err := refuseUncarriedAuthority(module.Name, resolved); err != nil {
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

// renderAuthorityDocuments renders the module's authority document into the
// staged tree, approving the build its presenting service's workloads run, and
// returns the tree-relative paths written. A render is one module, and a module
// has one binding and so one authority: two instances would be two documents
// under one ID, the second silently overwriting the first, so the pair is
// refused here as it is where the instances are derived.
func renderAuthorityDocuments(owned string, opts *RenderOptions) ([]DeclaredAuthority, error) {
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
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, fmt.Errorf("create authority directory: %w", err)
	}
	declared := make([]DeclaredAuthority, 0, len(instances))
	names := make([]string, 0, len(instances))
	for index := range instances {
		document, err := authorityDocument(owned, opts, &instances[index])
		if err != nil {
			return nil, err
		}
		file, err := writeAuthorityDocument(directory, document, instances[index].Module)
		if err != nil {
			return nil, err
		}
		declared = append(declared, DeclaredAuthority{
			Path: filepath.ToSlash(filepath.Join(solutionAuthorityOverlay(opts.Environment), file)), Authority: document.Authority, Build: string(document.ApprovedBuild),
		})
		names = append(names, file)
	}
	if err := writeAuthorityKustomization(owned, opts.Environment, names); err != nil {
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
	}
	if err := document.Validate(); err != nil {
		return nil, fmt.Errorf("authority of %s/%s is invalid: %w", instance.Module, instance.Service, err)
	}
	return document, nil
}

// authorityBindings derives the units of authority a contract asks for: one per
// (binding, operation), under an ID the host's envelope lists for exact
// inclusion — <presence binding>:<binding>:<operation>, scoped by the presence
// binding so two instances of one module on one host hold distinct units and
// withdrawing one instance's leaves the other's alone — with the operation's
// scope ceiling as one sorted, comma-joined value, and the module's queue and
// namespace when it declares one of each. A module declaring none grants no
// queue- or namespace-scoped authority, which is a narrower grant and never
// every queue; a module declaring several is refused before this, by
// refuseUncarriedAuthority, rather than granted none.
func authorityBindings(presenceBinding string, contract *modulecontract.Resolved) []solutionhost.AuthorityBinding {
	var queue, namespace string
	if len(contract.Queues) == 1 {
		queue = contract.Queues[0]
	}
	if len(contract.Namespaces) == 1 {
		namespace = contract.Namespaces[0]
	}
	var bindings []solutionhost.AuthorityBinding
	for _, binding := range contract.Bindings {
		for _, operation := range binding.Operations {
			bindings = append(bindings, solutionhost.AuthorityBinding{
				ID:        presenceBinding + ":" + binding.ID + ":" + operation,
				Revision:  binding.Revision,
				Audience:  binding.Audience,
				Scope:     strings.Join(binding.Scopes[operation], ","),
				Queue:     queue,
				Namespace: namespace,
			})
		}
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].ID < bindings[j].ID })
	return bindings
}

// refuseUncarriedAuthority refuses a contract declaring what core's authority
// document cannot carry today, so nothing a module publishes is dropped between
// the contract and the signed document: a host cannot enforce a declaration it
// never receives, and a declaration that changed without the document
// changing would be enforced as before. Each is named, with the field the
// document would need. A module declaring none of them renders as it does;
// one declaring any waits on core growing the document.
// errUncarriedAuthority is the refusal of a contract the reader accepted and
// the signed authority document cannot carry whole: the renderer's verdict
// past the reader, named so a conformance run can tell it from any other.
var errUncarriedAuthority = errors.New("the contract declares what the signed authority document cannot carry")

func refuseUncarriedAuthority(module string, contract *modulecontract.Resolved) error {
	var uncarried []string
	if len(contract.Queues) > 1 {
		uncarried = append(uncarried, fmt.Sprintf("%d queues (an AuthorityBinding carries one queue)", len(contract.Queues)))
	}
	if len(contract.Namespaces) > 1 {
		uncarried = append(uncarried, fmt.Sprintf("%d namespaces (an AuthorityBinding carries one namespace)", len(contract.Namespaces)))
	}
	if len(contract.ScopeCeilings) > 0 {
		uncarried = append(uncarried, "scope_ceilings (the document has no field for the module's own ceilings)")
	}
	if len(contract.Destinations) > 0 {
		uncarried = append(uncarried, "destinations (the document has no field for the endpoints a module exposes, nor their kinds)")
	}
	for _, binding := range contract.Bindings {
		if binding.BindingKey != "" {
			uncarried = append(uncarried, fmt.Sprintf("binding %s binding_key (an AuthorityBinding carries no key)", binding.ID))
		}
		if binding.Lookup != nil && binding.Lookup.Method != "" {
			uncarried = append(uncarried, fmt.Sprintf("binding %s lookup.method (an AuthorityBinding carries no lookup method)", binding.ID))
		}
	}
	if len(uncarried) == 0 {
		return nil
	}
	return fmt.Errorf("%w: module %s declares %s, and nothing it declares is dropped on the way to the host; this needs core's solutionhost.AuthorityBinding to grow those fields before the module can render authority", errUncarriedAuthority, module, strings.Join(uncarried, "; "))
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
	if err := os.WriteFile(filepath.Join(directory, file), body, 0o600); err != nil {
		return "", fmt.Errorf("write authority %q: %w", document.Authority, err)
	}
	return file, nil
}

// writeAuthorityKustomization lists the delivered documents so the overlay
// builds, exactly as the presence overlay does.
func writeAuthorityKustomization(owned, environment string, names []string) error {
	sort.Strings(names)
	body, err := yaml.Marshal(kustomizationManifest{APIVersion: kustomizeAPIVersion, Kind: kindKustomization, Resources: names})
	if err != nil {
		return fmt.Errorf("encode authority kustomization: %w", err)
	}
	path := filepath.Join(owned, filepath.FromSlash(solutionAuthorityOverlay(environment)), kustomizationFile)
	if err := os.WriteFile(path, body, 0o600); err != nil {
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
	rendered, err := renderedWorkloads(filepath.Join(owned, filepath.FromSlash(unit.Path)), opts.Environment)
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
