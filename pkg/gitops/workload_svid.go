package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	coreservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

const (
	svidVolume  = "codefly-workload-svid"
	svidMount   = "/var/run/secrets/spiffe.io"
	peersVolume = "codefly-workload-peers"
	peersMount  = "/etc/codefly/workload-peers"
)

// The render supplies these paths only to explicit workload-identity consumers.
func workloadIdentityPaths() map[string]string {
	return map[string]string{
		"TOKEN_FILE":           path.Join(identityTokenMount, identityTokenFile),
		"CERT_FILE":            path.Join(svidMount, "tls.crt"),
		"KEY_FILE":             path.Join(svidMount, "tls.key"),
		"TRUST_BUNDLE_FILE":    path.Join(svidMount, "ca.crt"),
		"ALLOWED_CALLERS_FILE": path.Join(peersMount, "allowed-callers"),
		"MINT_PEERS_FILE":      path.Join(peersMount, "mint-peers"),
		"GATEWAY_PEERS_FILE":   path.Join(peersMount, "gateway-peers"),
	}
}

// Bind the group's carrier keys with Core's encoder. Projection binds the paths
// to the authenticating container so an agent's overlay cannot redirect them
// away from the volumes the CLI owns.
func workloadIdentityEnvironment(environment string) (map[string]string, error) {
	info := &basev0.ConfigurationInformation{Name: environments.WorkloadIdentityConfiguration}
	for key, value := range workloadIdentityPaths() {
		info.ConfigurationValues = append(info.ConfigurationValues, &basev0.ConfigurationValue{Key: key, Value: value})
	}
	variables, err := resources.ConfigurationAsEnvironmentVariables(&basev0.Configuration{
		Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{info},
	}, environment, false)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(variables))
	for _, variable := range variables {
		value, ok := variable.Value.(string)
		if !ok || variable.File {
			return nil, fmt.Errorf("workload-identity path %s is not an inline string", variable.Key)
		}
		values[variable.Key] = value
	}
	return values, nil
}

// workloadIdentityPath recognizes only a CLI-owned carrier AND its exact
// resolved path. TOKEN_FILE is conservatively sensitive to Core's key-only
// classifier; the manifest guard can also judge the value. This admits no
// inline token and does not exempt arbitrary keys ending in _FILE.
func workloadIdentityPath(name string, value any) bool {
	literal, ok := value.(string)
	if !ok {
		return false
	}
	configuration := &basev0.Configuration{Origin: resources.ConfigurationWorkspace}
	for key, file := range workloadIdentityPaths() {
		carrier := resources.ConfigurationValueEnvironmentKey(configuration, environments.WorkloadIdentityConfiguration, &basev0.ConfigurationValue{Key: key})
		if name == carrier && literal == file {
			return true
		}
	}
	return false
}

func requestsWorkloadIdentity(service *resources.Service) bool {
	return slices.Contains(service.WorkspaceConfigurationDependencies, environments.WorkloadIdentityConfiguration)
}

// Core owns the account name both when projecting a ServiceAccount and when
// generating an agent's pod overlay. Peer planning uses that same resolver;
// projection below verifies the effective pod really runs as this account.
func workloadAccount(service string) string {
	overlay := &coreservices.PodTemplateOverlay{ServiceAccount: &coreservices.WorkloadServiceAccount{}}
	overlay.DefaultServiceAccountName(service)
	return overlay.ServiceAccountName()
}

type workloadPeer struct {
	Account  string
	SPIFFEID string
	Consumer bool
}

type workloadPeers map[string]workloadPeer

