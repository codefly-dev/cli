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

// peerAuthentication renders the Istio STRICT-mTLS policy shared by both
// backends. It selects the backend workload by the conventional app label; if
// the workload is unlabelled the policy simply matches nothing rather than
// locking out unrelated pods.
type peerAuthentication struct {
	APIVersion string                 `yaml:"apiVersion"`
	Kind       string                 `yaml:"kind"`
	Metadata   objectMeta             `yaml:"metadata"`
	Spec       peerAuthenticationSpec `yaml:"spec"`
}

type peerAuthenticationSpec struct {
	Selector workloadSelector `yaml:"selector"`
	MTLS     mtlsSetting      `yaml:"mtls"`
}

type workloadSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type mtlsSetting struct {
	Mode string `yaml:"mode"`
}

func renderPeerAuthentication(exposure *Exposure) (string, error) {
	object := peerAuthentication{
		APIVersion: "security.istio.io/v1",
		Kind:       "PeerAuthentication",
		Metadata: objectMeta{
			Name:      exposure.Service + "-mtls",
			Namespace: exposure.Namespace,
			Labels:    managedLabels(exposure.Service),
		},
		Spec: peerAuthenticationSpec{
			Selector: workloadSelector{MatchLabels: map[string]string{"app": exposure.Service}},
			MTLS:     mtlsSetting{Mode: "STRICT"},
		},
	}
	return marshalDocument(object)
}
