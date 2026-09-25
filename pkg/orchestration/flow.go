package orchestration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/dockerstart"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/internal/selectionguard"
	"github.com/codefly-dev/cli/pkg/remotenetwork"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
)

type Flow struct {
	// configurationReferences orders a consumer after the producers its
	// workspace configurations reference (configurationReferenceOption); every
	// rebuild of the graph carries it.
	configurationReferences architecture.DependencyOption

	workspace *resources.Workspace

	// graphWorkspace is the workspace the dependency graph is built from: the
	// run's module closure when the caller supplied seeds, otherwise the whole
	// pin set. Read it through dependencyWorkspace(), never directly.
	graphWorkspace *resources.Workspace

	// Where we start
	originService *resources.Service
	originModule  *resources.Module

	// coRoots are the module-qualified uniques of services started as roots
	// alongside the origin, sharing its graph. A product composition selects
	// several solutions, each a root that requires the same host services;
	// running one flow per solution would give each its own graph, and those
	// collide on ports and on container-recovery scope. The origin stays the
	// service the run is named and reported by.
	coRoots []string

	// The world
	world *World

	// What we do
	playbook *Playbook
	policy   PlaybookPolicy

	// How we keep track of state
	SharedState *StateManager

	// How we keep track of resources
	ConfigurationManager *configurations.Manager

	hub *Hub

	failureMu   sync.Mutex
	failure     *FlowFailure
	startCancel context.CancelFunc

	endpoints       map[string][]*basev0.Endpoint
	networkMappings map[string][]*basev0.NetworkMapping

	loadOnly bool
	initOnly bool

	standAlone  bool
	excludeRoot bool

	scope string

	// containerRecoveryScope is this flow's projected container ownership and
	// containerRecoveryIdentity the acknowledgement its agents echo back, both
	// resolved once by ContainerRecoveryScope().
	containerRecoveryScope    dockerrun.ContainerRecoveryScope
	containerRecoveryIdentity string

	runtimeContext string
	// docker records whether the Docker engine is reachable and, for messaging,
	// the docker context/endpoint the run command resolved. Gates the free→nix
	// fallback per service (see resolveDockerFallback). dockerProbed is set only
	// when the run command supplied a status; other entry points (test/build/
	// deploy/sync) leave the fallback disabled and keep their prior behavior.
	docker       DockerStatus
	dockerProbed bool
	// startDocker, when true, lets resolveDockerFallback auto-start a local
	// Docker engine (OrbStack/Docker Desktop/colima/…) if a service can only run
	// under Docker and the daemon is not reachable. Set from the --start-docker
	// flag (default true) by the run command; other entry points leave it false.
	startDocker bool
	// preferences are this developer's machine-local choices (~/.codefly/
	// preferences.yaml), e.g. run Go services native but postgres nix. They
	// override runtimeContext PER SERVICE; runtimeContext is the fallback.
	// Loaded best-effort in NewFlow (a missing/!malformed file = empty prefs).
	preferences *resources.UserPreferences
	fixture     string

	// overrides are per-service runtime env-var overrides: KEY -> VAL keyed by
	// either the module-qualified unique ("<module>/<service>") or the bare
	// service name. `codefly run ... --set <service>:KEY=VAL` supplies the bare
	// form; callers that must target exactly one service use the unique, since
	// two composed modules may each hold a service of the same name.
	overrides map[string]map[string]string

	// testRequest carries CLI-provided test filtering/suite/extra-args
	// to the origin runner when the flow is in TestMode. Dependency
	// runners ignore it — they only need to be Started, not tested.
	testRequest *runtimev0.TestRequest
	// testDependencyMode is resolved from the origin agent's authoritative
	// suite advertisement before dependency managers are selected. Legacy
	// agents retain the dependency-free behavior they had before the contract.
	testDependencyMode agentv0.TestDependencyMode
	temporaryPorts     bool
	portOverrides      map[string]uint16

	// syncRequest carries CI dry-run intent to every builder participating in a
	// SyncMode flow. Nil retains the conventional mutating sync behavior.
	syncRequest *builderv0.SyncRequest

	// Output running configurations
	outputEnvPath    string
	outputEnvService string

	// actual services running
	services []*resources.Service
	// except when we do remote
	remoteServices []*Remote

	excludedDependencyServices []string

	// stateListener, when set, is invoked on every runtime lifecycle transition
	// of every managed service (not just the origin) so a UI can render live
	// per-service status.
	stateListener StateListener

	// beganMu guards states. states records the last runtime lifecycle state
	// THIS run has emitted (via emitState) per service. ServiceReachable
	// consults it so a readiness probe can only ever confirm a service we
	// actually launched — never a zombie from a previous run squatting on the
	// same deterministically hashed port — and readiness consults it for the
	// completed Start a dependency must have reached. Written from the
	// orchestration goroutine, read from the UI poller, hence the lock.
	beganMu sync.Mutex
	states  map[string]tui.ServiceState

	// teardownPhaseBudget bounds one reverse-topological teardown layer; zero
	// selects defaultTeardownPhaseBudget. Since layers are sequential, a whole
	// Stop/Shutdown is bounded by this budget times the number of layers.
	teardownPhaseBudget time.Duration
	teardownMu          sync.Mutex
	lastTeardown        *teardownReceipt
}

// StateListener observes per-service runtime lifecycle transitions. service is
// the Unique ("saas/vault"); port is the service's primary port once known
// (0 otherwise). Wired from the TUI in `codefly run` so dependencies show
// their own progress instead of hiding behind the origin's spinner.
type StateListener func(service string, state tui.ServiceState, port int)

func MapValues[K comparable, V any](m map[K]V) []V {
	var values []V
	for _, v := range m {
		values = append(values, v)
	}
	return values
}

type World struct {
	Env  *environments.Environment
	Mode Mode

	// containerRecoveryIdentity is the ownership acknowledgement every agent
	// spawned for this flow must return before it can create Docker resources.
	containerRecoveryIdentity string

	// Push drives whether a docker build pushes its image to the registry.
	// Scoped to the flow (not process-global) so a snapshot render that
	// requires push cannot silently make a later in-process BuildMode build
	// push without being asked.
	Push bool

	// BuildxBuilder names a caller-provided docker buildx builder to run the
	// build on (e.g. a native amd64 buildkit reached over the network), letting
	// an amd64 target build without local QEMU emulation. Empty selects the
	// default builder, or the dedicated local container builder for multi-arch.
	BuildxBuilder string
	BuildCache    *builderv0.BuildCacheOptions

	// CaptureImageDigest asks a pushed build to resolve the immutable manifest
	// digest of the image it published so a caller can report or pin it. It is
	// opt-in per flow: a plain `build module` never consumes a digest, so it does
	// not opt in and never pays for (or fails on) digest resolution. A snapshot
	// resolves the digest unconditionally because its manifest pins it.
	CaptureImageDigest bool

	// CollectImageSBOM requires digest-bound image SBOM evidence for every image
	// a build produces. It is opt-in per flow: collecting evidence runs a
	// container scanner and fails the build when coverage is incomplete, so a
	// caller that does not consume the evidence neither pays for it nor fails on
	// an agent that cannot yet serve it.
	CollectImageSBOM bool

	Workspace               *resources.Workspace
	DeploymentDestination   func(*resources.Module, *resources.Service) string
	KubernetesOutputProfile builderv0.KubernetesOutputProfile

	// ValidateCluster opts a promotable render into a server-side dry-run of
	// its manifests against the environment's declared cluster. Off by default:
	// a render needs no cluster. See resolveClusterValidation.
	ValidateCluster bool
	// NamespaceProbe is the cluster read ValidateCluster performs before asking
	// for the dry-run; nil selects the kubectl-backed default.
	NamespaceProbe NamespaceProbe

	// DAG
	Dependencies *architecture.ServiceDependencies

	// Keep track of things
	SharedState *StateManager

	LocalNetworkManager  *network.RuntimeManager
	RemoteNetworkManager *remotenetwork.RemoteManager

	ConfigurationManager *configurations.Manager

	RemoteManager deployments.Manager

	SyncRequest *builderv0.SyncRequest

	excludedWorkspaceConfigurations map[string]bool

	// runtimeContextFor is the flow's choice of runtime context per service, so
	// the world can derive a producer's proposed mappings before it initializes
	// (referencedProducerMappings).
	runtimeContextFor func(*resources.Service) string

	// workspaceConfigurationValues are values the run path derives itself,
	// keyed group -> key -> value. They are layered onto the resolved workspace
	// configurations of every service declaring that group, so a derived value
	// reaches a service through the same carrier a declared one does rather
	// than through a raw process variable the service has no contract to read.
	workspaceConfigurationValues map[string]map[string]string

	// OutputSink receives narration otherwise printed directly via pkg/cli.
	// Always non-nil: NewFlow defaults it to a no-op sink.
	OutputSink OutputSink

	// AnswerProvider answers interactive questions a builder plugin asks
	// during Sync. Always non-nil: NewFlow defaults it to a headless
	// (defaults-only) provider.
	AnswerProvider AnswerProvider
}

// configurationReferenceOption reads what env provides to the workspace — the
// same read a run provisions from — and orders every service declaring a
// workspace configuration after the services its ${endpoint:…} references name.
// A read that fails orders nothing: the run reports the configuration fault
// itself when it loads.
func configurationReferenceOption(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) architecture.DependencyOption {
	if workspace == nil || env == nil {
		return nil
	}
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime())
	if err != nil {
		wool.Get(ctx).In("configurationReferenceOption").Debug("cannot read workspace configurations; no reference orders the run", wool.Field("error", err.Error()))
		return nil
	}
	producers := configurations.EndpointProducers(provided.Infos)
	if len(producers) == 0 {
		return nil
	}
	return architecture.WithConfigurationReferences(producers)
}

