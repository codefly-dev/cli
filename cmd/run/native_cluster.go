package run

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/composition"
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
	nativeidentity "github.com/codefly-dev/core/workloadidentity/native"
	"gopkg.in/yaml.v3"
)

// A native run of a composition that declares a host stands in for the cell
// the composition would be delivered to. What a cell gives a workload — a
// credential the host can attest, a signed module manifest delivered to the
// host, an envelope an administrator wrote — this run gives it the same way,
// from one in-process authority, so the host runs its one registry code path
// and a developer debugs on a laptop what a cell runs. What a cell's mesh gives a workload — its transport
// identity, mTLS on every hop — a laptop does not have and this run does not
// stand in for: every hop is plain HTTP, and every service but the host
// gateway is expected to listen on loopback, where the carrier the run writes
// for it says it lives (loopback.go checks).
//
// It is deliberately only as strong as a laptop needs: the issuer lives for
// the run and its key is never written. The host reads the run's
// workload-identity anchor and its delivery trust anchor from the two paths the
// run names (CODEFLY_NATIVE_IDENTITY_ANCHOR, SOLUTION_HOST_LOCAL_TRUST_ANCHOR),
// under the run's own state, so nothing is written outside the workspace. The
// host refuses both settings in a pod, and nothing here imitates a cluster:
// there is no API server, CA, pod or ServiceAccount, only credentials the run
// signs for the processes it started (core workloadidentity/native).
//
// Every identity workload gets its credential when it is projected, before it
// starts, naming its declared container and nothing else: the host mints on
// the attested account, an installation and the admin's grant, never on a
// build. A module's manifest is delivered once its identity workloads have
// started — every started one listed — and again, at the next generation,
// whenever that set changes. Until the host has applied it, a mint answers
// "not established yet", which the workload retries.

type nativeCluster struct {
	authority  *nativecluster.Authority
	workspace  *resources.Workspace
	env        *environments.Environment
	flow       *orchestration.Flow
	fixture    string
	stateDir   string
	identities map[string]gitops.NativeIdentity
	signer     solutionhost.Signer

	hostModule, hostService, hostNamespace string

	// composition guards workspace and identities: the components this run
	// composes, which admit extends with the ones a reloaded workspace adds
	// while the flow keeps reading them. Its own lock, because deliver
	// holds mu while it reads both.
	composition                       sync.RWMutex
	hostIdentityAnchor, hostAnchorDir string

	mu          sync.Mutex
	workloads   map[string]nativecluster.Workload // unique -> projected identity
	projections map[string]nativecluster.Projection
	// running are the identity workloads that reached Running: the ones a
	// module's manifest lists.
	running map[string]bool
	// delivered is the last manifest delivered per module, a withdrawal
	// included.
	delivered map[string]*solutionhost.ModuleManifest
	// pendingWithdrawals are modules whose withdrawal the host has not applied
	// yet: retried on the next composition refresh, even with no gone roots.
	pendingWithdrawals map[string]bool
	envelope           *gitops.EnvelopeWriter
	stop               chan struct{}
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
	identities, err := gitops.ResolveNativeIdentities(ctx, workspace, env)
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
		stateDir:           filepath.Join(workspace.Dir(), ".codefly", "native-cluster", env.Name),
		identities:         identities,
		signer:             signer,
		workloads:          map[string]nativecluster.Workload{},
		projections:        map[string]nativecluster.Projection{},
		running:            map[string]bool{},
		delivered:          map[string]*solutionhost.ModuleManifest{},
		pendingWithdrawals: map[string]bool{},
		stop:               make(chan struct{}),
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
	world := flow.World()
	world.RenderOwnedGroups = cluster.renderOwnedGroups
	world.RuntimeOverrides = cluster.runtimeOverrides
	flow.WithRunningHook(func(service string, _ int) {
		go cluster.onRunning(context.WithoutCancel(ctx), service)
	})
	go cluster.refreshTokens()
	w.Info("native run: the run attests the composition's workloads", wool.Field("anchor", authority.AnchorFingerprint()[:16]))
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

// projectHost writes what the host reads to verify this run: the anchor every
// workload credential is checked against, and the delivery trust anchor — a
// verification policy bound to that anchor and admitting the developer key.
func (cluster *nativeCluster) projectHost() error {
	// The native run owns the anchor it binds the policy to and the signer it
	// admits: its own delivery key, which no workspace lists.
	policy, err := localcluster.NativeVerificationPolicy(cluster.authority.AnchorFingerprint(), cluster.signer, cluster.env.Host)
	if err != nil {
		return err
	}
	cluster.hostIdentityAnchor = filepath.Join(cluster.stateDir, "host", "identity-anchor.json")
	cluster.hostAnchorDir = filepath.Join(cluster.stateDir, "host", "delivery-trust")
	for _, dir := range []string{filepath.Dir(cluster.hostIdentityAnchor), cluster.hostAnchorDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := cluster.authority.Anchor().WriteFile(cluster.hostIdentityAnchor); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cluster.hostAnchorDir, "verification_policy.json"), policy, 0o600)
}

