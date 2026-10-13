package gitops

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// NativePeer is one workload identity a native run issues: the account the
// service would run as in a cell, the SPIFFE ID the host verifies, and whether
// the service asked for the workload-identity group (the gateway may not).
type NativePeer struct {
	Module   string
	Service  string
	Account  string
	SPIFFEID string
	Consumer bool
	// TokenOnly marks a service that declares module-identity and no
	// workload-identity: in a cell its render mounts only a projected token,
	// so a native run issues it a token and no SVID, and it is in nobody's
	// peer list.
	TokenOnly bool
}

// NativeWorkloadAccount is the ServiceAccount name a service runs as: Core's
// default for its name, the same one a cell's render projects.
func NativeWorkloadAccount(service string) string {
	return workloadAccount(service)
}

// ResolveNativePeers plans the identities a native run issues, by the same
// rule the cell render plans them: the gateway and every explicit consumer,
// each in its module's namespace. A native run has no cluster to require.
func ResolveNativePeers(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (map[string]NativePeer, error) {
	consumers, err := environments.WorkloadIdentityServices(ctx, workspace, env)
	if err != nil {
		return nil, err
	}
	if env.Host == nil || env.Host.Gateway == "" {
		return nil, fmt.Errorf("environment %s declares no host.gateway", env.Name)
	}
	peers := map[string]NativePeer{}
	add := func(module, service string, consumer bool) error {
		namespace := env.ModuleNamespace(workspace, module)
		if namespace == "" {
			return fmt.Errorf("workload identity %s/%s has no resolved namespace: the environment declares no namespace", module, service)
		}
		account := workloadAccount(service)
		peers[resources.ServiceUnique(module, service)] = NativePeer{
			Module: module, Service: service, Account: account,
			SPIFFEID: env.Host.SPIFFEID(namespace, account), Consumer: consumer,
		}
		return nil
	}
	module, service, _ := strings.Cut(env.Host.Gateway, "/")
	if err = add(module, service, false); err != nil {
		return nil, err
	}
	for _, consumer := range consumers {
		if err = add(consumer.Module, consumer.Name, true); err != nil {
			return nil, err
		}
	}
	// And every module-identity service the composition runs: it presents a
	// projected token at its mint, so the run projects one and delivers its
	// presence and authority, as a cell's render does.
	refs, err := workspace.LoadServiceWithModules(ctx)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		unique := resources.ServiceUnique(ref.Module, ref.Name)
		if _, planned := peers[unique]; planned {
			continue
		}
		service, loadErr := workspace.LoadService(ctx, ref)
		if loadErr != nil {
			return nil, loadErr
		}
		if !service.ModuleIdentity {
			continue
		}
		if addErr := add(ref.Module, ref.Name, true); addErr != nil {
			return nil, addErr
		}
		peer := peers[unique]
		peer.TokenOnly = true
		peers[unique] = peer
	}
	return peers, nil
}

// NativePeerFiles is the peer list one workload trusts, by the cell rule: a
// consumer trusts the gateway, the gateway trusts every consumer.
func NativePeerFiles(host *environments.EnvironmentHost, peers map[string]NativePeer, unique string) ([]string, error) {
	plan := workloadPeers{}
	for key, peer := range peers {
		if peer.TokenOnly && key != unique {
			continue
		}
		plan[key] = workloadPeer{Account: peer.Account, SPIFFEID: peer.SPIFFEID, Consumer: peer.Consumer}
	}
	files, err := plan.files(host, unique)
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSpace(files["allowed-callers"]), "\n"), nil
}

// RequestsWorkloadIdentity reports whether a service declared the group.
func RequestsWorkloadIdentity(service *resources.Service) bool {
	return requestsWorkloadIdentity(service)
}