func resolveWorkloadPeers(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (workloadPeers, error) {
	consumers, err := environments.WorkloadIdentityServices(ctx, workspace, env)
	if err != nil {
		return nil, err
	}
	if len(consumers) == 0 {
		return nil, nil
	}
	if !env.DeclaresLocalK3d() {
		return nil, fmt.Errorf("environment %s cannot render workload-identity: SVID issuance requires a declared local k3d cluster (cluster.kind: k3d, cluster.context: k3d-<name>)", env.Name)
	}
	peers := workloadPeers{}
	add := func(module, service string, consumer bool) error {
		namespace := env.ModuleNamespace(workspace, module)
		if namespace == "" {
			return fmt.Errorf("workload identity %s/%s has no resolved namespace", module, service)
		}
		account := workloadAccount(service)
		peers[resources.ServiceUnique(module, service)] = workloadPeer{Account: account, SPIFFEID: env.Host.SPIFFEID(namespace, account), Consumer: consumer}
		return nil
	}
	module, service, _ := strings.Cut(env.Host.Gateway, "/")
	if err := add(module, service, false); err != nil {
		return nil, err
	}
	for _, consumer := range consumers {
		if err := add(consumer.Module, consumer.Name, true); err != nil {
			return nil, err
		}
	}
	return peers, nil
}

func (peers workloadPeers) files(host *environments.EnvironmentHost, unique string) (map[string]string, error) {
	if host == nil || host.Gateway == "" {
		return nil, fmt.Errorf("workload-identity requires host.gateway")
	}
	gateway, exists := peers[host.Gateway]
	if !exists {
		return nil, fmt.Errorf("host.gateway %s has no resolved workload identity", host.Gateway)
	}
	ids := []string{gateway.SPIFFEID}
	if unique == host.Gateway {
		ids = nil
		for _, peer := range peers {
			if peer.Consumer {
				ids = append(ids, peer.SPIFFEID)
			}
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return nil, fmt.Errorf("workload-identity %s has no peers", unique)
	}
	contents := strings.Join(ids, "\n") + "\n"
	return map[string]string{"allowed-callers": contents, "mint-peers": contents, "gateway-peers": contents}, nil
}

// projectWorkloadSVID uses the token projection seam, before presence hashes
// the unit. Only the authenticating container receives private credentials.
func projectWorkloadSVID(root string, service *resources.Service, env *environments.Environment, scope unitScope) error {
	unique := resources.ServiceUnique(scope.Module, service.Name)
	peer, exists := scope.WorkloadPeers[unique]
	if !requestsWorkloadIdentity(service) {
		// A gateway can obtain credentials from the platform independently of
		// this group. Its account must still match the ID its consumers trust.
		if exists {
			workloads, err := renderedWorkloads(root, env.Name)
			if err != nil {
				return err
			}
			count := 0
			for i := range workloads {
				if !servingKind(workloads[i].Kind) {
					continue
				}
				count++
				if err := verifyWorkloadPeer(&workloads[i], peer, env.Host, scope.Namespace); err != nil {
					return err
				}
			}
			if count == 0 {
				return fmt.Errorf("gateway %s renders no serving workload", unique)
			}
		}
		return nil
	}
	if !exists || !peer.Consumer {
		return fmt.Errorf("workload-identity service %s has no composition identity plan", unique)
	}
	files, err := scope.WorkloadPeers.files(env.Host, unique)
	if err != nil {
		return err
	}
	values, err := workloadIdentityEnvironment(env.Name)
	if err != nil {
		return err
	}
	workloads, err := renderedWorkloads(root, env.Name)
	if err != nil {
		return err
	}
	var patches []podProjection
	count := 0
	for i := range workloads {
		workload := &workloads[i]
		if !servingKind(workload.Kind) {
			continue
		}
		count++
		if err := verifyWorkloadPeer(workload, peer, env.Host, scope.Namespace); err != nil {
			return err
		}
		container, _, err := authenticatingContainer(service.Name, workload)
		if err != nil {
			return err
		}
		for _, reserved := range []struct{ name, mount string }{{svidVolume, svidMount}, {peersVolume, peersMount}} {
			if workload.hasVolume(reserved.name) {
				return fmt.Errorf("workload %s already declares reserved volume %s", workload.Name, reserved.name)
			}
			for _, mount := range workload.mounts[container.Name] {
				if mountOverlaps(mount.path, reserved.mount) {
					return fmt.Errorf("workload %s mount %s shadows workload-identity path %s", workload.Name, mount.path, reserved.mount)
				}
			}
		}
		patches = append(patches,
			podProjection{kind: workload.Kind, name: workload.Name, container: container.Name, volume: map[string]any{"name": svidVolume, "csi": map[string]any{"driver": "spiffe.csi.cert-manager.io", "readOnly": true}}, mount: readOnlyMount(svidVolume, svidMount), env: values},
			podProjection{kind: workload.Kind, name: workload.Name, container: container.Name, volume: map[string]any{"name": peersVolume, "configMap": map[string]any{"name": service.Name + "-workload-peers"}}, mount: readOnlyMount(peersVolume, peersMount)},
		)
	}
	if count == 0 {
		return fmt.Errorf("workload-identity service %s renders no serving workload", unique)
	}
	// This also supports services declaring no presence document, including the
	// gateway: opting in always projects a token for the same host audience.
	opts := &RenderOptions{Environment: env.Name, Host: env.Host}
	if err := projectIdentityToken(root, opts, SolutionArtifactUnit{Name: service.Name, Path: "."}); err != nil {
		return err
	}
	if err := addOverlayPatches(root, env.Name, patches); err != nil {
		return err
	}
	name := service.Name + "-workload-identity"
	metadata := func(name string) map[string]any { return map[string]any{"name": name, "namespace": scope.Namespace} }
	docs := []map[string]any{
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role", "metadata": metadata(name), "rules": []any{map[string]any{"apiGroups": []string{"cert-manager.io"}, "resources": []string{"certificaterequests"}, "verbs": []string{"create"}}}},
		{"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding", "metadata": metadata(name), "roleRef": map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name}, "subjects": []any{map[string]any{"kind": "ServiceAccount", "name": peer.Account, "namespace": scope.Namespace}}},
		{"apiVersion": "v1", "kind": "ConfigMap", "metadata": metadata(service.Name + "-workload-peers"), "data": files},
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	for _, doc := range docs {
		if err := encoder.Encode(doc); err != nil {
			return err
		}
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	overlay := filepath.Join(root, "overlays", env.Name)
	const file = "workload-identity.yaml"
	if _, err := os.Stat(filepath.Join(overlay, file)); err == nil {
		return fmt.Errorf("reserved workload-identity resource file already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.WriteFile(filepath.Join(overlay, file), output.Bytes(), 0o600); err != nil {
		return err
	}
	// Both plain manifests and kustomize overlays are supported by the token
	// seam. Add the resource to the index when the overlay has one.
	for _, name := range []string{kustomizationFile, kustomizationFileAlt, kindKustomization} {
		if _, err := os.Stat(filepath.Join(overlay, name)); err == nil {
			return addKustomizationResource(overlay, file)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func verifyWorkloadPeer(workload *renderedWorkload, peer workloadPeer, host *environments.EnvironmentHost, namespace string) error {
	if host == nil || workload.ServiceAccount != peer.Account || host.SPIFFEID(namespace, workload.ServiceAccount) != peer.SPIFFEID {
		return fmt.Errorf("workload %s runs as ServiceAccount %s, not the resolved workload identity %s", workload.Name, workload.ServiceAccount, peer.Account)
	}
	return nil
}

func mountOverlaps(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") || a == "/" || b == "/"
}