// composed is the workspace this run composes, as admitted so far.
func (cluster *nativeCluster) composed() *resources.Workspace {
	cluster.composition.RLock()
	defer cluster.composition.RUnlock()
	return cluster.workspace
}

// identity is the identity this run projects for a workload, by unique.
func (cluster *nativeCluster) identity(unique string) (gitops.NativeIdentity, bool) {
	cluster.composition.RLock()
	defer cluster.composition.RUnlock()
	identity, ok := cluster.identities[unique]
	return identity, ok
}

// envelopeFor is the run's envelope writer, pointed at the gateway as it
// answers now: the gateway can be restarted on another port during the run,
// and a writer kept on its first address would fail every envelope write
// after that, before the endpoint projection the delivery ends with.
func (cluster *nativeCluster) envelopeFor(ctx context.Context, gatewayURL string) (*gitops.EnvelopeWriter, error) {
	if cluster.envelope != nil {
		cluster.envelope.GatewayURL = gatewayURL
		return cluster.envelope, nil
	}
	administrator, err := cluster.fixtureAdministrator(ctx)
	if err != nil {
		return nil, err
	}
	cluster.envelope = &gitops.EnvelopeWriter{GatewayURL: gatewayURL, FixtureProvider: administrator.Provider, FixtureToken: administrator.Token}
	return cluster.envelope, nil
}

func (cluster *nativeCluster) identityOf(service *resources.Service) (string, gitops.NativeIdentity, bool) {
	unique := resources.WithUnique(service).Unique()
	identity, ok := cluster.identity(unique)
	return unique, identity, ok
}

// admit introduces into the running composition the components a reloaded
// workspace composes that this run did not know at start. Their identities
// are planned by the rule attach used (ResolveNativeIdentities) and added;
// the components already known may neither change nor disappear: a running
// principal whose identity changed would be a replacement, and one dropped
// would be a withdrawal, a narrowing the host rightly refuses from a
// development loop, so both refuse by name and leave the run as it was. The
// run's authority — its CA, its API server, the host's token and trust
// anchor — is untouched: the host keeps its process, and an admitted
// component is projected, delivered and enveloped through the same path as
// any workload reaching Running. It returns the uniques admitted, sorted.
func (cluster *nativeCluster) admit(ctx context.Context, workspace *resources.Workspace) ([]string, error) {
	identities, err := gitops.ResolveNativeIdentities(ctx, workspace, cluster.env)
	if err != nil {
		return nil, err
	}
	cluster.composition.Lock()
	defer cluster.composition.Unlock()
	knownUniques := make([]string, 0, len(cluster.identities))
	for unique := range cluster.identities {
		knownUniques = append(knownUniques, unique)
	}
	sort.Strings(knownUniques)
	for _, unique := range knownUniques {
		known := cluster.identities[unique]
		reloaded, still := identities[unique]
		switch {
		case !still:
			return nil, fmt.Errorf("the reloaded composition no longer composes %s, which this run delivers as %s; a run never withdraws a component, stop the run to remove it", unique, known.SPIFFEID)
		case reloaded != known:
			return nil, fmt.Errorf("%s runs as %s and the reloaded composition names it %s; a running principal is never replaced, stop the run to change it", unique, known.SPIFFEID, reloaded.SPIFFEID)
		}
	}
	var added []string
	for unique := range identities {
		if _, known := cluster.identities[unique]; !known {
			added = append(added, unique)
		}
	}
	sort.Strings(added)
	for _, unique := range added {
		cluster.identities[unique] = identities[unique]
	}
	cluster.workspace = workspace
	return added, nil
}

