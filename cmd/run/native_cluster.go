package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/localcluster"
	"github.com/codefly-dev/cli/pkg/nativecluster"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
)

// A native run of a composition that declares a host stands in for the cell
// the composition would be delivered to. What a cell gives a workload — an API
// server that reviews its projected token and reports its pod, an SVID, the
// peers it trusts, the host's mint address, a signed presence and authority
// delivered to the host, an envelope an administrator wrote — this run gives
// it the same way, from one in-process authority, so the host runs its one
// registry code path and a developer debugs on a laptop what a cell runs.
//
// It is deliberately only as strong as a laptop needs: the authority lives for
// the run and its keys are never written. The host reads its stand-in for the
// cluster — the API server's address, its CA, the host's own token and
// namespace — and its trust anchor from the two directories the run names
// (SOLUTION_HOST_LOCAL_CLUSTER, SOLUTION_HOST_LOCAL_TRUST_ANCHOR), under the
// run's own state, so nothing is written outside the workspace. The host
// refuses both settings in a pod, and nothing here makes the host believe it
// is in one.
//
// A workload starts before the host has been told about it, because its build
// is only known once it runs. A cell answers that with a restart; this run
// answers it by withholding the workload's projected token until the envelope
// admitting it is committed and its presence and authority are delivered.
// Without a token the workload waits, as it would for a kubelet that had not
// projected one yet. Once it has one, the host has the documents: until its
// registry has read them it answers "not established yet", which the workload
// retries, and never "refused".

type nativeCluster struct {
	authority *nativecluster.Authority
	workspace *resources.Workspace
	env       *environments.Environment
	flow      *orchestration.Flow
	fixture   string
	stateDir  string
	peers     map[string]gitops.NativePeer
	signer    solutionhost.Signer

	hostModule, hostService, hostNamespace string
	hostWorkload                           nativecluster.Workload
	hostServiceAccountDir, hostAnchorDir   string

	mu          sync.Mutex
	workloads   map[string]nativecluster.Workload // unique -> projected identity
	projections map[string]nativecluster.Projection
	documents   map[string]*gitops.NativeDocuments // unique -> last rendered
	// builds are every build this run approved: a rebuilt workload adds its
	// new build and keeps the old one approved for the run, so an envelope the
	// run writes only ever widens — a narrowing is a witnessed act, not a dev
	// loop's side effect.
	builds map[solutionhost.ImageDigest]bool
	// withheld are the workloads whose projected token is not written yet:
	// the ones this run delivers, until it has. See release.
	withheld map[string]bool
	// solutions caches which composed modules ship a solution manifest.
	solutions map[string]bool
	running   map[string]bool
	envelope  *gitops.EnvelopeWriter
	stop      chan struct{}
}

var (
	nativeClusterMu     sync.Mutex
	nativeClusterActive *nativeCluster
)