// FlowFailure carries a runner-level death up to the top-level command.
// A failure here means a service started OK but its underlying process
// has since exited (e.g. user binary crashed, agent plugin lost contact).
// It does NOT cover failures during Load/Init/Start — those bubble up
// through the normal flow.Start() return error.
type FlowFailure struct {
	Service string
	Message string
}

func (f FlowFailure) Error() string {
	return fmt.Sprintf("%s: %s", f.Service, f.Message)
}

func NewFlow(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *environments.Environment, mode Mode, opts ...FlowOption) (*Flow, error) {
	w := wool.Get(ctx).In("NewFlow")

	options := &flowOptions{}
	for _, opt := range opts {
		opt(options)
	}
	graphWorkspace, err := runModuleClosure(ctx, workspace, options.moduleClosureSeeds)
	if err != nil {
		return nil, w.Wrap(err)
	}

	// Get dependency graph. A service reaching a producer only through a
	// workspace configuration group the composition root writes is ordered
	// after it, as for a declared dependency.
	configurationReferences := configurationReferenceOption(ctx, workspace, env)
	var graphOptions []architecture.DependencyOption
	if configurationReferences != nil {
		graphOptions = append(graphOptions, configurationReferences)
	}
	dependencies, err := architecture.NewServiceDependencies(ctx, graphWorkspace, graphOptions...)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world := &World{
		Env:          env,
		Mode:         mode,
		Workspace:    workspace,
		Dependencies: dependencies,
		OutputSink:   noopOutputSink{},
	}
	world.AnswerProvider = headlessAnswerProvider{world: world}

	configurationManager, err := configurations.NewManager(ctx, workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}

	localReader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	if err != nil {
		return nil, w.Wrap(err)
	}
	configurationManager.WithLoader(localReader)

	// A GitOps snapshot render is value-free: it emits secret references derived
	// from the committed declarations and discards every secret value. Resolving
	// references against their real provider would demand provider auth or local
	// plaintext value files for values that never reach a manifest, so a snapshot
	// resolves them to an inert placeholder instead (see snapshotSecretResolver).
	//
	// The placeholder is only ever safe because the promotable profile strips
	// secret values back into secretKeyRefs before a builder sees them; any other
	// profile would ship the placeholder as a real value. Bind that profile here,
	// at the same point the resolver is registered, so the two cannot drift: a
	// snapshot is by definition the promotable GitOps snapshot.
	if mode == SnapshotMode {
		configurationManager.WithSecretResolver(snapshotSecretResolver{})
		//nolint:staticcheck // SA1019: builder_deploy and the whole render path still gate on PROMOTABLE_GITOPS_V1; this must match them, migrate together.
		world.KubernetesOutputProfile = builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1
	}

	stateManager, err := NewStateManager(ctx, configurationManager, world.Dependencies)
	if err != nil {
		return nil, w.Wrap(err)
	}

	world.SharedState = stateManager
	world.ConfigurationManager = configurationManager

	world.LocalNetworkManager, err = network.NewRuntimeManager(ctx, configurationManager)
	if err != nil {
		return nil, w.Wrap(err)
	}
	world.RemoteNetworkManager, err = remotenetwork.NewRemoteManager(ctx, configurationManager)
	if err != nil {
		return nil, w.Wrap(err)
	}

	flow := &Flow{
		workspace:      workspace,
		graphWorkspace: graphWorkspace,
		originService:  service,
		originModule:   module,

		world: world,

		configurationReferences: configurationReferences,

		SharedState:          stateManager,
		ConfigurationManager: configurationManager,

		endpoints:       make(map[string][]*basev0.Endpoint),
		networkMappings: make(map[string][]*basev0.NetworkMapping),
	}
	world.runtimeContextFor = flow.runtimeContextFor
	// Per-developer runtime preferences (~/.codefly/preferences.yaml). Best
	// effort: a missing file = empty prefs (everything falls back to the global
	// runtime context); a malformed file is logged, not fatal — debugging should
	// never be blocked by a bad preference file.
	prefs, prefErr := resources.LoadUserPreferences(workspace.Dir())
	if prefErr != nil {
		w.Warn("ignoring malformed user preferences", wool.ErrField(prefErr))
		prefs = &resources.UserPreferences{}
	}
	flow.preferences = prefs

	return flow, nil
}

// SelectableRuntimeContexts returns every runtime context this run can put a
// service in: the one it was launched with, plus every override the
// preferences file can apply. The launch context alone does not answer what a
// run will do — preferences override it per service and per agent — so a
// caller deciding whether this run can reach Docker has to consider all of
// them. Sorted and deduplicated so the result does not depend on map order.
func (flow *Flow) SelectableRuntimeContexts() []string {
	if flow == nil {
		return nil
	}
	selectable := map[string]bool{flow.runtimeContext: true}
	if flow.preferences != nil && flow.preferences.Runtime != nil {
		preferences := flow.preferences.Runtime
		if preferences.Default != "" {
			selectable[preferences.Default] = true
		}
		for _, selected := range preferences.ByAgent {
			selectable[selected] = true
		}
		for _, selected := range preferences.ByService {
			selectable[selected] = true
		}
	}
	contexts := make([]string, 0, len(selectable))
	for context := range selectable {
		contexts = append(contexts, context)
	}
	slices.Sort(contexts)
	return contexts
}

// runtimeContextFor resolves the runtime context for a service: the developer's
// per-service/per-agent preference (~/.codefly/preferences.yaml) wins, else the
// global runtime context the run was launched with. This is what lets
// `codefly run service mind` run mind native (fast local Go) while its postgres
// dependency runs nix (Docker-free), with no committed project config.
func (flow *Flow) runtimeContextFor(svc *resources.Service) string {
	agentName := ""
	if svc != nil && svc.Agent != nil {
		agentName = svc.Agent.Name
	}
	serviceName := ""
	if svc != nil {
		serviceName = svc.Name
	}
	return flow.preferences.RuntimeContextFor(serviceName, agentName, flow.runtimeContext)
}

// DockerStatus captures whether the Docker engine is reachable and, for
// messaging, the docker context/endpoint the CLI resolved. Populated by the
// run command (which alone shells out to the docker CLI) and used to gate the
// free→nix runtime fallback per service.
type DockerStatus struct {
	Running  bool
	Context  string
	Endpoint string
}

// where reports the resolved docker endpoint for user-facing errors, e.g.
// `docker context "orbstack" → unix:///…` or `endpoint unix:///var/run/docker.sock`.
func (d DockerStatus) where() string {
	switch {
	case d.Context != "" && d.Endpoint != "":
		return fmt.Sprintf("docker context %q → %s", d.Context, d.Endpoint)
	case d.Endpoint != "":
		return fmt.Sprintf("endpoint %s", d.Endpoint)
	case d.Context != "":
		return fmt.Sprintf("docker context %q", d.Context)
	default:
		return "the default docker socket"
	}
}