// runtimeOverrides is the process environment a cell gives a workload and a
// laptop does not: the host finds its API server, a module-identity service
// finds its projected token. Where a workload reaches the host's gateway is
// not here: it is the workspace value the composition points at the gateway's
// `workload` endpoint, which the run resolves like every other
// `${endpoint:…}` reference, at the address this run allocated.
func (cluster *nativeCluster) runtimeOverrides(service *resources.Service) map[string]string {
	unique, identity, ok := cluster.identityOf(service)
	out := map[string]string{}
	if ok {
		// A module-identity service reads its token from the path a cell's
		// render mounts unless told otherwise; this is the otherwise.
		projection, err := cluster.project(unique, identity)
		if err != nil {
			wool.Get(context.Background()).In("nativeCluster").Warn("cannot project the module's token", wool.Field("service", unique), wool.ErrField(err))
			return out
		}
		out[workloadTokenFileVariable] = projection.TokenFile
	}
	if unique == resources.ServiceUnique(cluster.hostModule, cluster.hostService) {
		out[nativeidentity.AnchorEnvironment] = cluster.hostIdentityAnchor
		out["SOLUTION_HOST_LOCAL_TRUST_ANCHOR"] = cluster.hostAnchorDir
	}
	return out
}

// renderOwnedGroups projects the authority a cell's render would project for
// a service that declares module-authority: its mesh principal, the host's
// audiences and the internal token the gateway verifies.
func (cluster *nativeCluster) renderOwnedGroups(ctx context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
	var out []*basev0.Configuration
	// The delivery host turns its surface on from its federation keys, which
	// a cell's composition declares for the environment and this run derives
	// from the host block: handed last, so they settle over what the host's
	// own authored group leaves empty.
	if unique := resources.WithUnique(service).Unique(); unique == resources.ServiceUnique(cluster.hostModule, cluster.hostService) &&
		slices.Contains(service.WorkspaceConfigurationDependencies, environments.FederationConfiguration) {
		group, err := federationGroup(cluster.env.Host)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", unique, err)
		}
		out = append(out, group)
	}
	if slices.Contains(service.WorkspaceConfigurationDependencies, environments.ArtifactCacheConfiguration) {
		group, err := artifactCacheGroup(cluster.composed().Dir())
		if err != nil {
			return nil, err
		}
		out = append(out, group)
	}
	if !slices.Contains(service.WorkspaceConfigurationDependencies, environments.ModuleAuthorityConfiguration) {
		return out, nil
	}
	unique, identity, ok := cluster.identityOf(service)
	if !ok {
		return nil, fmt.Errorf("%s declares module-authority but not module-identity, so it has no principal to be handed", unique)
	}
	token, err := cluster.internalToken(ctx)
	if err != nil {
		return nil, err
	}
	authority := &basev0.ConfigurationInformation{Name: environments.ModuleAuthorityConfiguration, ConfigurationValues: []*basev0.ConfigurationValue{
		{Key: "PRINCIPAL", Value: identity.SPIFFEID},
		{Key: "AUDIENCE", Value: cluster.env.Host.ModuleAudience},
		{Key: "PROJECTION_AUDIENCE", Value: cluster.env.Host.Audience},
		{Key: "INTERNAL_TOKEN", Value: token, Secret: true},
	}}
	return append(out, &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{authority}}), nil
}

