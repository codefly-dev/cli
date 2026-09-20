package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/codefly-dev/cli/pkg/cli/yamledit"
	coreservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

const (
	networkPolicyAPIVersion = "networking.k8s.io/v1"
	kindNetworkPolicy       = "NetworkPolicy"
	egressPolicyFile        = "egress-network-policy.yaml"
	kindConfigMap           = "ConfigMap"
	kindKustomizationDoc    = "Kustomization"
	serviceBaseDir          = "base"
	// proxySidecarSuffix names the sidecar after the managed service it
	// terminates, so a pod consuming two managed endpoints carries two
	// distinguishable containers.
	proxySidecarSuffix = "-proxy"
)

type networkPolicy struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   namespacedMeta    `yaml:"metadata"`
	Spec       networkPolicySpec `yaml:"spec"`
}

type networkPolicySpec struct {
	PodSelector map[string]any        `yaml:"podSelector"`
	PolicyTypes []string              `yaml:"policyTypes"`
	Egress      []networkPolicyEgress `yaml:"egress"`
}

type networkPolicyEgress struct {
	To    []networkPolicyPeer `yaml:"to"`
	Ports []networkPolicyPort `yaml:"ports"`
}

type networkPolicyPeer struct {
	IPBlock networkPolicyIPBlock `yaml:"ipBlock"`
}

type networkPolicyIPBlock struct {
	CIDR string `yaml:"cidr"`
}

type networkPolicyPort struct {
	Protocol string `yaml:"protocol"`
	Port     int    `yaml:"port"`
}

// managedEgressPolicy renders the egress NetworkPolicy that lets the namespace's
// workloads reach one managed endpoint. Its peers are the CIDRs the cell
// published for that endpoint and its port is the one the cell declared the
// endpoint listens on — both descriptor facts. codefly keeps no engine-keyed
// port table to fall back on: a policy opened on a guessed port drops every
// connection to the endpoint while the deploy reports success.
//
// The selector is the whole namespace rather than the consuming pods. A
// NetworkPolicy is an allow-list union, so this permits nothing beyond the
// declared CIDRs and port, and it needs no pod labels out of a tree the service
// agent owns.
func managedEgressPolicy(service, namespace string, managed *resources.EnvironmentManagedService) (*networkPolicy, error) {
	if len(managed.EgressCIDRs) == 0 {
		return nil, nil
	}
	if managed.Port == 0 {
		// A transport or identity binding is the cell describing how the endpoint
		// is reached, and core refuses such a descriptor with no port. Reaching
		// here means the declaration was edited away from its source, so refuse
		// instead of falling back to a port for the engine.
		if managed.Transport != nil || managed.Identity != nil {
			return nil, fmt.Errorf("managed service %q declares a transport binding but no port to reach it on", service)
		}
		return nil, nil
	}
	if namespace == "" {
		return nil, fmt.Errorf("managed service %q declares egress CIDRs but its environment has no namespace", service)
	}
	peers := make([]networkPolicyPeer, 0, len(managed.EgressCIDRs))
	for _, cidr := range managed.EgressCIDRs {
		peers = append(peers, networkPolicyPeer{IPBlock: networkPolicyIPBlock{CIDR: cidr}})
	}
	return &networkPolicy{
		APIVersion: networkPolicyAPIVersion,
		Kind:       kindNetworkPolicy,
		Metadata:   namespacedMeta{Name: "egress-" + service, Namespace: namespace},
		Spec: networkPolicySpec{
			PodSelector: map[string]any{},
			PolicyTypes: []string{"Egress"},
			Egress: []networkPolicyEgress{{
				To:    peers,
				Ports: []networkPolicyPort{{Protocol: "TCP", Port: managed.Port}},
			}},
		},
	}, nil
}

// projectManagedTransport applies the environment's declared managed-service
// bindings to one consuming service's rendered tree: the proxy the cell says
// terminates the connection, the address its application dials, and the runtime
// identity it authenticates as. Only the managed services this service declares
// a dependency on are applied, so a namespace's other managed endpoints leave
// its pods untouched.
func projectManagedTransport(
	ctx context.Context,
	serviceRoot string,
	moduleName string,
	service *resources.Service,
	env *resources.Environment,
) error {
	consumed := consumedManagedServices(service, env)
	if len(consumed) == 0 {
		return nil
	}
	identity, err := soleWorkloadIdentity(service.Name, consumed, env)
	if err != nil {
		return err
	}
	base := filepath.Join(serviceRoot, serviceBaseDir)
	for _, name := range consumed {
		managed := env.ManagedServices[name]
		if err = attachManagedTransport(base, moduleName, name, service, &managed); err != nil {
			return err
		}
	}
	return coreservices.ProjectWorkloadIdentity(ctx, base, env.Namespace, service.Name, identity)
}