// resolveDockerFallback finalizes the runtime context for every service still
// on "free" (codefly picks the backend). Each service is resolved against its
// plugin's PREFERENCE-ORDERED SupportedBackends, which is already filtered to
// what is available on this host. This keeps the CLI as the source of truth for
// the chosen backend and ensures every later boundary—including configuration
// filtering and environment export—sees the concrete runtime rather than the
// unresolved "free" hint.
//
// When Docker is known to be unavailable, Docker entries are excluded. A
// service whose only viable backend is Docker then triggers auto-start or an
// actionable error instead of failing deep in startup.
func (flow *Flow) resolveDockerFallback(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.resolveDockerFallback")
	if flow.hub == nil {
		return nil
	}

	// Phase 1 — classify every "free" service WITHOUT mutating it yet. The plugin
	// already filtered SupportedBackends to what is installed on THIS host, in
	// preference order (LOCAL > NIX > DOCKER), so trust it (single source of
	// truth). When Docker was probed and is unreachable, skip Docker; otherwise
	// it remains a selectable backend. An empty choice means no backend can run.
	type resolution struct {
		runner *Runner
		chosen string // native, nix, container, or "" when no backend can run
	}
	var resolutions []resolution
	needsDocker := false
	for _, m := range flow.hub.managers {
		manager, ok := m.(*Manager)
		if !ok || manager.Runner == nil {
			continue
		}
		runner := manager.Runner
		// Only the "free" default auto-resolves; an explicit context (native/
		// nix/container, or a per-service preference) is honored untouched.
		if runner.runtimeContext != resources.RuntimeContextFree {
			continue
		}
		backends := runner.SupportedBackends()
		// An agent that advertises NO backends gives us no capability information
		// to resolve "free" against. Blocking it here would regress every agent
		// that predates SupportedBackends (or simply leaves it unset), which ran
		// fine on the unresolved "free" hint. Leave it untouched; a genuinely
		// unrunnable agent fails later with its own diagnostic. Only agents that
		// advertise backends but none currently selectable are "blocked".
		if len(backends) == 0 {
			continue
		}
		chosen := ""
		for _, backend := range backends {
			switch backend {
			case agentv0.Backend_LOCAL:
				chosen = resources.RuntimeContextNative
			case agentv0.Backend_NIX:
				chosen = resources.RuntimeContextNix
			case agentv0.Backend_DOCKER:
				if !flow.dockerProbed || flow.docker.Running {
					chosen = resources.RuntimeContextContainer
				}
			}
			if chosen != "" {
				break
			}
		}
		resolutions = append(resolutions, resolution{runner: runner, chosen: chosen})
		if chosen == "" {
			needsDocker = true
		}
	}

	// If a service can ONLY run under Docker, try to start the engine before
	// falling back/blocking — much better UX than erroring when OrbStack/Docker
	// Desktop is merely stopped. Re-resolve after a successful start so every
	// service receives its concrete backend.
	if needsDocker && flow.dockerProbed && !flow.docker.Running && flow.startDocker {
		started, name, derr := dockerstart.EnsureRunning(ctx, flow.docker.Context)
		if derr == nil {
			if started {
				w.Info(fmt.Sprintf("Started the Docker engine via %s.", name))
			}
			flow.docker.Running = true
			return flow.resolveDockerFallback(ctx)
		}
		w.Debug("could not auto-start Docker", wool.ErrField(derr))
	}

	// Phase 2 — apply concrete selections and collect services with no viable
	// backend. Explicit runtime contexts were excluded above and remain intact.
	var fellBack, nixFallbacks, blocked []string
	for _, r := range resolutions {
		switch r.chosen {
		case "":
			blocked = append(blocked, r.runner.Unique())
		case resources.RuntimeContextNix:
			r.runner.WithRuntimeContext(r.chosen)
			if flow.dockerProbed && !flow.docker.Running {
				fellBack = append(fellBack, r.runner.Unique()+" → nix")
				nixFallbacks = append(nixFallbacks, r.runner.Unique())
			}
		default:
			r.runner.WithRuntimeContext(r.chosen)
			if flow.dockerProbed && !flow.docker.Running {
				fellBack = append(fellBack, r.runner.Unique()+" → native")
			}
		}
	}

	if len(blocked) > 0 {
		if !flow.dockerProbed || flow.docker.Running {
			return w.NewError("cannot run: no runtime backend available for %s. The agent advertised no executable backend for this environment.", strings.Join(blocked, ", "))
		}
		// No runtime backend is available for these services: Docker is down and
		// the plugin advertises no Docker-free backend installed on this host
		// (its native toolchain is missing AND nix is not installed) — or it
		// advertises no backend at all.
		return w.NewError("cannot run: no runtime backend available for %s. The Docker engine is not reachable (%s) and no Docker-free backend (native toolchain or nix) is installed for them. Start Docker, or install the native toolchain / nix, and re-run.",
			strings.Join(blocked, ", "), flow.docker.where())
	}
	if len(fellBack) > 0 {
		w.Warn(fmt.Sprintf("Docker engine not reachable (%s) — resolving %d service(s) to a Docker-free backend: %s.",
			flow.docker.where(), len(fellBack), strings.Join(fellBack, ", ")))
	}
	if len(nixFallbacks) > 0 {
		// Loud because switching a stateful service (a database) off its Docker
		// image onto the nix runtime can pair on-disk state with a different
		// engine version than the one that wrote it (e.g. a pg16 data dir under
		// nix pg17) — which surfaces later as an opaque startup failure.
		w.Warn(fmt.Sprintf("%d service(s) fell back to the nix runtime instead of their Docker image: %s. Stateful services may hit a data/version incompatibility with state created under Docker; if one fails to start, start Docker and re-run.",
			len(nixFallbacks), strings.Join(nixFallbacks, ", ")))
	}
	return nil
}

func (flow *Flow) Load(ctx context.Context) error {
	w := wool.Get(ctx).In("NewFlow")

	if flow.standAlone {
		w.Debug("running in stand-alone Mode")
	}

	// LoadRequired the resources
	var identities []*resources.ServiceIdentity
	for _, service := range flow.services {
		id, err := service.Identity()
		if err != nil {
			return w.Wrap(err)
		}
		identities = append(identities, id)
	}
	// An excluded root normally has no runtime manager and therefore is absent
	// from flow.services. When its process environment is the requested output,
	// its own and workspace configuration must still be admitted to the same
	// configuration manager as a real Runner would use. This loads configuration
	// authority only; it does not load the root agent or project process.
	if flow.exportsExcludedOriginEnvironment() {
		id, err := flow.originService.Identity()
		if err != nil {
			return w.Wrap(err)
		}
		identities = append(identities, id)
	}
	err := flow.ConfigurationManager.Restrict(ctx, identities)
	if err != nil {
		return w.Wrap(err)
	}

	err = flow.ConfigurationManager.Load(ctx, flow.world.Env.Runtime())
	if err != nil {
		return w.Wrap(err)
	}

	w.Debug("got resources",
		wool.Field("dns", flow.ConfigurationManager.DNS()))

	var playbook *Playbook

	switch flow.world.Mode {
	case RunMode:
		policy, err := NewRuntimeStartPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		if flow.loadOnly {
			w.Debug("load only")
			playbook.WithStoppingAfter(stopAfterRoots(flow.rootUniques(), RuntimeLoad))
		}
		if flow.initOnly {
			w.Debug("init only")
			playbook.WithStoppingAfter(stopAfterRoots(flow.rootUniques(), RuntimeInit))
		}
	case TestMode:
		policy, err := NewRuntimeTestPolicy(
			ctx,
			flow.world.Dependencies,
			flow,
			resources.WithUnique(flow.originService).Unique(),
			flow.testDependencyMode,
		)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		if flow.loadOnly {
			w.Debug("load only")
			playbook.WithStoppingAfter(stopAfterRoots(flow.rootUniques(), RuntimeLoad))
		}
		if flow.initOnly {
			w.Debug("init only")
			playbook.WithStoppingAfter(stopAfterRoots(flow.rootUniques(), RuntimeInit))
		}
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == RuntimeTest
		})
	case LintMode, CompileMode:
		terminal := RuntimeLint
		if flow.world.Mode == CompileMode {
			terminal = RuntimeBuild
		}
		origin := resources.WithUnique(flow.originService).Unique()
		policy, err := NewRuntimeValidationPolicy(ctx, flow.world.Dependencies, flow, origin, terminal)
		if err != nil {
			return w.Wrapf(err, "cannot create validation policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == origin && action.Type == terminal
		})

	case BuildMode:
		policy, err := NewBuildPolicy(ctx, flow.hub, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == BuilderBuild
		})
	case SyncMode:
		var policy PlaybookPolicy
		var err error
		origin := resources.WithUnique(flow.originService).Unique()
		if flow.syncRequest.GetDryRun() {
			policy, err = NewSyncDriftPolicy(ctx, flow.world.Dependencies, flow, origin)
		} else {
			policy, err = NewSyncPolicy(ctx, flow.world.Dependencies, flow)
		}
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == origin && action.Type == BuilderSync
		})
	case DeployMode:
		policy, err := NewDeployPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return action.Service == resources.WithUnique(flow.originService).Unique() && action.Type == BuilderDeploy
		})
	case SnapshotMode:
		policy, err := NewSnapshotPolicy(ctx, flow.world.Dependencies, flow)
		if err != nil {
			return w.Wrapf(err, "cannot create policy")
		}
		policy.standAlone = flow.standAlone
		flow.WithPolicy(policy)
		playbook, err = NewPlaybook(ctx, flow.world)
		if err != nil {
			return w.Wrapf(err, "cannot create playbook")
		}
		playbook.WithPolicy(policy)
		playbook.WithStoppingAfter(func(ctx context.Context, action Action) bool {
			return policy.completed(action)
		})

	}
	flow.playbook = playbook

	// Fix the callback
	for _, manager := range flow.hub.managers {
		manager.DoSetCallback(flow.playbook.Seed)
		// Wire post-start crashes into the flow so Start cancels the playbook.
		manager.DoSetFailureSink(flow.reportFailure)
	}

	return nil
}

func (flow *Flow) WithPolicy(policy PlaybookPolicy) *Flow {
	flow.policy = policy
	return flow
}

func (flow *Flow) Start(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Begin")
	if flow == nil {
		return w.NewError("cannot execute nil flow")
	}
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	flow.beginStart(cancel)
	defer flow.clearStart()

	err := flow.playbook.Begin(runCtx, flow.rootBeginActions()...)
	failure, failed := flow.endStart()
	if failed {
		failureErr := w.Wrapf(failure, "service failed after start")
		if err != nil {
			return errors.Join(failureErr, w.Wrapf(err, "cannot stop start playbook"))
		}
		return failureErr
	}
	if err != nil {
		return w.Wrapf(err, "cannot execute start playbook")
	}
	return nil
}

func (flow *Flow) Test(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Begin")
	if flow == nil {
		return w.NewError("cannot execute nil flow")
	}
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}

	err := flow.playbook.Begin(ctx, Action{Type: RuntimeBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute test playbook")
	}
	return nil
}

// OriginTestResponse returns the structured TestResponse from the origin
// service's Test RPC, or nil if it was never tested (e.g. the flow ran in a
// non-test mode, or the RPC failed before returning a response).
func (flow *Flow) OriginTestResponse() *runtimev0.TestResponse {
	if flow == nil || flow.hub == nil {
		return nil
	}
	origin := resources.WithUnique(flow.originService).Unique()
	for _, manager := range flow.hub.managers {
		if manager == nil {
			continue
		}
		if manager.Unique() == origin {
			return manager.RunnerTestResponse()
		}
	}
	return nil
}

// OriginTestSkipped reports whether the origin service's tests were skipped
// because its agent advertises no test capability. An agent that owns no suites
// is neither a pass nor a failure: there was nothing to run.
func (flow *Flow) OriginTestSkipped() bool {
	if flow == nil || flow.hub == nil {
		return false
	}
	origin := resources.WithUnique(flow.originService).Unique()
	for _, manager := range flow.hub.managers {
		if manager == nil || manager.Unique() != origin {
			continue
		}
		if source, ok := manager.(interface {
			RunnerTestSkipped() bool
		}); ok {
			return source.RunnerTestSkipped()
		}
		return false
	}
	return false
}

