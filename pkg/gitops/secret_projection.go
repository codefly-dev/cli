package gitops

import (
	"fmt"

	"github.com/codefly-dev/core/resources"
)

// externalSecretAPIVersion is the External Secrets Operator API the promotable
// bundle already understands: render.go whitelists external-secrets.io secretKey
// references and pkg/tenants patches an ExternalSecret's store per tenant.
const (
	externalSecretAPIVersion = "external-secrets.io/v1"
	kindExternalSecret       = "ExternalSecret"
	// secretRefreshInterval keeps the External Secrets Operator re-reading the
	// remote store on a fixed cadence so a rotated vault value propagates into the
	// in-cluster Secret. Omitting it lets the field default to "0" on some ESO
	// installs, which syncs once and never refreshes — silently defeating rotation.
	secretRefreshInterval = "1h"

	kindSecretStore        = "SecretStore"
	kindClusterSecretStore = "ClusterSecretStore"
)

// externalSecretStoreKinds are the only kinds an ExternalSecret's secretStoreRef
// accepts. An environment declaration naming a backend type ("azure-keyvault")
// instead of one of these renders a manifest that passes codefly's render checks
// but is rejected by ESO admission after promotion, so it is caught here.
var externalSecretStoreKinds = map[string]struct{}{
	kindSecretStore:        {},
	kindClusterSecretStore: {},
}

type externalSecret struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   externalSecretMeta `yaml:"metadata"`
	Spec       externalSecretSpec `yaml:"spec"`
}

type externalSecretMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type externalSecretSpec struct {
	RefreshInterval string                 `yaml:"refreshInterval"`
	SecretStoreRef  externalSecretStoreRef `yaml:"secretStoreRef"`
	Target          externalSecretTarget   `yaml:"target"`
	Data            []externalSecretData   `yaml:"data"`
}

type externalSecretStoreRef struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

type externalSecretTarget struct {
	Name string `yaml:"name"`
}

type externalSecretData struct {
	SecretKey string               `yaml:"secretKey"`
	RemoteRef externalSecretRemote `yaml:"remoteRef"`
}

type externalSecretRemote struct {
	Key string `yaml:"key"`
}

// managedSecretProjection renders the ExternalSecret that materializes a managed
// service's secret-<service> from a remote secret store. It is the missing joint
// of the KV→secretKeyRef chain: an environment declares where a managed service's
// secrets live (EnvironmentManagedSecretReference), the operator writes the real
// values into that store once, and this projection copies the declared remote
// keys into the in-cluster Secret the promotable manifests already reference — no
// secret value ever entering git, state, or manifests.
//
// Every reference of one managed service must resolve through the same store: an
// ExternalSecret owns a single target Secret, so mixing stores would silently
// drop all but one store's keys.
func managedSecretProjection(service, namespace string, refs []resources.EnvironmentManagedSecretReference) (*externalSecret, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if namespace == "" {
		return nil, fmt.Errorf("managed service %q declares secret references but its environment has no namespace", service)
	}
	store := refs[0].SecretStore
	if store.Name == "" || store.Kind == "" {
		return nil, fmt.Errorf("managed service %q secret store requires both name and kind", service)
	}
	if _, ok := externalSecretStoreKinds[store.Kind]; !ok {
		return nil, fmt.Errorf("managed service %q secret store kind %q must be SecretStore or ClusterSecretStore", service, store.Kind)
	}
	data := make([]externalSecretData, 0, len(refs))
	for _, ref := range refs {
		if ref.Name == "" || ref.RemoteKey == "" {
			return nil, fmt.Errorf("managed service %q secret reference requires both name and remote-key", service)
		}
		if ref.SecretStore != store {
			return nil, fmt.Errorf("managed service %q secret references resolve through more than one store", service)
		}
		data = append(data, externalSecretData{
			SecretKey: ref.Name,
			RemoteRef: externalSecretRemote{Key: ref.RemoteKey},
		})
	}
	target := "secret-" + service
	return &externalSecret{
		APIVersion: externalSecretAPIVersion,
		Kind:       kindExternalSecret,
		Metadata:   externalSecretMeta{Name: target, Namespace: namespace},
		Spec: externalSecretSpec{
			RefreshInterval: secretRefreshInterval,
			SecretStoreRef:  externalSecretStoreRef{Name: store.Name, Kind: store.Kind},
			Target:          externalSecretTarget{Name: target},
			Data:            data,
		},
	}, nil
}