// consumedManagedServices returns, in a stable order, the environment's managed
// services this service's pods dial. Only edges that constrain running count: a
// build or schema edge on a database is read by the toolchain that generates
// code, not by the workload, so stamping its identity onto the pod would
// authenticate a container that never opens the connection.
func consumedManagedServices(service *resources.Service, env *resources.Environment) []string {
	var consumed []string
	for name := range env.ManagedServices {
		dependency := managedDependency(service, name)
		if dependency == nil || !dependency.Kind.Participates(resources.StageRun) {
			continue
		}
		consumed = append(consumed, name)
	}
	sort.Strings(consumed)
	return consumed
}

func managedDependency(service *resources.Service, managed string) *resources.ServiceDependency {
	for _, dependency := range service.ServiceDependencies {
		if dependency.Name == managed {
			return dependency
		}
	}
	return nil
}

// soleWorkloadIdentity returns the one runtime identity a service's managed
// dependencies declare. A pod runs under a single ServiceAccount, so two managed
// endpoints naming different principals have no rendering: whichever was stamped
// last would win and the other endpoint would refuse the workload at runtime
// with nothing in the deploy to show for it. Several endpoints reached as the
// same principal are one identity and render as one.
func soleWorkloadIdentity(service string, consumed []string, env *resources.Environment) (*resources.EnvironmentWorkloadIdentity, error) {
	var identity *resources.EnvironmentWorkloadIdentity
	var declaring []string
	for _, name := range consumed {
		declared := env.ManagedServices[name].Identity
		if declared == nil {
			continue
		}
		if identity != nil && !reflect.DeepEqual(identity, declared) {
			return nil, fmt.Errorf("service %q consumes managed services %s, which declare different runtime identities; a pod authenticates as one",
				service, strings.Join(append(declaring, name), ", "))
		}
		identity = declared
		declaring = append(declaring, name)
	}
	return identity, nil
}

// attachManagedTransport renders one managed service's declared transport into a
// consuming service's base manifests: the proxy container when the cell
// terminates the connection in the pod, and the address the application dials in
// either mode. A managed service with no declared transport carries no new fact,
// so the tree the agent rendered is left as it is.
func attachManagedTransport(
	base, moduleName, managedName string,
	service *resources.Service,
	managed *resources.EnvironmentManagedService,
) error {
	if managed.Transport == nil {
		return nil
	}
	proxied := false
	switch managed.Transport.Mode {
	case resources.TransportModeDirect:
	case resources.TransportModeProxy:
		// The rendered tree is promotable, and a promotable manifest may only
		// carry images pinned by digest. A tag moves, so a pinned workload beside
		// a floating proxy would reconcile to a proxy nobody reviewed.
		if !digestImagePattern.MatchString(managed.Transport.Image) {
			return fmt.Errorf("managed service %q declares proxy image %q, which is not pinned by sha256 digest",
				managedName, managed.Transport.Image)
		}
		if err := injectProxySidecar(base, managedName, *managed.Transport); err != nil {
			return err
		}
		proxied = true
	default:
		return fmt.Errorf("managed service %q declares transport mode %q, which this renderer does not implement",
			managedName, managed.Transport.Mode)
	}
	host, port := managed.DialHost()
	rewritten, err := rewriteDialAddress(base, endpointKeyPrefix(moduleName, managedName, service), host, port)
	if err != nil {
		return err
	}
	// A proxy exists to be dialed at loopback. If the rendered configuration
	// names no address for this dependency there is nothing to repoint, and the
	// sidecar would sit beside an application still dialing the endpoint itself —
	// a deploy that reconciles clean and fails on first connection.
	if proxied && rewritten == 0 {
		return fmt.Errorf("managed service %q declares a proxy transport but service %q carries no endpoint address to point at it",
			managedName, service.Name)
	}
	return nil
}

// endpointKeyPrefix is the environment-variable prefix the rendered
// configuration carries for every endpoint of one dependency, built the same way
// the agent built it. A dependency declared without a module belongs to the
// consuming service's own module.
func endpointKeyPrefix(moduleName, managedName string, service *resources.Service) string {
	module := managedDependency(service, managedName).Module
	if module == "" {
		module = moduleName
	}
	return strings.ToUpper(strings.ReplaceAll(
		fmt.Sprintf("%s__%s__%s__", resources.EndpointPrefix, module, managedName), "-", "_"))
}