// attachNativeCluster wires the run as the composition's cluster when its
// environment declares a host and the run is native. It is a no-op otherwise.
func attachNativeCluster(ctx context.Context, flow *orchestration.Flow, workspace *resources.Workspace, env *environments.Environment, fixture string) error {
	if env == nil || env.Host == nil || runtimeContext == resources.RuntimeContextContainer {
		return nil
	}
	w := wool.Get(ctx).In("nativeCluster")
	nativeClusterMu.Lock()
	defer nativeClusterMu.Unlock()
	if nativeClusterActive != nil {
		nativeClusterActive.close()
		nativeClusterActive = nil
	}
	if err := env.Host.Validate(); err != nil {
		return err
	}
	peers, err := gitops.ResolveNativePeers(ctx, workspace, env)
	if err != nil {
		return err
	}
	signer, err := signing.LoadDeveloperKey()
	if err != nil {
		return fmt.Errorf("a native run delivers with the developer key, and none is held: %w", err)
	}
	authority, err := nativecluster.Start(ctx)
	if err != nil {
		return err
	}
	cluster := &nativeCluster{
		authority: authority, workspace: workspace, env: env, flow: flow, fixture: fixture,
		stateDir:    filepath.Join(workspace.Dir(), ".codefly", "native-cluster", env.Name),
		peers:       peers,
		signer:      signer,
		workloads:   map[string]nativecluster.Workload{},
		projections: map[string]nativecluster.Projection{},
		documents:   map[string]*gitops.NativeDocuments{},
		builds:      map[solutionhost.ImageDigest]bool{},
		withheld:    map[string]bool{},
		solutions:   map[string]bool{},
		running:     map[string]bool{},
		stop:        make(chan struct{}),
	}
	parts := strings.Split(env.Host.Delivery, "/")
	if len(parts) != 3 {
		authority.Close()
		return fmt.Errorf("host.delivery %q is not <module>/<service>/<endpoint>", env.Host.Delivery)
	}
	cluster.hostModule, cluster.hostService = parts[0], parts[1]
	cluster.hostNamespace = env.ModuleNamespace(workspace, cluster.hostModule)
	if cluster.hostNamespace == "" {
		authority.Close()
		return fmt.Errorf("the host module %s resolves to no namespace: the environment declares none", cluster.hostModule)
	}
	if err := cluster.projectHost(); err != nil {
		authority.Close()
		return err
	}
	if err := cluster.loadDelivered(); err != nil {
		authority.Close()
		return err
	}
	world := flow.World()
	world.RenderOwnedGroups = cluster.renderOwnedGroups
	world.RuntimeOverrides = cluster.runtimeOverrides
	flow.WithRunningHook(func(service string, _ int) {
		go cluster.onRunning(context.WithoutCancel(ctx), service)
	})
	go cluster.refreshTokens()
	host, port := authority.APIServer()
	w.Info("native cluster: the run is the composition's cluster", wool.Field("api-server", host+":"+port), wool.Field("ca", authority.CAFingerprint()[:16]))
	nativeClusterActive = cluster
	return nil
}

func stopNativeCluster() {
	nativeClusterMu.Lock()
	defer nativeClusterMu.Unlock()
	if nativeClusterActive != nil {
		nativeClusterActive.close()
		nativeClusterActive = nil
	}
}

func (cluster *nativeCluster) close() {
	select {
	case <-cluster.stop:
	default:
		close(cluster.stop)
	}
	_ = cluster.authority.Close()
}

func randomUID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw[:4]) + "-" + hex.EncodeToString(raw[4:6]) + "-" + hex.EncodeToString(raw[6:8]) + "-" + hex.EncodeToString(raw[8:10]) + "-" + hex.EncodeToString(raw[10:])
}