// federationGroup is the federation surface a native run hands the delivery
// host: the coordinate it answers for, the ownership domain it accepts a
// binding under (the one the run's own policy admits the run's signer for)
// and the trust policy, pinned keys — the three values a cell's composition
// declares in the host's federation group, derived here from the host block
// the manifests this run delivers are stamped with, so the host accepts what
// the run signs and nothing else. The host's registration secrets stay in its
// authored group, untouched.
func federationGroup(host *environments.EnvironmentHost) (*basev0.Configuration, error) {
	if host == nil || host.Coordinate == "" || host.Domain == "" {
		return nil, fmt.Errorf("the delivery host answers for the environment's host.coordinate under host.domain, and the environment declares no host with both")
	}
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: environments.FederationConfiguration, ConfigurationValues: []*basev0.ConfigurationValue{
			{Key: environments.FederationHostCoordinateKey, Value: host.Coordinate},
			{Key: environments.FederationHostOwnershipDomainsKey, Value: host.Domain},
			{Key: environments.FederationHostTrustPolicyKey, Value: environments.FederationHostTrustPolicyKeys},
		},
	}}}, nil
}

// artifactCacheGroup is the artifact-cache group a native run hands a service
// that declares it: ROOT, the absolute path of the workspace's artifact cache,
// the read-only source a host's native bundle adapter reads a frontend bundle
// from by the digest the manifest names. A cell's render projects nothing for
// the group, so the value exists in a local run alone.
func artifactCacheGroup(workspaceDir string) (*basev0.Configuration, error) {
	// Created here, since the host opens the root read-only when it starts,
	// before the first bundle is retained in it.
	root, err := composition.EnsureArtifactCacheDir(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("prepare the artifact cache of %s: %w", workspaceDir, err)
	}
	return &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
		Name: environments.ArtifactCacheConfiguration, ConfigurationValues: []*basev0.ConfigurationValue{{Key: environments.ArtifactCacheRootKey, Value: root}},
	}}}, nil
}

// frontendBundle archives the bundle a component's frontend build wrote
// beside its entry service, in core's canonical form, and retains it in the
// workspace's artifact cache under its digest — the read-only source the
// host's native adapter reads bundles from — so the manifest names the bytes
// the host verifies. A component that declares a page and built no bundle is
// refused by name, with what to build: a host admits no new frontend without
// its signed payload, and a mutable fallback is never delivered in its place.
// A headless component needs no bundle, and a leftover one is not its page.
func (cluster *nativeCluster) frontendBundle(ctx context.Context, module *resources.Module) (*solutionhost.FrontendPayload, error) {
	frontend, err := solutionrun.DeclaredFrontend(ctx, module)
	if err != nil || frontend == nil {
		return nil, err
	}
	root, built, err := solutionrun.FrontendBundle(module)
	if err != nil {
		return nil, err
	}
	if !built {
		source := filepath.Join(module.ServicesDir(), "fe-remote")
		return nil, fmt.Errorf("module %s declares a frontend (%s beside its service-entry) and its bundle is not built: %s holds no %s; a host admits no frontend without its signed bundle, so build the frontend from %s with its own build workflow and start the run again",
			module.Name, solutionrun.FrontendFileName, filepath.Join(module.ServicesDir(), filepath.FromSlash(solutionrun.FrontendBundleDir)), solutionrun.FrontendBundleManifest, source)
	}
	digest, _, err := composition.RetainCanonicalArchive(ctx, cluster.composed().Dir(), root)
	if err != nil {
		return nil, fmt.Errorf("retain the frontend bundle of %s: %w", module.Name, err)
	}
	return &solutionhost.FrontendPayload{Artifact: gitops.FrontendArtifactName, Digest: solutionhost.PayloadDigest(digest), MediaType: composition.CanonicalArchiveMediaType}, nil
}

