package run

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"

	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"

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

	hostModule, hostService, hostEndpoint string

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
	// delivery serializes manifest generations and network calls without
	// holding mu. A canceled waiter must not wait for another host call.
	deliveryOnce sync.Once
	delivery     *semaphore.Weighted
	// delivered is the last manifest delivered per module, a withdrawal
	// included; protected by delivery rather than the short-lived state lock.
	delivered map[string]*solutionhost.ModuleManifest
	// outsideTargets are the bindings to a module outside the run that
	// admission has reported, so each is said once; protected by delivery.
	outsideTargets map[string]bool
	// pendingWithdrawals are modules whose withdrawal the host has not applied
	// yet: retried on the next composition refresh, even with no gone roots.
	pendingWithdrawals map[string]bool
	// envelope writes the module envelope through the host's gateway, for a
	// host reached through the legacy HTTP delivery.
	envelope *gitops.EnvelopeWriter
	stop     chan struct{}
	stopOnce sync.Once

	// host is the run's one connection to the host's composition interface,
	// reused by every delivery and withdrawal, and closed with the run.
	host compositionHost

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
	launcherMu.Lock()
	defer launcherMu.Unlock()
	if launcherActive != nil {
		launcherActive.close()
		launcherActive = nil
	}
	cluster, err := newNativeLauncher(ctx, flow, workspace, env, runtimeContext, fixture)
	if err != nil {
		return err
	}
	launcherActive = cluster
	return nil
}

// AttachNativeLauncher gives test entry points the same native authority
// admission as run, with explicit configuration and flow-owned teardown.
// Call after InitManagers determines whether this test starts any service.
func AttachNativeLauncher(ctx context.Context, flow *orchestration.Flow, workspace *resources.Workspace, env *environments.Environment, selectedRuntime, fixture string) error {
	if len(flow.LocalRuntimeServices()) == 0 {
		return nil
	}
	_, err := newNativeLauncher(ctx, flow, workspace, env, selectedRuntime, fixture)
	return err
}

func newNativeLauncher(ctx context.Context, flow *orchestration.Flow, workspace *resources.Workspace, env *environments.Environment, selectedRuntime, fixture string) (*launcher, error) {
	if env == nil || env.Host == nil || selectedRuntime == resources.RuntimeContextContainer {
		return nil, nil
	}
	w := wool.Get(ctx).In("launcher")
	if err := env.Host.Validate(); err != nil {
		return nil, err
	}
	identities, err := gitops.ResolveNativeIdentities(ctx, workspace, env)
	if err != nil {
		return nil, err
	}
	signer, err := signing.LoadDeveloperKey()
	if err != nil {
		return nil, fmt.Errorf("a native run delivers with the developer key, and none is held: %w", err)
	}
	stateDir := filepath.Join(workspace.Dir(), ".codefly", "run", env.Name)
	launcherIdentity, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, stateDir)
	if err != nil {
		return nil, err
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
	cluster.hostModule, cluster.hostService, cluster.hostEndpoint = env.Host.DeliveryEndpoint()
	// The run vouches for exactly what it composes: each identity workload,
	// each module's delivery carrier, and the applied observer it acts as.
	cluster.declare(identities)
	if err := cluster.projectHost(); err != nil {
		return nil, err
	}
	world := flow.World()
	world.RenderOwnedGroups = cluster.renderOwnedGroups
	world.RuntimeOverrides = cluster.runtimeOverrides
	world.BeginRuntimeStart = cluster.beginRuntimeStart
	go cluster.refreshTokens(ctx)
	w.Info("native run: the run vouches for the composition's workloads", wool.Field("anchor", launcherIdentity.AnchorFingerprint()[:16]))
	world.BeforeRuntimeStop = cluster.close
	return cluster, nil
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
	cluster.stopOnce.Do(func() {
		if cluster.stop != nil {
			close(cluster.stop)
		}
	})
	cluster.host.close()
	// A run torn down keeps no staged frontend copy: the next run stages and
	// installs again, and reuses every bundle still retained.
	if cluster.stateDir != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := (solutionrun.LocalFrontend{StateDir: cluster.frontendState()}).ForgetAll(ctx); err != nil {
			wool.Get(ctx).In("launcher.close").Warn("cannot remove the staged frontend copies", wool.ErrField(err))
		}
	}
}