// proxyContainer is the container the cell declares terminates the authenticated
// private connection. Image, args and the port it listens on are the cell's; the
// security context is codefly's promotable container contract, which every
// rendered container meets. Nothing else is added: the cell declares no sizing
// or probes for this container, and codefly will not invent either.
type proxyContainer struct {
	Name            string                    `yaml:"name"`
	Image           string                    `yaml:"image"`
	Args            []string                  `yaml:"args,omitempty"`
	Ports           []proxyContainerPort      `yaml:"ports"`
	SecurityContext restrictedSecurityContext `yaml:"securityContext"`
}

type proxyContainerPort struct {
	ContainerPort int `yaml:"containerPort"`
}

type restrictedSecurityContext struct {
	AllowPrivilegeEscalation bool                  `yaml:"allowPrivilegeEscalation"`
	RunAsNonRoot             bool                  `yaml:"runAsNonRoot"`
	ReadOnlyRootFilesystem   bool                  `yaml:"readOnlyRootFilesystem"`
	SeccompProfile           seccompProfile        `yaml:"seccompProfile"`
	Capabilities             containerCapabilities `yaml:"capabilities"`
}

type seccompProfile struct {
	Type string `yaml:"type"`
}

type containerCapabilities struct {
	Drop []string `yaml:"drop"`
}

func proxySidecar(managedName string, transport resources.EnvironmentManagedTransport) proxyContainer {
	return proxyContainer{
		Name:  managedName + proxySidecarSuffix,
		Image: transport.Image,
		Args:  transport.Args,
		Ports: []proxyContainerPort{{ContainerPort: transport.LocalPort}},
		SecurityContext: restrictedSecurityContext{
			// Written out rather than left to the zero value: the manifest contract
			// requires the key present and false, not merely absent.
			AllowPrivilegeEscalation: false,
			RunAsNonRoot:             true,
			ReadOnlyRootFilesystem:   true,
			SeccompProfile:           seccompProfile{Type: "RuntimeDefault"},
			Capabilities:             containerCapabilities{Drop: []string{"ALL"}},
		},
	}
}

// injectProxySidecar adds the declared proxy beside every workload in the base
// tree, replacing an existing container of the same name so a re-render is
// idempotent. It fails when the tree carries no workload: a proxy that lands
// nowhere leaves the application dialing a loopback port nothing listens on.
func injectProxySidecar(base, managedName string, transport resources.EnvironmentManagedTransport) error {
	sidecar, err := yamledit.Encode(proxySidecar(managedName, transport))
	if err != nil {
		return err
	}
	name := managedName + proxySidecarSuffix
	attached := 0
	err = editBaseDocuments(base, func(kind string, root *yaml.Node) bool {
		spec := podSpecNode(kind, root)
		if spec == nil {
			return false
		}
		containers := yamledit.MapValue(spec, "containers")
		if containers == nil || containers.Kind != yaml.SequenceNode {
			return false
		}
		for index, container := range containers.Content {
			if existing := yamledit.MapValue(container, "name"); existing != nil && existing.Value == name {
				containers.Content[index] = sidecar
				attached++
				return true
			}
		}
		containers.Content = append(containers.Content, sidecar)
		attached++
		return true
	})
	if err != nil {
		return err
	}
	if attached == 0 {
		return fmt.Errorf("managed service %q declares a proxy transport but %s carries no workload to run it beside", managedName, base)
	}
	return nil
}

// rewriteDialAddress points the application at the host and port the cell says
// it dials for this managed service, by rewriting the rendered configuration's
// endpoint entries in place. Under a proxy transport that address is loopback —
// the endpoint's own address never reaches the application, which is the point
// of terminating the connection in a sidecar.
//
// Only the host and port are replaced; a scheme or path the agent rendered is
// preserved, so this never reconstructs an address codefly does not own.
// It reports how many entries it rewrote, which is how the caller tells an
// application it has repointed from one it could not reach.
func rewriteDialAddress(base, keyPrefix, host string, port int) (int, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	rewritten := 0
	err := editBaseDocuments(base, func(kind string, root *yaml.Node) bool {
		if kind != kindConfigMap {
			return false
		}
		data := yamledit.MapValue(root, "data")
		if data == nil || data.Kind != yaml.MappingNode {
			return false
		}
		changed := false
		for index := 0; index+1 < len(data.Content); index += 2 {
			if !strings.HasPrefix(data.Content[index].Value, keyPrefix) {
				continue
			}
			value := data.Content[index+1]
			if replaced := replaceHostPort(value.Value, target); replaced != value.Value {
				value.Value = replaced
				changed = true
			}
			rewritten++
		}
		return changed
	})
	return rewritten, err
}