// internalToken is the value module-authority/INTERNAL_TOKEN aliases in a
// cell: internal-auth/CODEFLY_INTERNAL_TOKEN as the host's gateway resolves
// it, the secret a module presents on the hop the gateway verifies.
func (cluster *nativeCluster) internalToken(ctx context.Context) (string, error) {
	if cluster.env.Host.Gateway == "" {
		return "", fmt.Errorf("module-authority aliases the gateway's internal-auth/CODEFLY_INTERNAL_TOKEN, and the environment names no host.gateway")
	}
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

func (cluster *nativeCluster) project(unique string, identity gitops.NativeIdentity) (nativecluster.Projection, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if projection, done := cluster.projections[unique]; done {
		return projection, nil
	}
	namespace := cluster.env.ModuleNamespace(cluster.composed(), identity.Module)
	workload := nativecluster.Workload{
		Namespace: namespace, Account: identity.Account,
		PodName: identity.Service + "-native", PodUID: randomUID(), Audience: cluster.env.Host.Audience,
	}
	// The process is registered before its first credential is written, so
	// that credential already names its declared container: the one a cell's
	// render projects the credential into, and nothing else.
	cluster.authority.RegisterPod(&nativecluster.Pod{
		Namespace: namespace, ServiceAccount: identity.Account, Name: workload.PodName, UID: workload.PodUID, Container: identity.Service,
	})
	projection, err := cluster.authority.Project(filepath.Join(cluster.stateDir, "identities", identity.Module, identity.Service), &workload)
	if err != nil {
		return nativecluster.Projection{}, err
	}
	cluster.workloads[unique] = workload
	cluster.projections[unique] = projection
	return projection, nil
}

// refreshTokens re-issues every projected token at 80% of its lifetime, as
// the kubelet rotates a projected token: a mint client that re-reads the
// projection before every mint always presents a token the host has not
// recorded yet.
func (cluster *nativeCluster) refreshTokens() {
	ticker := time.NewTicker(nativecluster.TokenRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cluster.stop:
			return
		case <-ticker.C:
			cluster.mu.Lock()
			for unique := range cluster.workloads {
				workload := cluster.workloads[unique]
				_ = cluster.authority.RefreshToken(cluster.projections[unique].TokenFile, &workload)
			}
			cluster.mu.Unlock()
		}
	}
}

// onRunning is the cell's delivery, for one identity workload that just
// started: its module's manifest, listing it.
func (cluster *nativeCluster) onRunning(ctx context.Context, unique string) {
	w := wool.Get(ctx).In("nativeCluster.deliver", wool.Field("service", unique))
	identity, ok := cluster.identity(unique)
	if !ok || !cluster.delivers(identity) {
		return
	}
	cluster.mu.Lock()
	cluster.running[unique] = true
	cluster.mu.Unlock()
	if err := cluster.deliver(ctx, identity.Module); err != nil {
		w.Error("delivery to the host failed", wool.ErrField(err))
	}
}

// newDeliverer is the deliverer of this run's manifests to the host: the
// host's delivery endpoint as the flow resolves it now, this run's signer and
// state, and the delivery account's credential. It also answers the gateway's
// current address, which the envelope follows.
func (cluster *nativeCluster) newDeliverer(ctx context.Context) (*gitops.NativeDeliverer, string, error) {
	gatewayModule, gatewayService, ok := strings.Cut(cluster.env.Host.Gateway, "/")
	if !ok {
		return nil, "", fmt.Errorf("a native run writes the envelope through the host gateway, and host.gateway %q names no <module>/<service>", cluster.env.Host.Gateway)
	}
	gatewayURL, err := cluster.flow.GetAddressForEndpoint(ctx, gatewayModule, gatewayService, "rest")
	if err != nil {
		return nil, "", fmt.Errorf("the host gateway's rest endpoint is unresolved: %w", err)
	}
	deliveryURL, err := cluster.flow.GetAddressForEndpoint(ctx, cluster.hostModule, cluster.hostService, strings.Split(cluster.env.Host.Delivery, "/")[2])
	if err != nil {
		return nil, "", fmt.Errorf("the host's delivery endpoint is unresolved: %w", err)
	}
	return &gitops.NativeDeliverer{
		Workspace: cluster.composed(), Env: cluster.env, Signer: cluster.signer,
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
		ObserverToken: func() (string, error) {
			// The host's existing APPLIED read authenticates this distinct
			// publisher identity; document writer credentials cannot read it.
			pod := nativecluster.Pod{Namespace: "istio-presenter", ServiceAccount: "endpoint-publisher", Name: "endpoint-publisher-native", UID: randomUID()}
			if existing, ok := cluster.authority.Pod(pod.Namespace, pod.ServiceAccount); ok {
				pod = existing
			} else {
				cluster.authority.RegisterPod(&pod)
			}
			return cluster.authority.Token(pod.Namespace, pod.ServiceAccount, pod.Name, pod.UID, cluster.env.Host.Audience)
		},
	}, gatewayURL, nil
}