// frontendState is where this run keeps its frontend build records and staged
// copies, per environment.
func (cluster *launcher) frontendState() string {
	return filepath.Join(cluster.stateDir, "frontend")
}

// forgetFrontend removes a module's staged frontend copy.
func (cluster *launcher) forgetFrontend(ctx context.Context, module string) error {
	if cluster.stateDir == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return solutionrun.LocalFrontend{StateDir: cluster.frontendState()}.Forget(ctx, module)
}

// declare records identity workloads the run vouches for, with their delivery
// carriers and the observer the legacy HTTP delivery reads applied state as.
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
		out[workloadTokenFileVariable] = cluster.vouch.ProjectionPath(identity.Workload(cluster.env.Host.TrustDomain))
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
	group, err := environments.NativeHostFederation(cluster.env.Host, service)
	if err != nil {
		return nil, err
	}
	if group != nil {
		out = append(out, group)
	}
	if slices.Contains(service.WorkspaceConfigurationDependencies, environments.ArtifactCacheConfiguration) {
		cacheGroup, cacheErr := artifactCacheGroup(cluster.composed().Dir())
		if cacheErr != nil {
			return nil, cacheErr
		}
		out = append(out, cacheGroup)
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

// frontendBundle is the bundle of a module's declared page, built from the
// frontend's own inputs only when they changed since the bundle retained for
// them, and retained in the existing artifact cache for host delivery. It runs
// before the delivery lock is taken, so a build delays no other module's
// delivery. The frontend's own npm build runs natively unless the service that
// owns the page runs in a container, where it builds in the pinned builder
// image as deployment does. Headless components need no build.
func (cluster *launcher) frontendBundle(ctx context.Context, module *resources.Module) (*solutionhost.FrontendPayload, error) {
	frontend, err := solutionrun.DeclaredFrontend(ctx, module)
	if err != nil || frontend == nil {
		return nil, err
	}
	workspaceDir := cluster.composed().Dir()
	build := solutionrun.LocalFrontend{
		StateDir: cluster.frontendState(),
		Docker:   cluster.flow.ServiceRuntimeContext(resources.ServiceUnique(module.Name, module.ServiceEntry)) == resources.RuntimeContextContainer,
		Retain: func(ctx context.Context, root string) (string, error) {
			digest, _, retainErr := composition.RetainCanonicalArchive(ctx, workspaceDir, root)
			return digest, retainErr
		},
		Retained: func(digest string) bool {
			info, statErr := os.Stat(filepath.Join(composition.ArtifactCacheDir(workspaceDir), strings.TrimPrefix(digest, "sha256:")))
			return statErr == nil && info.Mode().IsRegular()
		},
	}
	digest, err := build.Bundle(ctx, module)
	if err != nil {
		return nil, err
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

// refreshTokens checks wall-clock expiry on a short poll, so waking from
// suspend does not leave expired credentials waiting for a lifetime timer.
func (cluster *launcher) refreshTokens(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cluster.stop:
			return
		case <-ticker.C:
			if err := cluster.vouch.Refresh(time.Now()); err != nil {
				wool.Get(ctx).In("launcher.refreshTokens").Error("cannot refresh workload credentials; retrying on next poll", wool.ErrField(err))
			}
		}
	}
}

// runReadyDelivery publishes the latest readiness after an attempt. Failed
// restarts remove stale routes through the same nonterminal signed manifest.
func (cluster *launcher) runReadyDelivery(ctx context.Context, unique string, identity gitops.NativeIdentity, waiter *deliveryWaiter, failed bool) {
	defer cluster.doneWaiting(unique, waiter)
	w := wool.Get(ctx).In("launcher.deliver", wool.Field("service", unique))
	err := hostDelivery{
		schedule: cluster.schedule, stop: cluster.stop, superseded: waiter.superseded,
		log: w.With(wool.Field("module", identity.Module)),
		ready: func(ctx context.Context) error {
			if _, composed := cluster.identity(unique); !composed {
				return errWorkloadGone
			}
			return cluster.hostReady(ctx)
		},
		deliver: func(ctx context.Context) error { return cluster.deliverReadiness(ctx, identity.Module, failed) },
	}.run(ctx)
	if deliveryAbandoned(err) {
		w.Debug("delivery abandoned", wool.ErrField(err))
	} else if err != nil {
		w.Error(fmt.Sprintf("delivery completion was not confirmed for module %s, workload %s; routes to module %s may be unavailable (the host may already have applied the manifest)", identity.Module, unique, identity.Module), wool.ErrField(err))
	}
}

// newDeliverer is the deliverer of this run's manifests to the host, with
// this run's signer and state and the delivery account's credential. A host
// whose delivery endpoint declares codefly.dev/composition receives them
// through it, over the run's one connection to it; the gateway address is then
// empty, since such a host takes no envelope. A host whose endpoint declares
// no such interface receives them through the legacy HTTP delivery route, with
// the envelope written through its gateway, whose address is answered: the
// reason is logged on every delivery that takes it. A host whose module cannot
// be read is neither: the delivery fails with the reason.
func (cluster *launcher) newDeliverer(ctx context.Context) (*gitops.NativeDeliverer, string, error) {
	implements, err := gitops.HostImplements(ctx, cluster.composed(), cluster.hostModule, cluster.hostService, cluster.hostEndpoint, gitops.CompositionInterface)
	if err != nil {
		return nil, "", err
	}
	deliverer := &gitops.NativeDeliverer{
		Workspace: cluster.composed(), Env: cluster.env, Signer: cluster.signer,
		StateDir: filepath.Join(cluster.stateDir, "delivery"),
		Token: func(module string) (string, error) {
			return cluster.vouch.Issue(cluster.vouch.Workload(module, workloadidentity.DeliveryService))
		},
	}
	if implements {
		target, resolveErr := cluster.flow.GRPCEndpointTarget(ctx, cluster.hostModule, cluster.hostService, cluster.hostEndpoint)
		if resolveErr != nil {
			return nil, "", fmt.Errorf("the host's delivery endpoint is unresolved: %w", resolveErr)
		}
		composition, dialErr := cluster.host.delivery(target, cluster.newCompositionDelivery)
		if dialErr != nil {
			return nil, "", dialErr
		}
		deliverer.Prepare, deliverer.Applied = composition.Prepare, composition.Applied
		return deliverer, "", nil
	}
	wool.Get(ctx).In("launcher.deliver").Info(fmt.Sprintf("the host's delivery endpoint %s/%s/%s declares no %s, so manifests go to its legacy HTTP delivery route and the module envelope is written through host.gateway",
		cluster.hostModule, cluster.hostService, cluster.hostEndpoint, gitops.CompositionInterface))
	if deliverer.DeliveryURL, err = cluster.flow.GetAddressForEndpoint(ctx, cluster.hostModule, cluster.hostService, cluster.hostEndpoint); err != nil {
		return nil, "", fmt.Errorf("the host's delivery endpoint is unresolved: %w", err)
	}
	gatewayModule, gatewayService, ok := strings.Cut(cluster.env.Host.Gateway, "/")
	if !ok {
		return nil, "", fmt.Errorf("the legacy HTTP delivery writes the envelope through the host gateway, and host.gateway %q names no <module>/<service>", cluster.env.Host.Gateway)
	}
	gatewayURL, err := cluster.flow.GetAddressForEndpoint(ctx, gatewayModule, gatewayService, "rest")
	if err != nil {
		return nil, "", fmt.Errorf("the host gateway's rest endpoint is unresolved: %w", err)
	}
	deliverer.ObserverToken = func() (string, error) { return cluster.vouch.Issue(cluster.vouch.Observer()) }
	return deliverer, gatewayURL, nil
}

// newCompositionDelivery speaks the composition interface over conn, reading
// each module's method policies, scope ceilings and roles from the module as
// the run composes it at the time of the delivery.
func (cluster *launcher) newCompositionDelivery(conn grpc.ClientConnInterface) *gitops.CompositionDelivery {
	load := func(ctx context.Context, module string) (string, error) {
		m, err := cluster.composed().LoadModuleFromName(ctx, module)
		if err != nil {
			return "", err
		}
		return m.Dir(), nil
	}
	return gitops.NewCompositionDelivery(conn, cluster.signer,
		func(ctx context.Context, module string) ([]*compositionv1.MethodPolicyV1, bool, error) {
			dir, err := load(ctx, module)
			if err != nil {
				return nil, false, err
			}
			return gitops.MethodPolicies(dir)
		},
		func(ctx context.Context, module string) ([]*authorityv1.WorkScopeV1, error) {
			dir, err := load(ctx, module)
			if err != nil {
				return nil, err
			}
			return gitops.ScopeCeilings(dir)
		},
		func(ctx context.Context, module string) ([]*compositionv1.RoleV1, error) {
			dir, err := load(ctx, module)
			if err != nil {
				return nil, err
			}
			return gitops.Roles(dir)
		})
}

// compositionHost holds the run's connection to the host's composition
// interface: dialled on the first delivery, reused by every later one and
// every retry, dialled again only when the host answers at another address (a
// host restarted on another port), and closed with the run.
type compositionHost struct {
	mu          sync.Mutex
	target      orchestration.GRPCTarget
	conn        *grpc.ClientConn
	composition *gitops.CompositionDelivery
	closed      bool
}

// delivery answers the composition client for target, dialling it unless the
// connection held already reaches it.
func (host *compositionHost) delivery(target orchestration.GRPCTarget, speak func(grpc.ClientConnInterface) *gitops.CompositionDelivery) (*gitops.CompositionDelivery, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	if host.closed {
		return nil, errRunStopped
	}
	if host.conn != nil && host.target == target {
		return host.composition, nil
	}
	conn, err := target.Dial()
	if err != nil {
		return nil, fmt.Errorf("dial the host's composition interface at %s: %w", target.Address, err)
	}
	if host.conn != nil {
		_ = host.conn.Close()
	}
	host.target, host.conn, host.composition = target, conn, speak(conn)
	return host.composition, nil
}

func (host *compositionHost) close() {
	host.mu.Lock()
	defer host.mu.Unlock()
	host.closed = true
	if host.conn != nil {
		_ = host.conn.Close()
		host.conn, host.composition = nil, nil
	}
}

// withdraw takes the stopped workloads out of their modules' manifests: a
// module that still runs an identity workload is delivered again at the next
// generation, listing what remains; one that runs none is withdrawn — its
// manifest at the next generation with `removed: true`, which the host keeps
// terminal for the binding while it holds it — and the run waits until the
// host has applied it. Each stopped workload is forgotten. The run's authority
// and the host's process are untouched. Returned are the modules withdrawn.
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
		if len(cluster.declaredIdentities(module)) > 0 {
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
			// A module that left the composition keeps no staged frontend.
			if err := cluster.forgetFrontend(ctx, module); err != nil {
				wool.Get(ctx).In("launcher.withdraw").Warn("cannot remove the staged frontend of a withdrawn module", wool.Field("module", module), wool.ErrField(err))
			}
		}
	}
	return withdrawn, nil
}