// OriginStartsForTest reports whether this test flow actually starts the origin.
// Only TEST_DEPENDENCY_MODE_START_STACK does: START_DEPENDENCIES replaces the
// origin's RuntimeStart with a sequencing barrier, and NONE skips Start
// altogether. Everything Codefly hands a service through StartRequest — its
// process overrides, the fixture, the exported runtime environment — therefore
// never reaches the service under test in those two modes. Resolved during
// InitManagers, so it is meaningful from Load onwards.
func (flow *Flow) OriginStartsForTest() bool {
	return flow != nil && flow.testDependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK
}

// TestDependencyModeName names the resolved dependency mode, for diagnostics
// that have to explain why a Start-delivered input could not be delivered.
func (flow *Flow) TestDependencyModeName() string {
	if flow == nil {
		return ""
	}
	return flow.testDependencyMode.String()
}

// OriginSyncResponse returns the structured SyncResponse from the origin
// service's Builder.Sync RPC, or nil if it was never synchronized.
func (flow *Flow) OriginSyncResponse() *builderv0.SyncResponse {
	if flow == nil || flow.hub == nil {
		return nil
	}
	origin := resources.WithUnique(flow.originService).Unique()
	for _, manager := range flow.hub.managers {
		if manager == nil || manager.Unique() != origin {
			continue
		}
		if source, ok := manager.(interface {
			BuilderSyncResponse() *builderv0.SyncResponse
		}); ok {
			return source.BuilderSyncResponse()
		}
		return nil
	}
	return nil
}

// OriginSyncSkipped reports whether the origin service's dry-run sync was
// skipped because its agent advertises no authoritative sync capability. A
// skipped drift check is neither a pass nor a failure: the agent owns no
// generated source that could drift.
func (flow *Flow) OriginSyncSkipped() bool {
	if flow == nil || flow.hub == nil {
		return false
	}
	origin := resources.WithUnique(flow.originService).Unique()
	for _, manager := range flow.hub.managers {
		if manager == nil || manager.Unique() != origin {
			continue
		}
		if source, ok := manager.(interface {
			BuilderSyncSkipped() bool
		}); ok {
			return source.BuilderSyncSkipped()
		}
		return false
	}
	return false
}

func (flow *Flow) DeploymentOutputs() map[string]*builderv0.DeploymentOutput {
	outputs := make(map[string]*builderv0.DeploymentOutput)
	if flow == nil || flow.hub == nil {
		return outputs
	}
	for _, manager := range flow.hub.managers {
		source, ok := manager.(interface {
			BuilderDeploymentOutput() *builderv0.DeploymentOutput
		})
		if !ok {
			continue
		}
		if output := source.BuilderDeploymentOutput(); output != nil {
			outputs[manager.Unique()] = output
		}
	}
	return outputs
}

// DeployedConfigurations returns, per deployed service unique, the
// configuration its Deploy exposed to consumers — in a restricted render, keys
// without values, some carrying the producer's template for assembling them.
func (flow *Flow) DeployedConfigurations() map[string]*basev0.Configuration {
	configurations := make(map[string]*basev0.Configuration)
	if flow == nil || flow.hub == nil {
		return configurations
	}
	for _, manager := range flow.hub.managers {
		source, ok := manager.(interface {
			BuilderDeployedConfiguration() *basev0.Configuration
		})
		if !ok {
			continue
		}
		if configuration := source.BuilderDeployedConfiguration(); configuration != nil {
			configurations[manager.Unique()] = configuration
		}
	}
	return configurations
}

// SelfEndpoints returns, per deployed service unique, the self-endpoint
// carriers derived from the network mappings its deploy recorded — the
// in-cluster address (container access) a render resolves, never the listen
// address. Core's builder emits the same carrier into the ConfigMap from v0.5.6;
// the render projects these after the fact so an agent built on an older core
// still carries it, and refuses a builder value that disagrees.
func (flow *Flow) SelfEndpoints(ctx context.Context) map[string]map[string]string {
	endpoints := map[string]map[string]string{}
	if flow == nil || flow.world == nil || flow.world.SharedState == nil {
		return endpoints
	}
	for _, unique := range flow.OrderedServiceUniques() {
		mappings, ok := flow.world.SharedState.GetNetworkMappingsFromUnique(unique)
		if !ok {
			continue
		}
		if variables := SelfEndpointEnvironmentVariables(ctx, mappings, resources.NewContainerNetworkAccess()); len(variables) > 0 {
			endpoints[unique] = variables
		}
	}
	return endpoints
}

// OriginImageDigest returns the immutable registry manifest digest of the image
// the origin service pushed during a build, or "" when nothing was pushed. It is
// how a targeted single-service build+push returns the digest to its caller.
func (flow *Flow) OriginImageDigest() string {
	if flow == nil || flow.hub == nil {
		return ""
	}
	origin := resources.WithUnique(flow.originService).Unique()
	for _, manager := range flow.hub.managers {
		if manager.Unique() != origin {
			continue
		}
		source, ok := manager.(interface{ BuilderImageDigest() string })
		if !ok {
			return ""
		}
		return source.BuilderImageDigest()
	}
	return ""
}

// OriginImageEvidence returns the digest-bound image SBOMs the origin service's
// build collected. A build with dependencies collects evidence for each of
// them; only the origin's is the caller's subject.
func (flow *Flow) OriginImageEvidence() []*builderv0.ImageSBOM {
	if flow == nil || flow.hub == nil {
		return nil
	}
	origin := resources.WithUnique(flow.originService).Unique()
	for _, manager := range flow.hub.managers {
		if manager.Unique() != origin {
			continue
		}
		source, ok := manager.(interface {
			BuilderImageEvidence() []*builderv0.ImageSBOM
		})
		if !ok {
			return nil
		}
		return source.BuilderImageEvidence()
	}
	return nil
}

func (flow *Flow) Build(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Build")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute build playbook")
	}
	return nil
}

func (flow *Flow) Sync(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.Sync")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute sync playbook")
	}
	return nil
}

func (flow *Flow) Deploy(ctx context.Context) error {
	if err := selectionguard.RejectUnboundExecution(flow.workspace.Dir(), flow.originModule.Dir()); err != nil {
		return err
	}
	w := wool.Get(ctx).In("flow.Deploy")
	// In stand-alone Mode, we set an ignore policy
	if flow.standAlone {
		flow.playbook.WithIgnore(func(ctx context.Context, action Action) bool {
			return action.Service != resources.WithUnique(flow.originService).Unique()
		})
	}
	err := flow.playbook.Begin(ctx, Action{Type: BuilderBegin, Service: resources.WithUnique(flow.originService).Unique()})
	if err != nil {
		return w.Wrapf(err, "cannot execute deploy playbook")
	}
	return nil

}

// Failure returns the first runner-level failure observed by the flow.
func (flow *Flow) Failure() (FlowFailure, bool) {
	if flow == nil {
		return FlowFailure{}, false
	}
	flow.failureMu.Lock()
	defer flow.failureMu.Unlock()
	if flow.failure == nil {
		return FlowFailure{}, false
	}
	return *flow.failure, true
}

// FailedService returns the service whose action failed the flow, the phase it
// failed in, and ok=false if the flow did not fail on a specific service's action.
// Lets the top-level command attribute a failure to the real culprit (e.g. a
// dependency that couldn't start) instead of always blaming the origin.
func (flow *Flow) FailedService() (string, ActionType, bool) {
	if flow == nil || flow.playbook == nil {
		return "", "", false
	}
	return flow.playbook.FailedService()
}