// projectHost writes what the kubelet projects into the host's pod and the
// trust anchor the host reads: its own token, the cluster CA, its namespace,
// and the verification policy bound to this run's CA and the developer key.
func (cluster *nativeCluster) projectHost() error {
	account := gitops.NativeWorkloadAccount(cluster.hostService)
	cluster.hostWorkload = nativecluster.Workload{
		Namespace: cluster.hostNamespace, Account: account,
		PodName: cluster.hostService + "-native", PodUID: randomUID(),
		Audience: "https://kubernetes.default.svc.cluster.local",
	}
	cluster.authority.RegisterPod(&nativecluster.Pod{
		Namespace: cluster.hostNamespace, ServiceAccount: account, Name: cluster.hostWorkload.PodName, UID: cluster.hostWorkload.PodUID,
		Container: cluster.hostService, Repository: "native/" + strings.ToLower(cluster.workspace.Name) + "/" + cluster.hostModule + "/" + cluster.hostService,
	})
	token, err := cluster.authority.Token(cluster.hostNamespace, account, cluster.hostWorkload.PodName, cluster.hostWorkload.PodUID, cluster.hostWorkload.Audience)
	if err != nil {
		return err
	}
	policy, err := localcluster.VerificationPolicy(cluster.authority.CAFingerprint(), cluster.env.Host)
	if err != nil {
		return err
	}
	cluster.hostServiceAccountDir = filepath.Join(cluster.stateDir, "host", "cluster")
	cluster.hostAnchorDir = filepath.Join(cluster.stateDir, "host", "delivery-trust")
	for _, dir := range []string{cluster.hostServiceAccountDir, cluster.hostAnchorDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	apiHost, apiPort := cluster.authority.APIServer()
	files := map[string][]byte{
		// The stand-in's own address, beside the material that authenticates
		// it: the host reads an https origin and nothing more.
		filepath.Join(cluster.hostServiceAccountDir, "server"):           []byte("https://" + net.JoinHostPort(apiHost, apiPort) + "\n"),
		filepath.Join(cluster.hostServiceAccountDir, "token"):            []byte(token),
		filepath.Join(cluster.hostServiceAccountDir, "ca.crt"):           cluster.authority.CAPEM(),
		filepath.Join(cluster.hostServiceAccountDir, "namespace"):        []byte(cluster.hostNamespace + "\n"),
		filepath.Join(cluster.hostAnchorDir, "verification_policy.json"): policy,
	}
	for path, content := range files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (cluster *nativeCluster) peerOf(service *resources.Service) (string, gitops.NativePeer, bool) {
	unique := resources.WithUnique(service).Unique()
	peer, ok := cluster.peers[unique]
	return unique, peer, ok
}

// runtimeOverrides is the process environment a cell gives a workload and a
// laptop does not: the host finds its API server, a module finds the mint.
func (cluster *nativeCluster) runtimeOverrides(service *resources.Service) map[string]string {
	unique, peer, ok := cluster.peerOf(service)
	out := map[string]string{}
	if ok && peer.TokenOnly {
		// A module-identity service reads its token from the path a cell's
		// render mounts unless told otherwise; this is the otherwise.
		projection, err := cluster.project(unique, peer)
		if err != nil {
			wool.Get(context.Background()).In("nativeCluster").Warn("cannot project the module's token", wool.Field("service", unique), wool.ErrField(err))
			return out
		}
		out[workloadTokenFileVariable] = projection.Files["TOKEN_FILE"]
		return out
	}
	if unique == resources.ServiceUnique(cluster.hostModule, cluster.hostService) {
		out["SOLUTION_HOST_LOCAL_CLUSTER"] = cluster.hostServiceAccountDir
		out["SOLUTION_HOST_LOCAL_TRUST_ANCHOR"] = cluster.hostAnchorDir
	}
	if ok && peer.Consumer && unique != cluster.env.Host.Gateway {
		port, err := cluster.flow.ProposedEndpointPort(context.Background(), cluster.env.Host.Gateway, "workload", "http")
		if err != nil {
			wool.Get(context.Background()).In("nativeCluster").Warn("cannot resolve the gateway's workload listener for the mint address", wool.ErrField(err))
		} else {
			out["CODEFLY__CREDENTIAL_MINT_URL"] = fmt.Sprintf("https://localhost:%d/modules/_work-context", port)
			// Every hop a module makes to the host carries a bearer or a minted
			// capability, so it goes to the gateway's workload listener: TLS
			// under the gateway's SVID, the module's SVID presented back.
			out["GATEWAY_URL"] = fmt.Sprintf("https://localhost:%d", port)
		}
	}
	return out
}

// renderOwnedGroups projects the identity a cell's render would project for a
// service that declares workload-identity, and the authority a module declares.
func (cluster *nativeCluster) renderOwnedGroups(ctx context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
	unique, peer, ok := cluster.peerOf(service)
	if !ok || !gitops.RequestsWorkloadIdentity(service) {
		return nil, nil
	}
	projection, err := cluster.project(unique, peer)
	if err != nil {
		return nil, err
	}
	identity := &basev0.ConfigurationInformation{Name: environments.WorkloadIdentityConfiguration}
	keys := make([]string, 0, len(projection.Files))
	for key := range projection.Files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		identity.ConfigurationValues = append(identity.ConfigurationValues, &basev0.ConfigurationValue{Key: key, Value: projection.Files[key]})
	}
	out := []*basev0.Configuration{{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{identity}}}
	for _, dependency := range service.WorkspaceConfigurationDependencies {
		if dependency != environments.ModuleAuthorityConfiguration {
			continue
		}
		token, err := cluster.internalToken(ctx)
		if err != nil {
			return nil, err
		}
		authority := &basev0.ConfigurationInformation{Name: environments.ModuleAuthorityConfiguration, ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: "PRINCIPAL", Value: peer.SPIFFEID},
			{Key: "AUDIENCE", Value: cluster.env.Host.ModuleAudience},
			{Key: "PROJECTION_AUDIENCE", Value: cluster.env.Host.Audience},
			{Key: "INTERNAL_TOKEN", Value: token, Secret: true},
		}}
		out = append(out, &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{authority}})
	}
	return out, nil
}