// withdrawModule delivers a module's withdrawal and waits for the host to
// apply it. A withdrawal is redelivered as it was until then: the same content
// settles to the same generation, which the host replays. A module this run
// never delivered has nothing to withdraw.
func (cluster *launcher) withdrawModule(ctx context.Context, module string) (bool, error) {
	if err := cluster.acquireDelivery(ctx); err != nil {
		return false, err
	}
	defer cluster.delivery.Release(1)
	delivered := cluster.delivered[module]
	cluster.mu.Lock()
	if delivered == nil {
		delete(cluster.pendingWithdrawals, module)
		cluster.mu.Unlock()
		return false, nil
	}
	cluster.pendingWithdrawals[module] = true
	cluster.mu.Unlock()
	withdrawal, err := gitops.WithdrawalOf(delivered)
	if err != nil {
		return false, err
	}
	deliverer, _, err := cluster.newDeliverer(ctx)
	if err != nil {
		return false, err
	}
	receipt, err := deliverer.Deliver(ctx, withdrawal)
	switch {
	case errors.Is(err, solutionhost.ErrWithdrawn):
		// The host already holds a withdrawal of this binding (a module
		// re-added and refused, then dropped again): nothing is served under it.
		cluster.delivered[module] = withdrawal
	case err != nil:
		return false, err
	default:
		cluster.delivered[module] = withdrawal
		wool.Get(ctx).In("launcher.withdraw").Info("withdrawal received", wool.Field("module", module),
			wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation))
		if err := deliverer.WaitForWithdrawal(ctx, withdrawal); err != nil {
			return false, err
		}
	}
	cluster.mu.Lock()
	delete(cluster.pendingWithdrawals, module)
	cluster.mu.Unlock()
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
// declared in the run, with the addresses this run allocated for the routed
// endpoints of those that are ready.
func (cluster *launcher) deliver(ctx context.Context, module string) error {
	return cluster.deliverReadiness(ctx, module, false)
}

