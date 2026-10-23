package run

import (
	"context"
	"fmt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	"github.com/codefly-dev/cli/pkg/orchestration"
	runfixture "github.com/codefly-dev/cli/pkg/run/fixture"
	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"github.com/codefly-dev/core/workloadidentity"
	nativeidentity "github.com/codefly-dev/core/workloadidentity/native"
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

type launcher struct {
	vouch      *identity.Launcher
	workspace  *resources.Workspace
	env        *environments.Environment
	flow       launcherFlow
	fixture    string
	stateDir   string
	identities map[string]gitops.NativeIdentity
	signer     solutionhost.Signer

	hostModule, hostService string

	// composition guards workspace and identities: the components this run
	// composes, which admit extends with the ones a reloaded workspace adds
	// while the flow keeps reading them. Its own lock, because deliver
	// holds mu while it reads both.
	composition                       sync.RWMutex
	hostIdentityAnchor, hostAnchorDir string

	mu sync.Mutex
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

	// schedule bounds each workload's wait for the host; waiters are the
	// deliveries in flight, one per workload.
	schedule  deliverySchedule
	waitersMu sync.Mutex
	waiters   map[string]*deliveryWaiter
}

var (
	launcherMu     sync.Mutex
	launcherActive *launcher
)

// attachLauncher wires the run as the composition's cluster when its
// environment declares a host and the run is native. It is a no-op otherwise.
func attachLauncher(ctx context.Context, flow *orchestration.Flow, workspace *resources.Workspace, env *environments.Environment, fixture string) error {
	if env == nil || env.Host == nil || runtimeContext == resources.RuntimeContextContainer {
		return nil
	}
	w := wool.Get(ctx).In("launcher")
	launcherMu.Lock()
	defer launcherMu.Unlock()
	if launcherActive != nil {
		launcherActive.close()
		launcherActive = nil
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
	stateDir := filepath.Join(workspace.Dir(), ".codefly", "run", env.Name)
	launcherIdentity, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, stateDir)
	if err != nil {
		return err
	}
	cluster := &launcher{
		vouch: launcherIdentity, workspace: workspace, env: env, flow: flow, fixture: fixture,
		stateDir:           stateDir,
		identities:         identities,
		signer:             signer,
		running:            map[string]bool{},
		delivered:          map[string]*solutionhost.ModuleManifest{},
		pendingWithdrawals: map[string]bool{},
		stop:               make(chan struct{}),
		schedule:           nativeDeliverySchedule,
	}
	parts := strings.Split(env.Host.Delivery, "/")
	if len(parts) != 3 {
		return fmt.Errorf("host.delivery %q is not <module>/<service>/<endpoint>", env.Host.Delivery)
	}
	cluster.hostModule, cluster.hostService = parts[0], parts[1]
	// The run vouches for exactly what it composes: each identity workload,
	// each module's delivery carrier, and the applied observer it acts as.
	cluster.declare(identities)
	if err := cluster.projectHost(); err != nil {
		return err
	}
	world := flow.World()
	world.RenderOwnedGroups = cluster.renderOwnedGroups
	world.RuntimeOverrides = cluster.runtimeOverrides
	flow.WithRunningHook(func(service string, _ int) {
		go cluster.onRunning(context.WithoutCancel(ctx), service)
	})
	go cluster.refreshTokens()
	w.Info("native run: the run vouches for the composition's workloads", wool.Field("anchor", launcherIdentity.AnchorFingerprint()[:16]))
	launcherActive = cluster
	return nil
}

func stopLauncher() {
	launcherMu.Lock()
	defer launcherMu.Unlock()
	if launcherActive != nil {
		launcherActive.close()
		launcherActive = nil
	}
}

func (cluster *launcher) close() {
	select {
	case <-cluster.stop:
	default:
		close(cluster.stop)
	}
}

// declare records identity workloads the run vouches for, with their delivery
// carriers and the observer.
func (cluster *launcher) declare(identities map[string]gitops.NativeIdentity) {
	if cluster.vouch == nil {
		return
	}
	cluster.vouch.Declare(cluster.vouch.Observer())
	for _, identity := range identities {
		cluster.vouch.DeclareModule(identity.Module, identity.Service)
	}
}

// projectHost writes what the host reads to verify this run: the anchor every
// workload credential is checked against, and the delivery trust anchor — a
// verification policy bound to that anchor and admitting the developer key.
func (cluster *launcher) projectHost() error {
	// The native run owns the anchor it binds the policy to and the signer it
	// admits: its own delivery key, which no workspace lists.
	policy, err := localcluster.NativeVerificationPolicy(cluster.vouch.AnchorFingerprint(), cluster.signer, cluster.env.Host)
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
	if err := cluster.vouch.WriteAnchor(cluster.hostIdentityAnchor); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cluster.hostAnchorDir, "verification_policy.json"), policy, 0o600)
}