// internalToken is the value module-authority/INTERNAL_TOKEN aliases in a
// cell: internal-auth/CODEFLY_INTERNAL_TOKEN as the host's gateway resolves
// it, the secret a module presents on the hop the gateway verifies.
func (cluster *nativeCluster) internalToken(ctx context.Context) (string, error) {
	gateway, err := cluster.flow.ServiceFromUnique(cluster.env.Host.Gateway)
	if err != nil {
		return "", err
	}
	info, err := cluster.flow.World().ResolveWorkspaceGroupFor(ctx, gateway, "internal-auth")
	if err != nil {
		return "", err
	}
	for _, value := range info.ConfigurationValues {
		if value.Key == "CODEFLY_INTERNAL_TOKEN" && value.Value != "" {
			return value.Value, nil
		}
	}
	return "", fmt.Errorf("module-authority aliases internal-auth/CODEFLY_INTERNAL_TOKEN, and the gateway %s resolves no value for it", cluster.env.Host.Gateway)
}

func (cluster *nativeCluster) project(unique string, peer gitops.NativePeer) (nativecluster.Projection, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if projection, done := cluster.projections[unique]; done {
		return projection, nil
	}
	callers, err := gitops.NativePeerFiles(cluster.env.Host, cluster.peers, unique)
	if err != nil {
		return nativecluster.Projection{}, err
	}
	namespace := cluster.env.ModuleNamespace(cluster.workspace, peer.Module)
	workload := nativecluster.Workload{
		Namespace: namespace, Account: peer.Account, SPIFFEID: peer.SPIFFEID,
		PodName: peer.Service + "-native", PodUID: randomUID(), Audience: cluster.env.Host.Audience,
		AllowedCallers: callers, MintPeers: callers, GatewayPeers: callers,
	}
	projection, err := cluster.authority.Project(filepath.Join(cluster.stateDir, "identities", peer.Module, peer.Service), &workload)
	if err != nil {
		return nativecluster.Projection{}, err
	}
	cluster.workloads[unique] = workload
	cluster.projections[unique] = projection
	if cluster.delivers(peer) && cluster.isSolution(peer.Module) {
		// Not projected yet: see the note on this type. A stale token from an
		// earlier run is removed too, since that run's authority is gone.
		//
		// A solution only. A solution mints at boot and waits for a token that
		// is not there yet; a module checks at boot that its token can be
		// opened and mints when it first needs to, so withholding it ends the
		// module rather than delaying it.
		if err := os.Remove(projection.Files["TOKEN_FILE"]); err != nil && !os.IsNotExist(err) {
			return nativecluster.Projection{}, err
		}
		cluster.withheld[unique] = true
	}
	// The pod exists from projection on; its build is known once it runs.
	cluster.authority.RegisterPod(&nativecluster.Pod{
		Namespace: namespace, ServiceAccount: peer.Account, Name: workload.PodName, UID: workload.PodUID,
		Container: peer.Service, Repository: "native/" + strings.ToLower(cluster.workspace.Name) + "/" + strings.ToLower(peer.Module) + "/" + strings.ToLower(peer.Service),
	})
	return projection, nil
}