// deliverReadiness delivers a module's readiness. A ready delivery builds its
// frontend bundle first, before the delivery lock is taken, so a build delays
// no other module's delivery; an early one (after a failed start) builds
// nothing.
func (cluster *launcher) deliverReadiness(ctx context.Context, module string, early bool) error {
	if len(cluster.declaredIdentities(module)) == 0 {
		return nil
	}
	loaded, err := cluster.composed().LoadModuleFromName(ctx, module)
	if err != nil {
		return err
	}
	var bundle *solutionhost.FrontendPayload
	if !early {
		if bundle, err = cluster.frontendBundle(ctx, loaded); err != nil {
			return err
		}
	}
	if err = cluster.acquireDelivery(ctx); err != nil {
		return err
	}
	defer cluster.delivery.Release(1)
	// Failure may precede admission. Replace a previous host snapshot, but do
	// not admit a new module just to clean up an unstarted attempt.
	if early && cluster.delivered[module] == nil {
		return nil
	}
	if len(cluster.declaredIdentities(module)) == 0 {
		return nil
	}
	if err = cluster.checkRoutingSnapshot(ctx); err != nil {
		return err
	}
	deliverer, gatewayURL, err := cluster.newDeliverer(ctx)
	if err != nil {
		return err
	}
	manifest, err := cluster.renderDelivery(ctx, deliverer, loaded, early, bundle)
	if err != nil {
		return err
	}
	return cluster.applyDelivery(ctx, deliverer, gatewayURL, module, manifest)
}