// replaceHostPort swaps the authority of an address for target, keeping any
// scheme and path around it.
func replaceHostPort(address, target string) string {
	prefix := ""
	rest := address
	if scheme, after, found := strings.Cut(address, "://"); found {
		prefix = scheme + "://"
		rest = after
	}
	path := ""
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		path = rest[slash:]
	}
	return prefix + target + path
}

// podSpecNode returns the pod template spec of a workload manifest, or nil when
// the manifest carries no pod template. It mirrors podSpec over nodes.
func podSpecNode(kind string, root *yaml.Node) *yaml.Node {
	spec := yamledit.MapValue(root, "spec")
	if spec == nil {
		return nil
	}
	switch kind {
	case kindPod:
		return spec
	case kindDeployment, kindStatefulSet, kindDaemonSet, kindReplicaSet, kindJob:
		return yamledit.MapValue(yamledit.MapValue(spec, "template"), "spec")
	case kindCronJob:
		template := yamledit.MapValue(yamledit.MapValue(spec, "jobTemplate"), "spec")
		return yamledit.MapValue(yamledit.MapValue(template, "template"), "spec")
	default:
		return nil
	}
}

// editBaseDocuments hands every manifest document directly in base to edit and
// rewrites the files whose documents it changed. It edits nodes rather than
// decoded values so each manifest keeps its key order and comments, leaving a
// reviewable diff that shows the projection and nothing else. A kustomization is
// skipped: it indexes the tree rather than describing a workload.
func editBaseDocuments(base string, edit func(kind string, root *yaml.Node) bool) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return fmt.Errorf("read rendered base %s: %w", base, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		extension := strings.ToLower(filepath.Ext(entry.Name()))
		if extension != yamlExtension && extension != ymlExtension {
			continue
		}
		path := filepath.Join(base, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		documents, decodeErr := decodeDocuments(path, data)
		if decodeErr != nil {
			return decodeErr
		}
		changed := false
		for _, document := range documents {
			root := document.Content[0]
			kind := ""
			if node := yamledit.MapValue(root, "kind"); node != nil {
				kind = node.Value
			}
			if kind == kindKustomizationDoc {
				continue
			}
			if edit(kind, root) {
				changed = true
			}
		}
		if !changed {
			continue
		}
		encoded, encodeErr := encodeDocuments(documents)
		if encodeErr != nil {
			return encodeErr
		}
		// A rendered manifest is a world-readable declaration, like its siblings.
		if err = os.WriteFile(path, encoded, 0o644); err != nil { //nolint:gosec
			return err
		}
	}
	return nil
}

// decodeDocuments decodes every YAML document of one rendered file as nodes,
// dropping the empty documents a trailing separator produces.
func decodeDocuments(path string, data []byte) ([]*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var documents []*yaml.Node
	for index := 0; ; index++ {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%s document %d: decode YAML: %w", path, index, err)
		}
		if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
			continue
		}
		documents = append(documents, &document)
	}
	return documents, nil
}

func encodeDocuments(documents []*yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	// Two spaces is the indentation every rendered manifest already carries, so
	// re-encoding one changes only the lines the projection touched.
	encoder.SetIndent(2)
	for _, document := range documents {
		if err := encoder.Encode(document); err != nil {
			return nil, err
		}
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// projectRenderedManagedTransport applies the environment's managed-service
// bindings to every service tree a single-service render produced — the origin
// service and any in-graph dependencies it pulled in — so per-service promotion
// renders the same transport and identity as a full module render. The service
// declarations come from the graph the flow already loaded, keyed by service
// unique, so no service is re-read from disk.
func projectRenderedManagedTransport(
	ctx context.Context,
	stage string,
	env *resources.Environment,
	graph map[string]*resources.Service,
) error {
	if len(env.ManagedServices) == 0 {
		return nil
	}
	modulesRoot := filepath.Join(stage, "modules")
	moduleEntries, err := os.ReadDir(modulesRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, moduleEntry := range moduleEntries {
		if !moduleEntry.IsDir() {
			continue
		}
		servicesRoot := filepath.Join(modulesRoot, moduleEntry.Name(), serviceUnitDir)
		serviceEntries, err := os.ReadDir(servicesRoot)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, serviceEntry := range serviceEntries {
			if !serviceEntry.IsDir() {
				continue
			}
			service := graph[resources.ServiceUnique(moduleEntry.Name(), serviceEntry.Name())]
			if service == nil {
				continue
			}
			if err := projectManagedTransport(
				ctx,
				filepath.Join(servicesRoot, serviceEntry.Name()),
				moduleEntry.Name(),
				service,
				env,
			); err != nil {
				return fmt.Errorf("project service %s managed transport: %w", serviceEntry.Name(), err)
			}
		}
	}
	return nil
}