// composed is the workspace this run composes, as admitted so far.
func (cluster *launcher) composed() *resources.Workspace {
	cluster.composition.RLock()
	defer cluster.composition.RUnlock()
	return cluster.workspace
}

// identity is the identity this run projects for a workload, by unique.
func (cluster *launcher) identity(unique string) (gitops.NativeIdentity, bool) {
	cluster.composition.RLock()
	defer cluster.composition.RUnlock()
	identity, ok := cluster.identities[unique]
	return identity, ok
}

// envelopeFor is the run's envelope writer, pointed at the gateway as it
// answers now: the gateway can be restarted on another port during the run,
// and a writer kept on its first address would fail every envelope write
// after that, before the endpoint projection the delivery ends with.
func (cluster *launcher) envelopeFor(ctx context.Context, gatewayURL string) (*gitops.EnvelopeWriter, error) {
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

func (cluster *launcher) identityOf(service *resources.Service) (string, gitops.NativeIdentity, bool) {
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
func (cluster *launcher) admit(ctx context.Context, workspace *resources.Workspace) ([]string, error) {
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
			return nil, fmt.Errorf("the reloaded composition no longer composes %s, which this run delivers as %s/%s; a run never withdraws a component, stop the run to remove it", unique, known.Module, known.Service)
		case reloaded != known:
			return nil, fmt.Errorf("%s runs as %s/%s and the reloaded composition names it %s/%s; a running principal is never replaced, stop the run to change it", unique, known.Module, known.Service, reloaded.Module, reloaded.Service)
		}
	}
	var added []string
	for unique := range identities {
		if _, known := cluster.identities[unique]; !known {
			added = append(added, unique)
		}
	}
	sort.Strings(added)
	newcomers := map[string]gitops.NativeIdentity{}
	for _, unique := range added {
		cluster.identities[unique] = identities[unique]
		newcomers[unique] = identities[unique]
	}
	cluster.workspace = workspace
	cluster.declare(newcomers)
	if cluster.vouch != nil && cluster.hostIdentityAnchor != "" && len(added) > 0 {
		// The host re-reads the anchor: a newcomer is vouched for from now on.
		if err := cluster.vouch.WriteAnchor(cluster.hostIdentityAnchor); err != nil {
			return nil, err
		}
	}
	return added, nil
}