// renderDelivery renders a loaded module's manifest. An early delivery keeps
// the page payload previously published, since deferral removes a page; a
// ready one names the bundle its caller built.
func (cluster *launcher) renderDelivery(ctx context.Context, deliverer *gitops.NativeDeliverer, loaded *resources.Module, early bool, bundle *solutionhost.FrontendPayload) (*solutionhost.ModuleManifest, error) {
	module := loaded.Name
	services := make([]*resources.Service, 0, len(loaded.ServiceReferences))
	for _, reference := range loaded.ServiceReferences {
		service, loadErr := cluster.composed().LoadService(ctx, &resources.ServiceWithModule{Name: reference.Name, Module: module})
		if loadErr != nil {
			return nil, loadErr
		}
		services = append(services, service)
	}
	if early {
		// Deferral removes a page. Keep a previously published payload during
		// unrelated partial startup instead of accidentally unpublishing it.
		if previous := cluster.delivered[module]; previous != nil && previous.Frontend != nil {
			bundle = previous.Frontend.Payload
		}
	}
	started := cluster.startedIdentities(module)
	var ready []string
	for _, identity := range started {
		ready = append(ready, resources.ServiceUnique(identity.Module, identity.Service))
	}
	carriers, err := cluster.flow.ReadyEndpointCarriers(ctx, module, resources.NewNativeNetworkAccess(), ready)
	if err != nil {
		return nil, err
	}
	return deliverer.Render(ctx, &gitops.NativeWorkload{
		Module: loaded, Services: services, Declared: cluster.declaredIdentities(module),
		Started: started, Carriers: carriers, FrontendBundle: bundle, DeferFrontend: early && bundle == nil,
	})
}