// refreshTokens re-issues every projected token before it expires, as the
// kubelet rotates a projected token.
func (cluster *nativeCluster) refreshTokens() {
	ticker := time.NewTicker(nativecluster.TokenLifetime / 3)
	defer ticker.Stop()
	for {
		select {
		case <-cluster.stop:
			return
		case <-ticker.C:
			cluster.mu.Lock()
			for unique := range cluster.workloads {
				if cluster.withheld[unique] {
					continue
				}
				workload := cluster.workloads[unique]
				_ = cluster.authority.RefreshToken(cluster.projections[unique].Files["TOKEN_FILE"], &workload)
			}
			cluster.mu.Unlock()
			_ = cluster.authority.RefreshToken(filepath.Join(cluster.hostServiceAccountDir, "token"), &cluster.hostWorkload)
		}
	}
}

// onRunning is the cell's delivery, for one workload that just started: pin
// the build it runs, write the envelope the host admits it under, and deliver
// its presence and authority.
func (cluster *nativeCluster) onRunning(ctx context.Context, unique string) {
	w := wool.Get(ctx).In("nativeCluster.deliver", wool.Field("service", unique))
	peer, ok := cluster.peers[unique]
	if !ok || !cluster.delivers(peer) {
		return
	}
	service, err := cluster.flow.ServiceFromUnique(unique)
	if err != nil {
		w.Warn("cannot resolve the running service", wool.ErrField(err))
		return
	}
	if err := cluster.deliver(ctx, unique, peer, service); err != nil {
		w.Error("delivery to the host failed", wool.ErrField(err))
		return
	}
}

// deliveredDocuments is what the run remembers of one workload's last
// delivery, across runs.
type deliveredDocuments struct {
	Unique    string                  `json:"unique"`
	Documents *gitops.NativeDocuments `json:"documents"`
}

func (cluster *nativeCluster) deliveredDir() string {
	return filepath.Join(cluster.stateDir, "delivered")
}

func deliveredFileName(unique string) string {
	return strings.NewReplacer("/", "__", string(filepath.Separator), "__").Replace(unique) + ".json"
}