// withdraw takes the stopped workloads out of their modules' manifests: a
// module that still runs an identity workload is delivered again at the next
// generation, listing what remains; one that runs none is withdrawn — its
// manifest at the next generation with `removed: true`, terminal for the
// binding — and the run waits until the host has applied it. Each stopped
// workload is forgotten. The run's authority, the envelope and the host's
// process are untouched. Returned are the modules withdrawn.
func (cluster *nativeCluster) withdraw(ctx context.Context, uniques []string) ([]string, error) {
	modules := map[string]bool{}
	for _, unique := range uniques {
		if identity, ok := cluster.identity(unique); ok && cluster.delivers(identity) {
			modules[identity.Module] = true
		}
		cluster.forget(unique)
	}
	cluster.mu.Lock()
	for module := range cluster.pendingWithdrawals {
		modules[module] = true
	}
	cluster.mu.Unlock()
	pending := make([]string, 0, len(modules))
	for module := range modules {
		pending = append(pending, module)
	}
	sort.Strings(pending)
	var withdrawn []string
	for _, module := range pending {
		if len(cluster.startedIdentities(module)) > 0 {
			if err := cluster.deliver(ctx, module); err != nil {
				return withdrawn, fmt.Errorf("%s: deliver the workloads that remain: %w", module, err)
			}
			continue
		}
		done, err := cluster.withdrawModule(ctx, module)
		if err != nil {
			return withdrawn, fmt.Errorf("%s: withdraw: %w", module, err)
		}
		if done {
			withdrawn = append(withdrawn, module)
		}
	}
	return withdrawn, nil
}

// withdrawModule delivers a module's withdrawal and waits for the host to
// apply it. A withdrawal is redelivered as it was until then: the same content
// settles to the same generation, which the host replays. A module this run
// never delivered has nothing to withdraw.
func (cluster *nativeCluster) withdrawModule(ctx context.Context, module string) (bool, error) {
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	delivered := cluster.delivered[module]
	if delivered == nil {
		delete(cluster.pendingWithdrawals, module)
		return false, nil
	}
	cluster.pendingWithdrawals[module] = true
	withdrawal, err := gitops.WithdrawalOf(delivered)
	if err != nil {
		return false, err
	}
	deliverer, _, err := cluster.newDeliverer(ctx)
	if err != nil {
		return false, err
	}
	receipt, err := deliverer.Deliver(ctx, withdrawal, cluster.env.ModuleNamespace(cluster.composed(), module))
	if err != nil {
		return false, err
	}
	cluster.delivered[module] = withdrawal
	wool.Get(ctx).In("nativeCluster.withdraw").Info("withdrawal received", wool.Field("module", module),
		wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation))
	if err := deliverer.WaitForWithdrawal(ctx, withdrawal); err != nil {
		return false, err
	}
	delete(cluster.pendingWithdrawals, module)
	return true, nil
}

// forget drops everything this run keeps for one workload: the identity the
// composition gave it, its projection and whether it runs. Nothing of the
// run's authority is touched.
func (cluster *nativeCluster) forget(unique string) {
	cluster.composition.Lock()
	delete(cluster.identities, unique)
	cluster.composition.Unlock()
	cluster.mu.Lock()
	delete(cluster.projections, unique)
	delete(cluster.workloads, unique)
	delete(cluster.running, unique)
	cluster.mu.Unlock()
}

// workloadTokenFileVariable names the projected token's path to a process
// that cannot read it from the path a pod mounts it at.
const workloadTokenFileVariable = "CODEFLY__WORKLOAD_TOKEN_FILE" //nolint:gosec // G101: the name of a path variable, not a credential

// delivers reports whether this run delivers a manifest for an identity's
// module: every module but the host's own.
func (cluster *nativeCluster) delivers(identity gitops.NativeIdentity) bool {
	return identity.Module != cluster.hostModule
}