// Caller holds delivery across rendering, generation reservation and Applied.
func (cluster *launcher) applyDelivery(ctx context.Context, deliverer *gitops.NativeDeliverer, gatewayURL, module string, manifest *solutionhost.ModuleManifest) error {
	if gatewayURL != "" {
		if err := cluster.ensureEnvelope(ctx, gatewayURL); err != nil {
			return err
		}
	}
	receipt, err := deliverer.Deliver(ctx, manifest)
	if err != nil {
		return err
	}
	cluster.delivered[module] = manifest
	w := wool.Get(ctx).In("launcher.deliver")
	w.Info("delivered", wool.Field("module", manifest.Alias), wool.Field("workloads", len(manifest.Workloads)),
		wool.Field("disposition", receipt.Disposition), wool.Field("generation", receipt.Generation))
	if err := deliverer.WaitForApplied(ctx, manifest); err != nil {
		return fmt.Errorf("module %s is delivered and not served: %w", manifest.Alias, err)
	}
	w.Info("applied", wool.Field("module", manifest.Alias), wool.Field("generation", manifest.Generation))
	return nil
}

// checkRoutingSnapshot refuses a delivery whose policy tables a hop's frozen
// routes no longer describe: the tables are read live, the routes were fixed
// when the run started, and a contract published since would deliver new
// policies behind old routes. Called under the delivery lock, before reading.
func (cluster *launcher) checkRoutingSnapshot(ctx context.Context) error {
	if err := cluster.flow.CheckRoutingSnapshot(ctx, cluster.composed()); err != nil {
		return fmt.Errorf("deliver no policy table behind the hop's routes: %w", err)
	}
	return nil
}

func (cluster *launcher) acquireDelivery(ctx context.Context) error {
	cluster.deliveryOnce.Do(func() { cluster.delivery = semaphore.NewWeighted(1) })
	return cluster.delivery.Acquire(ctx, 1)
}

// ensureEnvelope writes the composition's envelope — each module principal's
// grant, as configurations/<environment>/module-envelope.json states it — as
// the fixture's platform administrator, unless the host holds it already. A
// composition that states no grant writes none.
func (cluster *launcher) ensureEnvelope(ctx context.Context, gatewayURL string) error {
	grants, err := cluster.grants(ctx)
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
// (runfixture.ReadCompositionGrants), with an entry for each prepared binding
// a module installs (gitops.WithPreparedBindingGrants).
func (cluster *launcher) grants(ctx context.Context) (map[string]gitops.EnvelopeGrant, error) {
	envelopeProfile, err := cluster.env.ConfigurationProfileName()
	if err != nil {
		return nil, err
	}
	principals, err := runfixture.ReadCompositionGrants(cluster.composed(), envelopeProfile)
	if err != nil {
		return nil, err
	}
	if principals, err = gitops.WithPreparedBindingGrants(ctx, cluster.composed(), cluster.env, principals); err != nil {
		return nil, err
	}
	out := map[string]gitops.EnvelopeGrant{}
	for prefix, grant := range principals {
		out[prefix] = gitops.EnvelopeGrant(grant)
	}
	return out, nil
}