// persistDelivered records a workload's documents as the host now holds them.
//
// Every document names the envelope revision it was rendered against, and
// the host refuses a reconcile pass on a document that names any other. So
// when this run writes a new revision — a rebuilt workload's build approved —
// it must re-deliver every document the host holds from it, including those
// of workloads it has not started yet in this run. Those are only known from
// what an earlier run delivered.
func (cluster *nativeCluster) persistDelivered(unique string, documents *gitops.NativeDocuments) error {
	if err := os.MkdirAll(cluster.deliveredDir(), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(deliveredDocuments{Unique: unique, Documents: documents})
	if err != nil {
		return err
	}
	path := filepath.Join(cluster.deliveredDir(), deliveredFileName(unique))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadDelivered reads what earlier runs delivered for the workloads this run
// still composes, so a new envelope revision re-delivers them too. A record
// for a workload the composition no longer declares is left alone.
func (cluster *nativeCluster) loadDelivered() error {
	entries, err := os.ReadDir(cluster.deliveredDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(cluster.deliveredDir(), entry.Name()))
		if err != nil {
			return err
		}
		var record deliveredDocuments
		if err := json.Unmarshal(body, &record); err != nil || record.Documents == nil || record.Documents.Presence == nil {
			// Not a record this run wrote: ignore it rather than deliver it.
			continue
		}
		peer, composed := cluster.peers[record.Unique]
		if !composed || !cluster.delivers(peer) {
			continue
		}
		cluster.documents[record.Unique] = record.Documents
		if record.Documents.Digest != "" {
			cluster.builds[record.Documents.Digest] = true
		}
	}
	return nil
}

// workloadTokenFileVariable names the projected token's path to a process
// that cannot read it from the path a pod mounts it at.
const workloadTokenFileVariable = "CODEFLY__WORKLOAD_TOKEN_FILE" //nolint:gosec // G101: the name of a path variable, not a credential

// isSolution reports whether a composed module ships a solution manifest.
// Called with the lock held.
func (cluster *nativeCluster) isSolution(module string) bool {
	if known, ok := cluster.solutions[module]; ok {
		return known
	}
	loaded, err := cluster.workspace.LoadModuleFromName(context.Background(), module)
	solution := false
	if err == nil {
		manifest, manifestErr := solutionrun.ModuleSolution(loaded)
		solution = manifestErr == nil && manifest != nil
	}
	cluster.solutions[module] = solution
	return solution
}

// delivers reports whether this run delivers presence and authority for a
// peer: a workload that asked for an identity and is not the host's own.
func (cluster *nativeCluster) delivers(peer gitops.NativePeer) bool {
	return peer.Consumer && peer.Module != cluster.hostModule
}

// release projects the token of every workload whose documents the host now
// holds. Called with the lock held, after they are delivered.
func (cluster *nativeCluster) release(uniques []string) error {
	for _, unique := range uniques {
		if !cluster.withheld[unique] {
			continue
		}
		workload := cluster.workloads[unique]
		if err := cluster.authority.RefreshToken(cluster.projections[unique].Files["TOKEN_FILE"], &workload); err != nil {
			return fmt.Errorf("%s: project the workload's token: %w", unique, err)
		}
		delete(cluster.withheld, unique)
	}
	return nil
}

func (cluster *nativeCluster) deliver(ctx context.Context, unique string, peer gitops.NativePeer, service *resources.Service) error {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	workload, projected := cluster.workloads[unique]
	if !projected {
		return fmt.Errorf("%s is running with no projected identity", unique)
	}
	module, err := cluster.workspace.LoadModuleFromName(ctx, peer.Module)
	if err != nil {
		return err
	}
	var services []*resources.Service
	for _, reference := range module.ServiceReferences {
		loaded, loadErr := cluster.workspace.LoadService(ctx, &resources.ServiceWithModule{Name: reference.Name, Module: peer.Module})
		if loadErr != nil {
			return loadErr
		}
		services = append(services, loaded)
	}
	executable := filepath.Join(service.Dir(), ".cache", "native", "main")
	digest, err := gitops.ExecutableDigest(executable)
	if err != nil {
		return err
	}
	namespace := cluster.env.ModuleNamespace(cluster.workspace, peer.Module)
	cluster.authority.RegisterPod(&nativecluster.Pod{
		Namespace: namespace, ServiceAccount: peer.Account, Name: workload.PodName, UID: workload.PodUID,
		Container: peer.Service, Repository: "native/" + strings.ToLower(cluster.workspace.Name) + "/" + strings.ToLower(peer.Module) + "/" + strings.ToLower(peer.Service),
		Digest: string(digest),
	})
	gatewayURL, err := cluster.flow.GetAddressForEndpoint(ctx, strings.Split(cluster.env.Host.Gateway, "/")[0], strings.Split(cluster.env.Host.Gateway, "/")[1], "rest")
	if err != nil {
		return fmt.Errorf("the host gateway's rest endpoint is unresolved: %w", err)
	}
	deliveryURL, err := cluster.flow.GetAddressForEndpoint(ctx, cluster.hostModule, cluster.hostService, strings.Split(cluster.env.Host.Delivery, "/")[2])
	if err != nil {
		return fmt.Errorf("the host's delivery endpoint is unresolved: %w", err)
	}
	if cluster.envelope == nil {
		administrator, adminErr := cluster.fixtureAdministrator(ctx)
		if adminErr != nil {
			return adminErr
		}
		cluster.envelope = &gitops.EnvelopeWriter{GatewayURL: gatewayURL, FixtureProvider: administrator.Provider, FixtureToken: administrator.Token}
	}
	deliverer := &gitops.NativeDeliverer{
		Workspace: cluster.workspace, Env: cluster.env, Signer: cluster.signer,
		StateDir: filepath.Join(cluster.stateDir, "delivery"), DeliveryURL: deliveryURL,
		Token: func(namespace string) (string, error) {
			pod := nativecluster.Pod{Namespace: namespace, ServiceAccount: "delivery", Name: "delivery-native", UID: randomUID()}
			if existing, ok := cluster.authority.Pod(namespace, "delivery"); ok {
				pod = existing
			} else {
				cluster.authority.RegisterPod(&pod)
			}
			return cluster.authority.Token(namespace, "delivery", pod.Name, pod.UID, cluster.env.Host.Audience)
		},
	}
	native := gitops.NativeWorkload{
		Module: module, Service: service, Services: services, Namespace: namespace, Account: peer.Account,
		SPIFFEID: peer.SPIFFEID, Subject: cluster.subject(peer), Executable: executable,
	}
	rendered, err := deliverer.Render(ctx, &native)
	if err != nil {
		return err
	}
	previous, hadPrevious := cluster.documents[unique]
	cluster.documents[unique] = rendered
	cluster.builds[rendered.Digest] = true
	// A workload whose documents the host refuses must not take every other
	// workload's next delivery down with it: every envelope write carries
	// every principal, so its documents are put back as they were.
	committed := false
	defer func() {
		if committed {
			return
		}
		if hadPrevious {
			cluster.documents[unique] = previous
		} else {
			delete(cluster.documents, unique)
		}
	}()
	grants, err := cluster.grants()
	if err != nil {
		return err
	}
	revision, commit, err := cluster.envelope.Plan(ctx, func(revision uint64, current []byte) ([]byte, error) {
		// The host keeps every build it already approved: a developer's next
		// build adds to the envelope and withdraws nothing, so writing it is
		// a grant the host applies without a witness. Withdrawing a build is
		// a narrowing, a witnessed act, and not this loop's to perform.
		for _, build := range gitops.ApprovedBuilds(current) {
			cluster.builds[build] = true
		}
		var builds []solutionhost.ImageDigest
		var principals []gitops.EnvelopePrincipal
		for build := range cluster.builds {
			builds = append(builds, build)
		}
		for key, documents := range cluster.documents {
			if documents.Authority == nil {
				continue
			}
			principals = append(principals, gitops.EnvelopePrincipal{
				Prefix: documents.Principal, Bindings: documents.Bindings, Ceilings: documents.Ceilings,
				SPIFFEID: cluster.peers[key].SPIFFEID, Container: cluster.peers[key].Service, Grant: grants[documents.Principal],
			})
		}
		sort.Slice(principals, func(i, j int) bool { return principals[i].Prefix < principals[j].Prefix })
		next, buildErr := gitops.BuildEnvelope(revision, builds, principals)
		if buildErr != nil {
			return nil, buildErr
		}
		// And every principal the host already grants that this run has not
		// (yet) started: leaving one out would withdraw it.
		return gitops.KeepGrantedPrincipals(next, current)
	})
	if err != nil {
		return fmt.Errorf("plan the module envelope: %w", err)
	}
	// The envelope first: a document naming a revision the host does not hold
	// yet is outside its ceiling, and the host refuses the whole pass on it.
	if err := commit(ctx); err != nil {
		return fmt.Errorf("write the module envelope: %w", err)
	}
	committed = true
	deliverer.EnvelopeRevision = revision
	w := wool.Get(ctx).In("nativeCluster.deliver")
	// Every delivered document names the envelope revision, so a new revision
	// re-delivers every workload, as a cell's re-review would.
	keys := make([]string, 0, len(cluster.documents))
	for key := range cluster.documents {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		documents := cluster.documents[key]
		documents.Presence.EnvelopeRevision = revision
		if documents.Authority != nil {
			documents.Authority.EnvelopeRevision = revision
		}
		receipts, err := deliverer.Deliver(ctx, documents)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if err := cluster.persistDelivered(key, documents); err != nil {
			return fmt.Errorf("%s: remember what was delivered: %w", key, err)
		}
		for _, receipt := range receipts {
			w.Info("delivered", wool.Field("service", key), wool.Field("kind", receipt.Kind), wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation), wool.Field("envelope", revision))
		}
	}
	// Only now does a workload get its token: the host holds its documents,
	// so the first ask it can make is one the host answers "not established
	// yet" until its registry has read them, never "refused".
	return cluster.release(keys)
}