// startedIdentities are the identity workloads of a module that run now,
// sorted by service.
func (cluster *nativeCluster) startedIdentities(module string) []gitops.NativeIdentity {
	cluster.mu.Lock()
	running := make([]string, 0, len(cluster.running))
	for unique := range cluster.running {
		running = append(running, unique)
	}
	cluster.mu.Unlock()
	var started []gitops.NativeIdentity
	for _, unique := range running {
		if identity, ok := cluster.identity(unique); ok && identity.Module == module {
			started = append(started, identity)
		}
	}
	sort.Slice(started, func(i, j int) bool { return started[i].Service < started[j].Service })
	return started
}

// deliver delivers a module's manifest, listing every identity workload of it
// that runs now, with the addresses this run allocated for its routed
// endpoints, after making sure the host holds the composition's envelope.
func (cluster *nativeCluster) deliver(ctx context.Context, module string) error {
	started := cluster.startedIdentities(module)
	cluster.mu.Lock()
	defer cluster.mu.Unlock()
	if len(started) == 0 {
		return nil
	}
	loaded, err := cluster.composed().LoadModuleFromName(ctx, module)
	if err != nil {
		return err
	}
	services := make([]*resources.Service, 0, len(loaded.ServiceReferences))
	for _, reference := range loaded.ServiceReferences {
		service, loadErr := cluster.composed().LoadService(ctx, &resources.ServiceWithModule{Name: reference.Name, Module: module})
		if loadErr != nil {
			return loadErr
		}
		services = append(services, service)
	}
	deliverer, gatewayURL, err := cluster.newDeliverer(ctx)
	if err != nil {
		return err
	}
	if err = cluster.ensureEnvelope(ctx, gatewayURL); err != nil {
		return err
	}
	bundle, err := cluster.frontendBundle(ctx, loaded)
	if err != nil {
		return err
	}
	carriers, err := cluster.flow.EndpointCarriers(ctx, module, resources.NewNativeNetworkAccess())
	if err != nil {
		return err
	}
	manifest, err := deliverer.Render(ctx, &gitops.NativeWorkload{Module: loaded, Services: services, Started: started, Carriers: carriers, FrontendBundle: bundle})
	if err != nil {
		return err
	}
	receipt, err := deliverer.Deliver(ctx, manifest, cluster.env.ModuleNamespace(cluster.composed(), module))
	if err != nil {
		return err
	}
	cluster.delivered[module] = manifest
	wool.Get(ctx).In("nativeCluster.deliver").Info("delivered", wool.Field("module", module), wool.Field("workloads", len(manifest.Workloads)),
		wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation))
	return nil
}

// ensureEnvelope writes the composition's envelope — each module principal's
// grant, as configurations/<environment>/module-envelope.json states it — as
// the fixture's platform administrator, unless the host holds it already. A
// composition that states no grant writes none.
func (cluster *nativeCluster) ensureEnvelope(ctx context.Context, gatewayURL string) error {
	grants, err := cluster.grants()
	if err != nil || len(grants) == 0 {
		return err
	}
	writer, err := cluster.envelopeFor(ctx, gatewayURL)
	if err != nil {
		return err
	}
	if _, err = writer.Ensure(ctx, func(revision uint64) ([]byte, error) { return gitops.BuildEnvelope(revision, grants) }); err != nil {
		return fmt.Errorf("write the module envelope: %w", err)
	}
	return nil
}

// fixtureAdministrator is the platform administrator the selected fixture
// seeds: the one who writes the envelope, as a cell's administrator would.
// Read from the host module's fixture file, the host's own declaration of it.
type fixtureAdministrator struct {
	Provider string
	Token    string
}

func (cluster *nativeCluster) fixtureAdministrator(ctx context.Context) (fixtureAdministrator, error) {
	module, err := cluster.composed().LoadModuleFromName(ctx, cluster.hostModule)
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

// grants reads the composition's envelope policy: configurations/<env>/module-envelope.json,
// {"principals": {"<module>": <grant>}} — what the platform administrator
// grants each module principal, in the host's own grant shape.
func (cluster *nativeCluster) grants() (map[string]gitops.EnvelopeGrant, error) {
	path := filepath.Join(cluster.composed().Dir(), "configurations", cluster.env.Name, "module-envelope.json")
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