func (flow *Flow) reportFailure(unique, msg string) {
	if flow == nil {
		return
	}
	flow.failureMu.Lock()
	if flow.failure == nil {
		failure := FlowFailure{Service: unique, Message: msg}
		flow.failure = &failure
	}
	cancel := flow.startCancel
	flow.failureMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (flow *Flow) beginStart(cancel context.CancelFunc) {
	flow.failureMu.Lock()
	flow.startCancel = cancel
	failed := flow.failure != nil
	flow.failureMu.Unlock()
	if failed {
		cancel()
	}
}

func (flow *Flow) endStart() (FlowFailure, bool) {
	flow.failureMu.Lock()
	defer flow.failureMu.Unlock()
	flow.startCancel = nil
	if flow.failure == nil {
		return FlowFailure{}, false
	}
	return *flow.failure, true
}

func (flow *Flow) clearStart() {
	flow.failureMu.Lock()
	flow.startCancel = nil
	flow.failureMu.Unlock()
}

// ManagedServices returns the origin service name and the dependency service
// names this flow manages. flow.Stop() tears DOWN all of them — origin AND
// dependencies (neo4j, postgres, …) alike — so the runner UI can name exactly
// what is going away instead of a vague "stopping service". Nothing the flow
// started "stays alive" on stop; only nix-run service DATA persists on disk.
// WithStateListener registers a per-service lifecycle observer. Returns the
// flow for chaining.
func (flow *Flow) WithStateListener(l StateListener) *Flow {
	if flow != nil {
		flow.stateListener = l
	}
	return flow
}

// WithOutputSink routes this flow's narration to sink instead of the default
// no-op. Returns the flow for chaining. A nil sink is ignored.
func (flow *Flow) WithOutputSink(sink OutputSink) *Flow {
	if flow != nil && flow.world != nil && sink != nil {
		flow.world.OutputSink = sink
	}
	return flow
}

// WithAnswerProvider routes this flow's Sync-time interactive questions to
// provider instead of the default headless (defaults-only) behavior. Returns
// the flow for chaining. A nil provider is ignored.
func (flow *Flow) WithAnswerProvider(provider AnswerProvider) *Flow {
	if flow != nil && flow.world != nil && provider != nil {
		flow.world.AnswerProvider = provider
	}
	return flow
}

// emitState forwards a transition to the listener if one is set. Satisfies the
// stateEmitter interface so the runtime policy can report without knowing the
// flow concretely.
func (flow *Flow) emitState(service string, state tui.ServiceState, port int) {
	if flow == nil {
		return
	}
	// Record that THIS run has begun driving this service's runtime, so a later
	// readiness probe can trust the port belongs to us and not a leftover
	// process on the same hashed port. emitState is only ever called for a
	// service we are actively orchestrating, so recording unconditionally is
	// correct regardless of which phase this transition is.
	flow.beganMu.Lock()
	if flow.states == nil {
		flow.states = map[string]tui.ServiceState{}
	}
	flow.states[service] = state
	flow.beganMu.Unlock()

	if flow.stateListener != nil {
		flow.stateListener(service, state, port)
	}
}

// beganRun reports whether this run has emitted any runtime transition for the
// service — i.e. we actually launched it, as opposed to a stale process from a
// previous run holding the same deterministically hashed port.
func (flow *Flow) beganRun(service string) bool {
	flow.beganMu.Lock()
	defer flow.beganMu.Unlock()
	_, began := flow.states[service]
	return began
}

// startedRun reports whether the service's runtime Start completed
// successfully: the running state is emitted only once the Start call returns
// without error. An attempted — or still-running — Start is not readiness.
func (flow *Flow) startedRun(service string) bool {
	flow.beganMu.Lock()
	defer flow.beganMu.Unlock()
	return flow.states[service] == tui.StateRunning
}

// ServicePort returns a service's primary (first non-zero) port from the
// recorded network mappings, or 0 if not yet known. Used to surface "vault
// waiting(8721)" in the live status.
func (flow *Flow) ServicePort(service string) int {
	if flow == nil || flow.SharedState == nil {
		return 0
	}
	mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(service)
	if !ok {
		return 0
	}
	for _, m := range mappings {
		for _, inst := range m.Instances {
			if inst.Port > 0 {
				return int(inst.Port)
			}
		}
	}
	return 0
}

// ServiceReachable reports whether every one of a service's recorded network
// endpoints answers its declared health predicate (a real readiness probe),
// independent of where the synchronous action loop currently is. The loop emits
// a dependency's StateRunning only when its RuntimeStart call returns; while the
// loop is parked inside a long-blocking phase — typically the origin's `go
// build` — that emit is stuck, so an already-listening dependency keeps showing
// as "slow". Driving the live status off this probe instead makes it reflect
// reality, not loop timing. Returns false for services with no probeable
// endpoints (their status stays driven by the action-loop emit).
func (flow *Flow) ServiceReachable(ctx context.Context, service string) bool {
	if flow == nil || flow.SharedState == nil {
		return false
	}
	// Only trust a probe for a service this run actually launched. Codefly hashes
	// ports deterministically, so a zombie from a previous run squats on the very
	// port this run would use — probing before we've begun starting the service
	// would report that ghost as "ready". Requiring beganRun closes that window.
	if !flow.beganRun(service) {
		return false
	}
	mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(service)
	if !ok || len(mappings) == 0 {
		return false
	}
	// Judge the service by the same requirements readiness gates on, or the
	// live status contradicts the run: an endpoint nobody in this run consumes
	// must not keep a dependency showing as "slow" after the flow is ready.
	requirements, err := flow.serviceEndpointRequirements(ctx, service)
	if err != nil || len(requirements) == 0 {
		return false
	}
	return evaluateReadinessProbes(ctx, requirements) == nil
}

// PromoteReachable invokes onReady once for each managed dependency (origin
// excluded) that this run launched and is now accepting connections. It exists
// because the action loop emits a dependency's StateRunning only when its
// (blocking) Start call returns; while the loop is parked in a long phase — the
// origin's go compile being the usual culprit — an already-listening dependency
// keeps showing as "slow". Polling this from the UI drives status off real
// readiness instead. promoted dedupes across calls (each dep is reported once)
// and is owned by the caller's goroutine.
func (flow *Flow) PromoteReachable(ctx context.Context, origin string, promoted map[string]bool, onReady func(service string, port int)) {
	if flow == nil {
		return
	}
	for _, dep := range flow.OrderedServiceUniques() {
		if dep == origin || promoted[dep] {
			continue
		}
		if flow.ServiceReachable(ctx, dep) {
			promoted[dep] = true
			onReady(dep, flow.ServicePort(dep))
		}
	}
}

// OrderedServiceUniques returns the Unique of every managed service in
// dependency order (dependencies first, origin last) so the live status can
// show the "what's next" frontier before anything starts.
func (flow *Flow) OrderedServiceUniques() []string {
	if flow == nil {
		return nil
	}
	origin := ""
	if flow.originService != nil {
		origin = resources.WithUnique(flow.originService).Unique()
	}
	var out []string
	for _, s := range flow.services {
		if s == nil {
			continue
		}
		u := resources.WithUnique(s).Unique()
		if u == origin {
			continue
		}
		out = append(out, u)
	}
	if origin != "" {
		out = append(out, origin)
	}
	return out
}

// AgentCacheKeys returns every service-scoped agent owned by this flow,
// including managers created before a partial initialization failure. CI uses
// it to evict only completed flow agents while unrelated tasks remain active.
func (flow *Flow) AgentCacheKeys() []string {
	if flow == nil || flow.hub == nil {
		return nil
	}
	seen := map[string]bool{}
	keys := make([]string, 0, len(flow.hub.managers))
	for _, manager := range flow.hub.managers {
		if manager == nil {
			continue
		}
		key := manager.Unique()
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

func (flow *Flow) ManagedServices() (origin string, dependencies []string) {
	if flow == nil {
		return "", nil
	}
	if flow.originService != nil {
		origin = resources.WithUnique(flow.originService).Unique()
	}
	// Identify by module-qualified unique, not bare name: composed modules
	// routinely name their entry service the same thing, so a bare name both
	// dropped a service that shares the origin's name and rendered two managed
	// services identically. This is the same identity SendPlan already shows.
	for _, s := range flow.services {
		if s == nil {
			continue
		}
		unique := resources.WithUnique(s).Unique()
		if unique == origin {
			continue
		}
		dependencies = append(dependencies, unique)
	}
	return origin, dependencies
}

// output returns the flow's OutputSink, defaulting to a no-op if the flow
// isn't fully constructed. NewFlow always sets a non-nil OutputSink on
// world, but centralizing the guard here — rather than repeating (or
// omitting) it at every call site — keeps every narration call safe even
// against a Flow built by hand (as some tests do) instead of via NewFlow.
func (flow *Flow) output() OutputSink {
	if flow == nil || flow.world == nil || flow.world.OutputSink == nil {
		return noopOutputSink{}
	}
	return flow.world.OutputSink
}

// newTeardownContext builds a fresh, wool-instrumented background context for
// Stop/Shutdown, independent of the caller's (possibly already Done) context,
// logging through the flow's OutputSink.
func (flow *Flow) newTeardownContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := wool.New(ctx, resources.CLI.AsResource())
	provider.WithLogger(flow.output())
	ctx = provider.Inject(ctx)
	return ctx, func() {
		cancel()
		provider.Done()
	}
}

func (flow *Flow) Stop() error {
	if flow == nil || flow.hub == nil {
		return nil
	}
	// Builder-only flows have no Runtime runner to stop. Their service-scoped
	// agent connections are closed by the caller after this lifecycle hook.
	if flow.world != nil && (flow.world.Mode == BuildMode || flow.world.Mode == SyncMode || flow.world.Mode == DeployMode || flow.world.Mode == SnapshotMode) {
		return nil
	}
	return flow.runTeardown("Stop", defaultStopPhaseBudget, IManager.RunnerDoStop)
}

func (flow *Flow) Shutdown() error {
	if flow == nil || flow.hub == nil {
		return nil
	}
	return flow.runTeardown("Shutdown", defaultShutdownPhaseBudget, IManager.RunnerDoDestroy)
}

func (flow *Flow) GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error) {
	w := wool.Get(ctx).In("GetExecutor", wool.Field("action", action))
	manager, err := flow.hub.NewManager(action.Service)
	if err != nil {
		return nil, w.Wrap(err)
	}
	if action.Failed {
		return func(ctx context.Context) (*OutputProperty, error) {
			return Pause(), nil
		}, nil
	}
	switch action.Type {
	case RuntimeBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case RuntimeLoad:
		return manager.RunnerDoLoad, nil
	case RuntimeInit:
		return manager.RunnerDoInit, nil
	case RuntimeStart:
		return manager.RunnerDoStart, nil
	case RuntimeBuild:
		return manager.RunnerDoBuild, nil
	case RuntimeLint:
		return manager.RunnerDoLint, nil
	case RuntimeTest:
		return manager.RunnerDoTest, nil
	case BuilderBegin:
		return func(ctx context.Context) (*OutputProperty, error) {
			return OnInit(), nil
		}, nil
	case BuilderLoad:
		return manager.BuilderDoLoad, nil
	case BuilderInit:
		return manager.BuilderDoInit, nil
	case BuilderBuild:
		return manager.BuilderDoBuild, nil
	case BuilderSync:
		return manager.BuilderDoSync, nil
	case BuilderDeploy:
		return manager.BuilderDoDeploy, nil

	default:
		return nil, w.NewError("unknown action type %s for executor", action.Type)
	}
}

func (flow *Flow) GetDependenciesNetworkMappingsFor(ctx context.Context, service *resources.Service) ([]*basev0.NetworkMapping, error) {
	if flow == nil {
		return nil, nil
	}
	if flow.SharedState == nil {
		return nil, nil
	}
	return flow.SharedState.GetDependenciesNetworkMappings(ctx, service)
}

func (flow *Flow) GetAddressForEndpoint(ctx context.Context, module string, service string, endpoint string) (string, error) {
	if flow == nil {
		return "", fmt.Errorf("cannot get address from nil flow")
	}
	if flow.SharedState == nil {
		return "", fmt.Errorf("cannot get addresses from nil state")
	}
	// We get that from the stateManager
	unique := resources.ServiceUnique(module, service)
	mappings, ok := flow.SharedState.GetNetworkMappingsFromUnique(unique)
	if !ok {
		return "", fmt.Errorf("cannot get network mappings for %s", unique)

	}
	for _, np := range mappings {
		if np.Endpoint.Name == endpoint {
			// The CLI server is reached from the host, so its concrete answer
			// must prefer the host-native mapping. Private services normally
			// have native + container instances and no public instance.
			for _, instance := range np.Instances {
				if instance.Access != nil && instance.Access.Kind == resources.NetworkAccessNative {
					return instance.Address, nil
				}
			}
			// External endpoints may only have a public instance.
			for _, instance := range np.Instances {
				if instance.Access != nil && instance.Access.Kind == resources.NetworkAccessPublic {
					return instance.Address, nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot find network mappings for %s", unique)
}

func (flow *Flow) ServiceFromUnique(unique string) (*resources.Service, error) {
	return flow.world.Dependencies.ServiceFromUnique(unique)
}

func (flow *Flow) selectDependencyStage() error {
	if flow.world.Mode == SnapshotMode {
		// Snapshot spans build and run. Keep the union for membership; its
		// policy orders each stage independently.
		return flow.world.Dependencies.VerifyAcyclic(context.Background())
	}
	stage := resources.StageRun
	switch flow.world.Mode {
	case BuildMode:
		stage = resources.StageBuild
	}
	dependencies, err := flow.world.Dependencies.ForStage(stage)
	if err != nil {
		return err
	}
	flow.world.Dependencies = dependencies
	if flow.SharedState != nil {
		flow.SharedState.SetDependencies(dependencies)
	}
	return nil
}

// WithCoRoots adds services this run starts as roots beside the origin, given
// as module-qualified uniques.
func (flow *Flow) WithCoRoots(uniques ...string) *Flow {
	flow.coRoots = append(flow.coRoots, uniques...)
	return flow
}

// rootBeginActions is the opening action for every root, seeded together so
// each root drives its own closure through the same playbook.
func (flow *Flow) rootBeginActions() []Action {
	roots := flow.rootUniques()
	actions := make([]Action, 0, len(roots))
	for _, root := range roots {
		actions = append(actions, Action{Type: RuntimeBegin, Service: root})
	}
	return actions
}

// rootUniques returns every service this run starts as a root, origin first.
func (flow *Flow) rootUniques() []string {
	roots := make([]string, 0, len(flow.coRoots)+1)
	roots = append(roots, resources.WithUnique(flow.originService).Unique())
	return append(roots, flow.coRoots...)
}

// runClosure returns every service this run manages except the origin: the
// union of each root's dependency closure, plus the co-roots themselves.
//
// Each OrderTo lists a root's whole closure dependencies-first, so appending a
// root after its own closure and keeping the first occurrence of every service
// carries that ordering across the union — anything a service depends on was
// either already emitted for an earlier root or precedes it in this root's own
// order. With a single root it reduces to exactly OrderTo(origin).
func (flow *Flow) runClosure(ctx context.Context) ([]architecture.Service, error) {
	seen := map[string]bool{resources.WithUnique(flow.originService).Unique(): true}
	var required []architecture.Service
	add := func(unique string) {
		if seen[unique] {
			return
		}
		seen[unique] = true
		required = append(required, architecture.Service{Unique: unique})
	}
	for _, root := range flow.rootUniques() {
		order, err := flow.world.Dependencies.OrderTo(ctx, root)
		if err != nil {
			return nil, err
		}
		for _, service := range order {
			add(service.Unique)
		}
		add(root)
	}
	return required, nil
}

// scopeDependenciesToRun rebuilds the dependency graph with every service
// outside this run excluded, so what the policy reads is exactly what the flow
// manages. A run seeded from several roots cannot narrow its graph to any one
// of them, so this is what keeps propagation to a service's dependents from
// reaching a service the flow has no manager for.
func (flow *Flow) scopeDependenciesToRun(ctx context.Context, required []string, options []architecture.DependencyOption) error {
	inRun := map[string]bool{resources.WithUnique(flow.originService).Unique(): true}
	for _, unique := range required {
		inRun[unique] = true
	}
	var outside []string
	for _, service := range flow.world.Dependencies.Services() {
		if !inRun[service.Unique] {
			outside = append(outside, service.Unique)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	scoped := append(slices.Clone(options), architecture.ExcludeServices(outside...))
	dependencies, err := architecture.NewServiceDependencies(ctx, flow.dependencyWorkspace(), scoped...)
	if err != nil {
		return err
	}
	flow.world.Dependencies = dependencies
	if flow.SharedState != nil {
		flow.SharedState.SetDependencies(dependencies)
	}
	// The rebuild reloads the workspace, so it carries every edge kind again —
	// including the build-only ones selectDependencyStage already dropped. Re-run
	// that selection rather than repeat its rule here: without it a `build` or
	// `schema` edge between two services of the run set comes back as a runtime
	// ordering constraint, and paired with a runtime edge the other way it makes
	// the run graph cyclic.
	return flow.selectDependencyStage()
}

// managerDependencies chooses membership independently of snapshot stage order.
func (flow *Flow) managerDependencies(ctx context.Context) ([]architecture.Service, error) {
	origin := resources.WithUnique(flow.originService).Unique()
	if flow.world.Mode != SnapshotMode {
		return flow.runClosure(ctx)
	}
	closure, err := flow.world.Dependencies.Restrict(ctx, origin)
	if err != nil {
		return nil, err
	}
	build, err := closure.ForStage(resources.StageBuild)
	if err != nil {
		return nil, err
	}
	order, err := build.Graph().TopologicalSort()
	if err != nil {
		return nil, err
	}
	var required []architecture.Service
	for _, unique := range order {
		if unique != origin {
			required = append(required, architecture.Service{Unique: unique})
		}
	}
	return required, nil
}

func (flow *Flow) InitManagers(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.InitManagers")
	// Project ownership before anything is spawned, and hold it for every spawn
	// below: New() starts the agent process, which inherits the marker at exec
	// and can create containers from its Init onwards.
	containerRecoveryProjection.Lock()
	defer containerRecoveryProjection.Unlock()
	if _, err := flow.projectContainerRecovery(); err != nil {
		w.Warn("cannot project container recovery ownership; operations requiring recovery will be refused", wool.Field("error", err.Error()))
	}
	remotes := make(map[string]*Remote)
	var dependencyOptions []architecture.DependencyOption
	if flow.configurationReferences != nil {
		dependencyOptions = append(dependencyOptions, flow.configurationReferences)
	}
	if len(flow.remoteServices) > 0 {
		var cutoffs []string
		for _, remote := range flow.remoteServices {
			remotes[remote.Unique()] = remote
			cutoffs = append(cutoffs, remote.Unique())
		}
		dependencyOptions = append(dependencyOptions, architecture.SkipDependencyFor(cutoffs...))
	}
	if len(flow.excludedDependencyServices) > 0 {
		dependencyOptions = append(dependencyOptions, architecture.ExcludeServices(flow.excludedDependencyServices...))
	}
	if len(dependencyOptions) > 0 {
		dep, err := architecture.NewServiceDependencies(ctx, flow.dependencyWorkspace(), dependencyOptions...)
		if err != nil {
			return w.Wrap(err)
		}
		flow.world.Dependencies = dep
		if flow.SharedState != nil {
			flow.SharedState.SetDependencies(dep)
		}
	}

	if err := flow.selectDependencyStage(); err != nil {
		return w.Wrap(err)
	}

	// Test dependency policy is advertised by the origin agent, so load that
	// manager first as a preflight. It is retained and moved behind dependency
	// managers once the run set is known, preserving target-first teardown.
	flow.hub = &Hub{}
	var preloadedOrigin *Manager
	if flow.world.Mode == TestMode && !flow.excludeRoot {
		manager, err := New(ctx, flow.originModule, flow.originService, flow.world)
		flow.output().RegisterLoggingResource(resources.WithUnique(flow.originService).Unique())
		if err != nil {
			return w.Wrap(err)
		}
		preloadedOrigin = manager
		flow.hub.managers = append(flow.hub.managers, manager)
		flow.configureRunner(manager.Runner, flow.originService)
		if remote, ok := remotes[resources.WithUnique(flow.originService).Unique()]; ok {
			manager.Runner.WithRemote(remote.Environment)
		}
		if err := flow.configureTestExecution(manager.Runner); err != nil {
			return w.Wrap(err)
		}
	}

	// Create manager for every service required by this service when the
	// selected operation needs a live dependency graph.
	var required []string
	if !flow.standAlone {
		order, err := flow.managerDependencies(ctx)
		if err != nil {
			return w.Wrapf(err, "cannot order services")
		}
		for _, service := range order {
			required = append(required, service.Unique)
		}
		w.Debug("service dependencies", wool.NameField(flow.originService.Name), wool.Field("dependencies", required))
	}
	// We run in the proper order
	slices.Reverse(required)
	if len(flow.coRoots) > 0 {
		if err := flow.scopeDependenciesToRun(ctx, required, dependencyOptions); err != nil {
			return w.Wrap(err)
		}
	}
	if err := flow.validateDependencyEndpointDeclarations(required); err != nil {
		return w.Wrap(err)
	}
	if err := flow.logRunPlan(ctx, required, remotes); err != nil {
		return w.Wrapf(err, "cannot describe service run plan")
	}

	// Register each manager the moment it is
	// created. New() spawns the service's agent process (and its pgid tracking
	// file), so a failure partway through must still leave every
	// already-spawned runner reachable by flow.Stop() — otherwise a partial
	// init orphans those agents (and any process group they hold) until the
	// next run's reaper sweeps them.
	for _, unique := range required {
		flow.output().RegisterLoggingResource(unique)
		// Register source to handle "pretty" logging

		info, err := resources.ParseServiceWithOptionalModule(unique)
		w.Debug("creating run manager", wool.Field("for", unique))
		if err != nil {
			return w.Wrap(err)
		}

		mod, err := flow.workspace.LoadModuleFromName(ctx, info.Module)
		if err != nil {
			return w.Wrap(err)
		}

		svc, err := mod.LoadServiceFromName(ctx, info.Name)
		if err != nil {
			return w.Wrap(err)
		}

		flow.services = append(flow.services, svc)

		manager, err := New(ctx, mod, svc, flow.world)
		if err != nil {
			return w.Wrap(err)
		}

		flow.configureRunner(manager.Runner, svc)
		if remote, ok := remotes[unique]; ok {
			manager.Runner.WithRemote(remote.Environment)
		}
		flow.hub.managers = append(flow.hub.managers, manager)
	}

	// Now add the current one
	if preloadedOrigin != nil {
		flow.services = append(flow.services, flow.originService)
		// Dependency managers were appended after the preloaded origin. Rotate
		// the origin to the end so Stop() tears down the target before its stack.
		flow.hub.managers = append(flow.hub.managers[1:], preloadedOrigin)
	} else if !flow.excludeRoot {
		w.Debug("creating run manager", wool.Field("for", resources.WithUnique(flow.originService).Unique()))
		manager, err := New(ctx, flow.originModule, flow.originService, flow.world)
		flow.output().RegisterLoggingResource(resources.WithUnique(flow.originService).Unique())
		if err != nil {
			return w.Wrap(err)
		}
		flow.services = append(flow.services, flow.originService)
		flow.configureRunner(manager.Runner, flow.originService)
		if flow.testRequest != nil {
			manager.Runner.WithTestRequest(flow.testRequest)
		}
		if remote, ok := remotes[resources.WithUnique(flow.originService).Unique()]; ok {
			manager.Runner.WithRemote(remote.Environment)
		}
		flow.hub.managers = append(flow.hub.managers, manager)
	} else {
		// Keep the origin in the playbook so dependency ordering remains exactly
		// the same without loading its agent. If its SDK environment was requested,
		// the environment-only manager publishes it after dependency startup.
		noOp := NoOpManager{service: flow.originService}
		if flow.exportsExcludedOriginEnvironment() {
			flow.hub.managers = append(flow.hub.managers, &environmentOnlyManager{
				NoOpManager: noOp,
				export:      flow.exportExcludedOriginEnvironment,
			})
		} else {
			flow.hub.managers = append(flow.hub.managers, &noOp)
		}

	}

	// Now that every agent is loaded and advertises its SupportedBackends,
	// finalize the Docker/nix decision per service — falling back where safe and
	// stopping early where a service genuinely needs Docker.
	if err := flow.resolveDockerFallback(ctx); err != nil {
		return w.Wrap(err)
	}
	return nil
}

// validateDependencyEndpointDeclarations rejects a run whose services declare a
// dependency endpoint the producer does not expose — a reference nothing can
// satisfy, which readiness would otherwise discover by looking for a mapping
// that can never appear, turning a manifest typo into a run that waits forever
// instead of an error. Whether the producer grants the consumer that endpoint is
// a separate verdict, reached before the flow exists by runModuleClosure.
// Only declarations inside this run's service set are checked; one pointing
// outside it (excluded or remote-cut) is not this run's requirement.
func (flow *Flow) validateDependencyEndpointDeclarations(uniques []string) error {
	runSet := make(map[string]bool, len(uniques)+1)
	for _, unique := range uniques {
		runSet[unique] = true
	}
	consumers := append([]string{}, uniques...)
	consumers = append(consumers, resources.WithUnique(flow.originService).Unique())
	for _, unique := range consumers {
		consumer, err := flow.world.Dependencies.ServiceFromUnique(unique)
		if err != nil {
			return err
		}
		for _, dependency := range consumer.ServiceDependencies {
			if !runSet[dependency.Unique()] {
				continue
			}
			producer, err := flow.world.Dependencies.ServiceFromUnique(dependency.Unique())
			if err != nil {
				return err
			}
			for _, reference := range dependency.Endpoints {
				if !producerExposesReference(producer, reference) {
					return fmt.Errorf("service %s declares %s of %s, which exposes no such endpoint",
						unique, endpointReferenceLabel(reference), dependency.Unique())
				}
			}
		}
	}
	return nil
}

// producerExposesReference reports whether a producer's manifest can ever
// satisfy one declared reference, through the same name/API matching readiness
// selects with.
func producerExposesReference(producer *resources.Service, reference *resources.EndpointReference) bool {
	for _, endpoint := range producer.Endpoints {
		if endpointMatchesReference(&basev0.Endpoint{Name: endpoint.Name, Api: endpoint.API}, reference) {
			return true
		}
	}
	return false
}

func (flow *Flow) configureRunner(runner *Runner, service *resources.Service) {
	// A build-mode manager carries a Builder and no Runner (Manager.Load);
	// every setter below tolerates the nil, the field write must too.
	if runner == nil {
		return
	}
	runner.containerRecoveryIdentity = flow.containerRecoveryIdentity
	runner.WithRuntimeContext(flow.runtimeContextFor(service))
	runner.WithFixture(flow.fixture)
	runner.WithOverrides(flow.overridesFor(service))
	if flow.exportsRuntimeEnvironmentFor(service) {
		runner.WithOutputEnv(flow.outputEnvPath)
	}
}

func (flow *Flow) configureTestExecution(runner *Runner) error {
	execution, err := resolveTestExecution(runner.instance.Info, flow.testRequest)
	if err != nil {
		return fmt.Errorf("cannot resolve test suite for %s: %w", runner.Unique(), err)
	}
	flow.testRequest = execution.Request
	flow.testDependencyMode = execution.DependencyMode
	flow.standAlone = execution.DependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE
	runner.WithTestRequest(execution.Request)
	runner.WithServiceRunningForTest(execution.DependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK)
	if execution.skipped {
		flow.output().Info("Agent for <%s> advertises no test suites; skipping tests", runner.Unique())
		return nil
	}
	flow.output().Info("Test suite <%s> for <%s> uses dependency mode %s", execution.DisplaySuite(), runner.Unique(), execution.DependencyMode.String())
	return nil
}

func (flow *Flow) logRunPlan(ctx context.Context, dependencyUniques []string, remotes map[string]*Remote) error {
	if flow == nil || flow.originService == nil {
		return nil
	}
	origin := resources.WithUnique(flow.originService).Unique()
	runSet := append([]string{}, dependencyUniques...)
	if !flow.excludeRoot {
		runSet = append(runSet, origin)
	}
	if len(runSet) == 0 {
		flow.output().Info("Will run no local services for <%s>", origin)
		return nil
	}

	entries := make([]string, 0, len(runSet))
	for _, unique := range runSet {
		svc := flow.originService
		if unique != origin {
			var err error
			svc, err = flow.world.Dependencies.ServiceFromUnique(unique)
			if err != nil {
				return err
			}
		}
		entries = append(entries, formatServiceRunPlanEntry(unique, svc, remotes[unique]))
	}
	flow.output().Info("Will run %d service(s): %s", len(entries), strings.Join(entries, "; "))
	return nil
}

func formatServiceRunPlanEntry(unique string, service *resources.Service, remote *Remote) string {
	serviceVersion := "unknown"
	agent := "agent unknown"
	if service != nil {
		if service.Version != "" {
			serviceVersion = service.Version
		}
		if service.Agent != nil {
			agent = service.Agent.Identifier()
		}
	}
	entry := fmt.Sprintf("%s@%s via %s", unique, serviceVersion, agent)
	if remote != nil && remote.Environment != nil && remote.Environment.Name != "" {
		entry += fmt.Sprintf(" remote:%s", remote.Environment.Name)
	}
	return entry
}

func (flow *Flow) CreateManager(ctx context.Context) error {
	w := wool.Get(ctx).In("flow.InitManagers")
	w.Debug("creating run manager", wool.Field("for", resources.WithUnique(flow.originService).Unique()))
	manager, err := New(ctx, flow.originModule, flow.originService, flow.world)
	flow.output().RegisterLoggingResource(resources.WithUnique(flow.originService).Unique())
	if err != nil {
		return w.Wrap(err)
	}
	flow.hub = &Hub{managers: []IManager{manager}}
	return nil
}

func reachableNetworkInstance(instances []*basev0.NetworkInstance) *basev0.NetworkInstance {
	for _, instance := range instances {
		if instance.GetAccess().GetKind() == resources.NetworkAccessNative {
			return instance
		}
	}
	for _, instance := range instances {
		if instance.GetAccess().GetKind() == resources.NetworkAccessPublic {
			return instance
		}
	}
	if len(instances) == 0 {
		return nil
	}
	return instances[0]
}

func networkInstanceDialAddress(instance *basev0.NetworkInstance) string {
	if instance.GetHost() != "" {
		return instance.GetHost()
	}
	if instance.GetHostname() != "" && instance.GetPort() != 0 {
		return net.JoinHostPort(instance.GetHostname(), strconv.Itoa(int(instance.GetPort())))
	}
	address := instance.GetAddress()
	u, err := url.Parse(address)
	if err == nil && u.Host != "" {
		return u.Host
	}
	return address
}

func (flow *Flow) WithDeploymentManager(manager deployments.Manager) {
	flow.world.RemoteManager = manager
}

func (flow *Flow) WithDeploymentDestination(destination func(*resources.Module, *resources.Service) string) {
	flow.world.DeploymentDestination = destination
}

func (flow *Flow) WithKubernetesOutputProfile(profile builderv0.KubernetesOutputProfile) {
	flow.world.KubernetesOutputProfile = profile
}

// WithClusterValidation opts a promotable render into a server-side dry-run
// against the environment's declared cluster (`--validate-cluster`).
func (flow *Flow) WithClusterValidation(validate bool) {
	flow.world.ValidateCluster = validate
}

func (flow *Flow) WithStandAlone(alone bool) {
	flow.standAlone = alone
}

func (flow *Flow) WithPush(push bool) {
	flow.world.Push = push
}

func (flow *Flow) WithBuildCache(cache *builderv0.BuildCacheOptions) {
	flow.world.BuildCache = scopedBuildCache(cache)
}

func (flow *Flow) WithBuildxBuilder(name string) {
	flow.world.BuildxBuilder = name
}

func (flow *Flow) WithImageDigest(capture bool) {
	flow.world.CaptureImageDigest = capture
}

func (flow *Flow) WithImageSBOM(collect bool) {
	flow.world.CollectImageSBOM = collect
}

func (flow *Flow) WithRuntimeContext(runtimeContext string) {
	flow.runtimeContext = runtimeContext
}

// WithTemporaryPorts switches this flow from deterministic named ports to
// OS-probed ephemeral ports. It is intended for short-lived SDK-owned test
// dependency stacks, where separate package processes have independent
// RuntimeManagers and therefore cannot coordinate deterministic hash
// collisions through one in-memory allocation table.
func (flow *Flow) WithTemporaryPorts(enabled bool) {
	flow.temporaryPorts = enabled
	if enabled && flow.world != nil && flow.world.LocalNetworkManager != nil {
		flow.world.LocalNetworkManager.WithTemporaryPorts()
	}
}

// WithIsolatedInvocation marks this flow as a disposable invocation: it takes a
// fresh identity and runs under it as its naming scope, which is what agents
// fold into the container names, runtime state directories and log roots they
// derive (core's services.Base.UniqueWithWorkspace). Without it two disposable
// flows in one workspace shared every one of those names, so stopping either
// destroyed the other's resources.
//
// Only a caller that knows its resources are throwaway may ask for this: it is
// deliberately not implied by temporary ports, which `codefly ci run` uses to
// isolate a port space inside an already-isolated CODEFLY_HOME.
//
// Returns the identity it generated, or empty when the caller already named a
// scope of its own — that label wins, and nothing is generated over it.
func (flow *Flow) WithIsolatedInvocation() string {
	if flow.world == nil || flow.world.Env == nil || flow.world.Env.NamingScope != "" {
		return ""
	}
	flow.world.Env.NamingScope = NewInvocationID()
	return flow.world.Env.NamingScope
}

func (flow *Flow) TemporaryPortsEnabled() bool {
	return flow != nil && flow.temporaryPorts
}

// WithPortOverrides pins endpoints to caller-chosen host ports, keyed by
// EndpointDestination (module/service/endpoint). An override takes precedence
// over both the deterministic hash and temporary ports.
func (flow *Flow) WithPortOverrides(overrides map[string]uint16) {
	if len(overrides) == 0 {
		return
	}
	flow.portOverrides = overrides
	if flow.world != nil && flow.world.LocalNetworkManager != nil {
		flow.world.LocalNetworkManager.WithPortOverrides(overrides)
	}
}

func (flow *Flow) WithDockerStatus(status DockerStatus) {
	flow.docker = status
	flow.dockerProbed = true
}

// WithStartDocker enables auto-starting a local Docker engine when a service can
// only run under Docker and the daemon is not reachable (--start-docker).
func (flow *Flow) WithStartDocker(startDocker bool) {
	flow.startDocker = startDocker
}

func (flow *Flow) WithFixture(fixture string) {
	flow.fixture = fixture
}

func (flow *Flow) WithOverrides(overrides map[string]map[string]string) {
	flow.overrides = overrides
}

// WithWorkspaceConfigurationValues sets values the run path derives for named
// workspace configuration groups (group -> key -> value). They reach only the
// services that declare a dependency on the group, exactly as a declared value
// would.
func (flow *Flow) WithWorkspaceConfigurationValues(values map[string]map[string]string) {
	if flow.world == nil {
		return
	}
	flow.world.workspaceConfigurationValues = values
}

// overridesFor returns the runtime overrides targeting service, layering the
// bare-name entry under the module-qualified one. A module-qualified entry wins
// KEY BY KEY: the bare name is what --set supplies and is ambiguous across
// composed modules, so an entry that names the module addresses exactly one
// service and must not be diluted by a same-named service elsewhere in the
// graph — but returning only one of the two maps silently dropped every key the
// other set. The run path derives module-qualified overrides itself, so picking
// instead of layering made a derived injection suppress an operator's whole
// --set for that service without a word.
func (flow *Flow) overridesFor(service *resources.Service) map[string]string {
	if len(flow.overrides) == 0 || service == nil {
		return nil
	}
	byName := flow.overrides[service.Name]
	byUnique := flow.overrides[resources.WithUnique(service).Unique()]
	if byName == nil {
		return byUnique
	}
	if byUnique == nil {
		return byName
	}
	merged := make(map[string]string, len(byName)+len(byUnique))
	maps.Copy(merged, byName)
	maps.Copy(merged, byUnique)
	return merged
}

func (flow *Flow) WithExcludeRoot(excludeRoot bool) {
	flow.excludeRoot = excludeRoot
}

func (flow *Flow) WithInitOnly(only bool) {
	flow.initOnly = only
}

func (flow *Flow) Executed() []Action {
	return flow.playbook.Executed()
}

func (flow *Flow) WithLoadOnly(only bool) {
	flow.loadOnly = only

}

// WithTestRequest sets the TestRequest forwarded to the origin runner's
// Test RPC. Only relevant in TestMode; ignored otherwise.
func (flow *Flow) WithTestRequest(req *runtimev0.TestRequest) {
	flow.testRequest = req
}

// WithSyncRequest sets the request used by SyncMode builders. CI uses a
// dry-run request so generated drift can be proven without mutating source.
func (flow *Flow) WithSyncRequest(req *builderv0.SyncRequest) {
	flow.syncRequest = req
	if flow.world != nil {
		flow.world.SyncRequest = req
	}
}

func (flow *Flow) ActiveWorkspace() *resources.Workspace {
	return flow.workspace
}

// dependencyWorkspace is the workspace every (re)build of the dependency graph
// reads, so a rebuild cannot silently widen the run back to modules no root
// reaches. A Flow assembled field-by-field rather than through NewFlow carries
// no closure, and falls back to the composition it does carry.
func (flow *Flow) dependencyWorkspace() *resources.Workspace {
	if flow.graphWorkspace != nil {
		return flow.graphWorkspace
	}
	return flow.workspace
}

// Environment returns the environment this flow runs against — the same
// runtime projection is handed to the configuration manager and agents.
func (flow *Flow) Environment() *environments.Environment {
	if flow == nil || flow.world == nil {
		return nil
	}
	return flow.world.Env
}

func (flow *Flow) Origin() *resources.Service {
	return flow.originService
}

func (flow *Flow) WithOutputEnv(envPath string) {
	envPath = strings.TrimSpace(envPath)
	flow.outputEnvPath = envPath
	flow.outputEnvService = ""
	if envPath == "" {
		return
	}
	// Delete the file first
	if exists, err := shared.FileExists(context.Background(), envPath); err == nil && exists {
		err := shared.DeleteFile(context.Background(), envPath)
		if err != nil {
			flow.output().Error("cannot delete file %s: %s", envPath, err)
		}
	}
}

// WithOutputEnvService selects which one service contributes the flat
// --output-env artifact. Without an explicit selection the origin service is
// exported. Multiple service environments cannot be appended safely because
// identity and endpoint keys overlap and the last runner would silently win.
func (flow *Flow) WithOutputEnvService(unique string) {
	flow.outputEnvService = strings.TrimSpace(unique)
}

func (flow *Flow) exportsRuntimeEnvironmentFor(service *resources.Service) bool {
	if flow == nil || flow.outputEnvPath == "" || service == nil {
		return false
	}
	target := flow.outputEnvService
	if target == "" && flow.originService != nil {
		target = resources.WithUnique(flow.originService).Unique()
	}
	return resources.WithUnique(service).Unique() == target
}

// exportsExcludedOriginEnvironment reports whether --exclude-root selected
// the root as the sole owner of the output environment. The default output
// target is the root, so an explicit --output-env-service is not required.
func (flow *Flow) exportsExcludedOriginEnvironment() bool {
	return flow != nil && flow.excludeRoot && flow.exportsRuntimeEnvironmentFor(flow.originService)
}

type Remote struct {
	*resources.ServiceWithModule
	*environments.Environment
}

func (flow *Flow) WithRemotes(services []*Remote) {
	flow.remoteServices = services
}

// WithRunProfile applies an already-resolved run profile (the canonical,
// validated exclusions produced by resources.Workspace.ResolveRunProfile) to a
// run or test flow. Profiles only trim local runtime composition, so this rejects any other
// flow mode. The excluded dependency references are canonical module/service
// uniques, matching what architecture.ExcludeServices keys on.
func (flow *Flow) WithRunProfile(profile resources.RunProfile) error {
	if flow == nil || flow.world == nil || (flow.world.Mode != RunMode && flow.world.Mode != TestMode) {
		return fmt.Errorf("run profiles can only be applied to run or test flows")
	}
	flow.excludedDependencyServices = append([]string(nil), profile.ExcludeDependencies...)
	flow.world.excludedWorkspaceConfigurations = make(map[string]bool, len(profile.ExcludeWorkspaceConfigurations))
	for _, configuration := range profile.ExcludeWorkspaceConfigurations {
		flow.world.excludedWorkspaceConfigurations[configuration] = true
	}
	return nil
}

var _ ExecutorManager = &Flow{}
