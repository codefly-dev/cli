package routing

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// objectMeta is the subset of Kubernetes metadata the renderers emit.
type objectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

// managedLabels are stamped on every generated object so a solution's install
// and uninstall can select exactly the routes codefly authored for a service.
func managedLabels(service string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "codefly",
		"codefly.dev/service":          service,
	}
}

func marshalDocument(object any) (string, error) {
	data, err := yaml.Marshal(object)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	return string(data), nil
}

// STRICT mTLS IS WHAT MAKES A PRINCIPAL MEAN ANYTHING.
//
// A cell admits a caller by its mesh principal — its ServiceAccount's SPIFFE
// ID — and a principal is only a fact about the connection when the peer
// presented a mesh certificate. Where mTLS is PERMISSIVE the sidecar accepts
// plaintext too, and a plaintext connection carries no principal at all: the
// policy still says "only the hop", and anything in the cluster that can reach
// the port is not held to it. So STRICT is not a hardening option on top of
// the allow rules; it is the condition under which they are rules.
//
// PeerAuthentication has two shapes, and this file renders both from one
// function:
//
//   - NAMESPACE-WIDE (an empty selector): every workload of the namespace,
//     including one rendered later by something else. That is what a module's
//     namespace gets on a meshed environment, projected by the render
//     (pkg/gitops mesh_posture.go) for every namespace the module occupies,
//     because the guarantee has to hold for pods this render does not know
//     about.
//   - PER WORKLOAD (selector app=<service>): one backend, as `codefly expose
//     service --mtls` has always emitted it beside the routes it renders. An
//     unlabelled workload matches nothing rather than locking out unrelated
//     pods, which is why this shape cannot be the namespace's guarantee.

// MeshNamespacePolicyName is the name of the namespace-wide STRICT policy. A
// namespace holds one: a second object of this shape would be a second claim
// on the same guarantee.
const MeshNamespacePolicyName = "codefly-mesh-strict"

// PeerAuthenticationKind and PeerAuthenticationGroup identify the object, so a
// reader checking a rendered tree for the namespace's posture names the same
// kind this file emits.
const (
	PeerAuthenticationGroup   = "security.istio.io"
	PeerAuthenticationKind    = "PeerAuthentication"
	peerAuthenticationVersion = PeerAuthenticationGroup + "/v1"
	// MeshModeSTRICT is the only mode either shape is rendered with.
	MeshModeSTRICT = "STRICT"
)

// PeerAuthentication is the Istio mTLS policy as this package emits it. It is
// exported so the render can write the namespace-wide one into a promoted tree
// as it writes any other manifest it owns, without a second copy of the shape.
type PeerAuthentication struct {
	APIVersion string                 `yaml:"apiVersion"`
	Kind       string                 `yaml:"kind"`
	Metadata   objectMeta             `yaml:"metadata"`
	Spec       peerAuthenticationSpec `yaml:"spec"`
}

type peerAuthenticationSpec struct {
	// Selector is omitted for the namespace-wide policy: an empty selector is
	// every workload of the namespace, and emitting `selector: {}` instead
	// would be a selector that matches nothing in some readers.
	Selector *workloadSelector `yaml:"selector,omitempty"`
	MTLS     mtlsSetting       `yaml:"mtls"`
}

type workloadSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type mtlsSetting struct {
	Mode string `yaml:"mode"`
}

// NamespaceMeshPolicy is the namespace-wide STRICT mTLS policy for one
// namespace: every workload in it, whoever rendered it.
func NamespaceMeshPolicy(namespace string) PeerAuthentication {
	return peerAuthentication(MeshNamespacePolicyName, namespace, map[string]string{"app.kubernetes.io/managed-by": "codefly"}, nil)
}

// peerAuthentication is the one renderer of the object: a nil selector is the
// namespace-wide policy, a non-nil one selects the workloads it labels.
func peerAuthentication(name, namespace string, labels, selector map[string]string) PeerAuthentication {
	object := PeerAuthentication{
		APIVersion: peerAuthenticationVersion,
		Kind:       PeerAuthenticationKind,
		Metadata:   objectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       peerAuthenticationSpec{MTLS: mtlsSetting{Mode: MeshModeSTRICT}},
	}
	if selector != nil {
		object.Spec.Selector = &workloadSelector{MatchLabels: selector}
	}
	return object
}

func renderPeerAuthentication(exposure *Exposure) (string, error) {
	return marshalDocument(peerAuthentication(
		exposure.Service+"-mtls", exposure.Namespace,
		managedLabels(exposure.Service),
		map[string]string{"app": exposure.Service},
	))
}