// runtimeOverrides is the process environment a cell gives a workload and a
// laptop does not: the host finds its API server, a module-identity service
// finds its projected token. Where a workload reaches the host's gateway is
// not here: it is the workspace value the composition points at the gateway's
// `workload` endpoint, which the run resolves like every other
// `${endpoint:…}` reference, at the address this run allocated.
func (cluster *launcher) runtimeOverrides(service *resources.Service) map[string]string {
	unique, identity, ok := cluster.identityOf(service)
	out := map[string]string{}
	if ok {
		// A module-identity service reads its token from the path a cell's
		// render mounts unless told otherwise; this is the otherwise.
		credential, err := cluster.project(unique, identity)
		if err != nil {
			wool.Get(context.Background()).In("launcher").Warn("cannot project the module's credential", wool.Field("service", unique), wool.ErrField(err))
			return out
		}
		out[workloadTokenFileVariable] = credential.File
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
func (cluster *launcher) renderOwnedGroups(ctx context.Context, service *resources.Service) ([]*basev0.Configuration, error) {
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
		{Key: "PRINCIPAL", Value: identity.Workload(cluster.env.Host.TrustDomain).Unique()},
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
func (cluster *launcher) frontendBundle(ctx context.Context, module *resources.Module) (*solutionhost.FrontendPayload, error) {
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
func (cluster *launcher) internalToken(ctx context.Context) (string, error) {
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

func (cluster *launcher) project(unique string, identity gitops.NativeIdentity) (identity.Credential, error) {
	return cluster.vouch.Project(unique, identity.Workload(cluster.env.Host.TrustDomain))
}

// refreshTokens re-issues every projected token at 80% of its lifetime, as
// the kubelet rotates a projected token: a mint client that re-reads the
// projection before every mint always presents a token the host has not
// recorded yet.
func (cluster *launcher) refreshTokens() {
	ticker := time.NewTicker(identity.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cluster.stop:
			return
		case <-ticker.C:
			cluster.vouch.Refresh()
		}
	}
}

// onRunning is the cell's delivery, for one identity workload that just
// started: its module's manifest, listing it — once the host is ready to take
// it, retrying while it is not serving yet (hostDelivery). A workload that is
// not delivered is said so plainly: the gateway routes nothing to its module.
func (cluster *launcher) onRunning(ctx context.Context, unique string) {
	w := wool.Get(ctx).In("launcher.deliver", wool.Field("service", unique))
	identity, ok := cluster.identity(unique)
	if !ok || !cluster.delivers(identity) {
		return
	}
	cluster.mu.Lock()
	cluster.running[unique] = true
	cluster.mu.Unlock()
	waiter := cluster.waitFor(unique)
	defer cluster.doneWaiting(unique, waiter)
	err := hostDelivery{
		schedule: cluster.schedule, stop: cluster.stop, superseded: waiter.superseded,
		log: w.With(wool.Field("module", identity.Module)),
		ready: func(ctx context.Context) error {
			// A refreshed composition can withdraw the workload while its
			// delivery waits; nothing is then delivered on its behalf.
			if _, composed := cluster.identity(unique); !composed {
				return errWorkloadGone
			}
			return cluster.hostReady(ctx)
		},
		deliver: func(ctx context.Context) error { return cluster.deliver(ctx, identity.Module) },
	}.run(ctx)
	if deliveryAbandoned(err) {
		w.Debug("delivery abandoned", wool.ErrField(err))
		return
	}
	if err != nil {
		w.Error(fmt.Sprintf("delivery to the host failed: module %s's manifest does not list %s, so every gateway route to module %s answers 404 until it is delivered (a restart of the workload delivers again)", identity.Module, unique, identity.Module), wool.ErrField(err))
	}
}

// newDeliverer is the deliverer of this run's manifests to the host: the
// host's delivery endpoint as the flow resolves it now, this run's signer and
// state, and the delivery account's credential. It also answers the gateway's
// current address, which the envelope follows.
func (cluster *launcher) newDeliverer(ctx context.Context) (*gitops.NativeDeliverer, string, error) {
	deliveryURL, err := cluster.flow.GetAddressForEndpoint(ctx, cluster.hostModule, cluster.hostService, strings.Split(cluster.env.Host.Delivery, "/")[2])
	if err != nil {
		return nil, "", fmt.Errorf("the host's delivery endpoint is unresolved: %w", err)
	}
	// A host that implements codefly.dev/composition receives manifests through
	// ApplyManifest and holds the manifest as the ceiling: no envelope, no
	// gateway, nothing of the host known here but the interface it declares.
	if cluster.hostImplements(ctx, compositionInterface) {
		conn, err := grpc.NewClient(strings.TrimPrefix(strings.TrimPrefix(deliveryURL, "http://"), "grpc://"), grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, "", fmt.Errorf("dial the host's composition interface: %w", err)
		}
		composition := gitops.NewCompositionDelivery(conn)
		return &gitops.NativeDeliverer{
			Workspace: cluster.composed(), Env: cluster.env, Signer: cluster.signer,
			StateDir: filepath.Join(cluster.stateDir, "delivery"), DeliveryURL: deliveryURL,
			Token: func(module string) (string, error) {
				return cluster.vouch.Issue(cluster.vouch.Workload(module, workloadidentity.DeliveryService))
			},
			ObserverToken: func() (string, error) { return cluster.vouch.Issue(cluster.vouch.Observer()) },
			Post:          composition.Post, Applied: composition.Applied,
		}, "", nil
	}
	gatewayModule, gatewayService, ok := strings.Cut(cluster.env.Host.Gateway, "/")
	if !ok {
		return nil, "", fmt.Errorf("a native run writes the envelope through the host gateway, and host.gateway %q names no <module>/<service>", cluster.env.Host.Gateway)
	}
	gatewayURL, err := cluster.flow.GetAddressForEndpoint(ctx, gatewayModule, gatewayService, "rest")
	if err != nil {
		return nil, "", fmt.Errorf("the host gateway's rest endpoint is unresolved: %w", err)
	}
	return &gitops.NativeDeliverer{
		Workspace: cluster.composed(), Env: cluster.env, Signer: cluster.signer,
		StateDir: filepath.Join(cluster.stateDir, "delivery"), DeliveryURL: deliveryURL,
		Token: func(module string) (string, error) {
			return cluster.vouch.Issue(cluster.vouch.Workload(module, workloadidentity.DeliveryService))
		},
		ObserverToken: func() (string, error) {
			return cluster.vouch.Issue(cluster.vouch.Observer())
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
func (cluster *launcher) withdraw(ctx context.Context, uniques []string) ([]string, error) {
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
func (cluster *launcher) withdrawModule(ctx context.Context, module string) (bool, error) {
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
	receipt, err := deliverer.Deliver(ctx, withdrawal)
	if err != nil {
		return false, err
	}
	cluster.delivered[module] = withdrawal
	wool.Get(ctx).In("launcher.withdraw").Info("withdrawal received", wool.Field("module", module),
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
func (cluster *launcher) forget(unique string) {
	cluster.composition.Lock()
	delete(cluster.identities, unique)
	cluster.composition.Unlock()
	if cluster.vouch != nil {
		cluster.vouch.Forget(unique)
	}
	cluster.mu.Lock()
	delete(cluster.running, unique)
	cluster.mu.Unlock()
}

// workloadTokenFileVariable names the projected token's path to a process
// that cannot read it from the path a pod mounts it at.
const workloadTokenFileVariable = "CODEFLY__WORKLOAD_TOKEN_FILE" //nolint:gosec // G101: the name of a path variable, not a credential

// delivers reports whether this run delivers a manifest for an identity's
// module: every module but the host's own.
func (cluster *launcher) delivers(identity gitops.NativeIdentity) bool {
	return identity.Module != cluster.hostModule
}

// startedIdentities are the identity workloads of a module that run now,
// sorted by service.
func (cluster *launcher) startedIdentities(module string) []gitops.NativeIdentity {
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
func (cluster *launcher) deliver(ctx context.Context, module string) error {
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
	// A host reached through the composition interface holds the manifest as the
	// ceiling and takes no envelope.
	if gatewayURL != "" {
		if err = cluster.ensureEnvelope(ctx, gatewayURL); err != nil {
			return err
		}
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
	receipt, err := deliverer.Deliver(ctx, manifest)
	if err != nil {
		return err
	}
	cluster.delivered[module] = manifest
	wool.Get(ctx).In("launcher.deliver").Info("delivered", wool.Field("module", module), wool.Field("workloads", len(manifest.Workloads)),
		wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation))
	// Accepted into the inbox is not applied. The host says which, and why.
	if err := deliverer.WaitForApplied(ctx, manifest); err != nil {
		return fmt.Errorf("module %s is delivered and not served: %w", module, err)
	}
	wool.Get(ctx).In("launcher.deliver").Info("applied", wool.Field("module", module), wool.Field("generation", manifest.Generation))
	return nil
}

// ensureEnvelope writes the composition's envelope — each module principal's
// grant, as configurations/<environment>/module-envelope.json states it — as
// the fixture's platform administrator, unless the host holds it already. A
// composition that states no grant writes none.
func (cluster *launcher) ensureEnvelope(ctx context.Context, gatewayURL string) error {
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

func (cluster *launcher) fixtureAdministrator(ctx context.Context) (runfixture.Administrator, error) {
	module, err := cluster.composed().LoadModuleFromName(ctx, cluster.hostModule)
	if err != nil {
		return runfixture.Administrator{}, err
	}
	return runfixture.ReadAdministrator(module.Dir(), cluster.fixture)
}

// grants reads the composition's envelope policy: every composed workspace's
// configurations/<env>/module-envelope.json, then the consuming workspace's
// own, {"principals": {"<module>": <grant>}} — what the platform administrator
// grants each module principal, in the host's own grant shape. A platform base
// states its modules' grants and a solution composed on it states its own
// principal's; the consuming workspace decides a principal it states
// (runfixture.ReadCompositionGrants).
func (cluster *launcher) grants() (map[string]gitops.EnvelopeGrant, error) {
	principals, err := runfixture.ReadCompositionGrants(cluster.composed(), cluster.env.Name)
	if err != nil {
		return nil, err
	}
	out := map[string]gitops.EnvelopeGrant{}
	for prefix, grant := range principals {
		out[prefix] = gitops.EnvelopeGrant(grant)
	}
	return out, nil
}

// compositionInterface is the Codefly interface a host declares when it receives
// manifests through ApplyManifest.
const compositionInterface = "codefly.dev/composition"

// hostImplements reports whether the run's host service declares, in its
// module's interface endpoints, that it implements the named Codefly interface at
// any version.
func (cluster *launcher) hostImplements(ctx context.Context, name string) bool {
	module, err := cluster.composed().LoadModuleFromName(ctx, cluster.hostModule)
	if err != nil || module.Interface == nil {
		return false
	}
	for _, endpoint := range module.Interface.Endpoints {
		if endpoint.Service != cluster.hostService {
			continue
		}
		for _, implemented := range endpoint.Implements {
			if implemented == name || strings.HasPrefix(implemented, name+"@") {
				return true
			}
		}
	}
	return false
}