// fixtureAdministrator is the platform administrator the selected fixture
// seeds: the one who writes the envelope, as a cell's administrator would.
// Read from the host module's fixture file, the host's own declaration of it.
type fixtureAdministrator struct {
	Provider string
	Token    string
}

func (cluster *nativeCluster) fixtureAdministrator(ctx context.Context) (fixtureAdministrator, error) {
	module, err := cluster.workspace.LoadModuleFromName(ctx, cluster.hostModule)
	if err != nil {
		return fixtureAdministrator{}, err
	}
	path := filepath.Join(module.Dir(), "fixtures", cluster.fixture+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return fixtureAdministrator{}, fmt.Errorf("the envelope is written by the fixture's platform administrator, and the host declares no fixture %q: %w", cluster.fixture, err)
	}
	var document struct {
		Users []struct {
			Role         string `yaml:"role"`
			Provider     string `yaml:"provider"`
			ProviderID   string `yaml:"provider_id"`
			FixtureToken string `yaml:"fixture_token"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fixtureAdministrator{}, fmt.Errorf("read %s: %w", path, err)
	}
	for _, user := range document.Users {
		if user.Role != "super_admin" {
			continue
		}
		token := user.FixtureToken
		if token == "" {
			token = user.ProviderID
		}
		return fixtureAdministrator{Provider: user.Provider, Token: token}, nil
	}
	return fixtureAdministrator{}, fmt.Errorf("fixture %q seeds no super_admin, so nobody can write the envelope", cluster.fixture)
}

// subject is the principal the environment declares for a workload: the one
// the host expects the identity to carry.
func (cluster *nativeCluster) subject(peer gitops.NativePeer) string {
	if identity := cluster.env.WorkloadIdentityFor(peer.Module, peer.Service); identity != nil && identity.Principal != "" {
		return identity.Principal
	}
	return "system:serviceaccount:" + cluster.env.ModuleNamespace(cluster.workspace, peer.Module) + ":" + peer.Account
}

// grants reads the composition's envelope policy: configurations/<env>/module-envelope.json,
// {"principals": {"<prefix>": <grant>}} — what the platform administrator
// grants each module principal, in the host's own grant shape.
func (cluster *nativeCluster) grants() (map[string]gitops.EnvelopeGrant, error) {
	path := filepath.Join(cluster.workspace.Dir(), "configurations", cluster.env.Name, "module-envelope.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]gitops.EnvelopeGrant{}, nil
	}
	if err != nil {
		return nil, err
	}
	var document struct {
		Principals map[string]json.RawMessage `json:"principals"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	out := map[string]gitops.EnvelopeGrant{}
	for prefix, grant := range document.Principals {
		out[prefix] = gitops.EnvelopeGrant(grant)
	}
	return out, nil
}
