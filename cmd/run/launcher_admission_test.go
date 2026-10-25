package run

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentservices "github.com/codefly-dev/core/agents/services"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	coreservices "github.com/codefly-dev/core/services"
	"google.golang.org/grpc/credentials/insecure"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/run/identity"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type admissionFlow struct {
	pairedLauncherFlow
	selected []string
}

func (f *admissionFlow) LocalRuntimeServices() []string { return f.selected }
func (f *admissionFlow) ReadyEndpointCarriers(_ context.Context, module string, _ *basev0.NetworkAccess, ready []string) (map[string]string, error) {
	carriers := map[string]string{}
	for _, unique := range ready {
		ref, _ := resources.ParseServiceWithOptionalModule(unique)
		key := resources.EndpointAsEnvironmentVariableKey(&resources.EndpointInformation{Module: module, Service: ref.Name, Name: "http", API: "http"})
		carriers[key] = "http://" + f.address
	}
	return carriers, nil
}

type admissionPeer struct {
	compositionv1.UnimplementedCompositionServiceServer
	mu        sync.Mutex
	manifests []*solutionhost.ModuleManifest
}

func (p *admissionPeer) ApplyManifest(_ context.Context, req *compositionv1.ApplyManifestRequest) (*compositionv1.ApplyManifestResponse, error) {
	m, err := solutionhost.ManifestFromVerified(req.GetCarrier().GetDocument())
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.manifests = append(p.manifests, m)
	p.mu.Unlock()
	return &compositionv1.ApplyManifestResponse{Generation: m.Generation}, nil
}

func (p *admissionPeer) snapshots() []*solutionhost.ModuleManifest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*solutionhost.ModuleManifest(nil), p.manifests...)
}

func admissionLauncher(t *testing.T) (*launcher, *admissionPeer) {
	t.Helper()
	workspace, env := writeHostedNativeWorkspace(t)
	moduleFile := filepath.Join(workspace.Dir(), "modules", "model", resources.ModuleConfigurationName)
	require.NoError(t, os.WriteFile(moduleFile, []byte("kind: module\nname: model\nservices:\n  - name: model\ninterface:\n  endpoints:\n    - service: model\n      endpoint: rest\n      implements: [codefly.dev/composition@1.0.0]\n"), 0600))
	workspace = composeModule(t, workspace, "alpha")
	workspace = composeModule(t, workspace, "beta")
	for _, name := range []string{"alpha", "beta"} {
		dir := filepath.Join(workspace.Dir(), "modules", name)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "module.package.codefly.yaml"), []byte("kind: module-package\nschema: codefly/module-package/v2\nid: example/"+name+"\nversion: 1.0.0\nminimum-codefly-version: '>=0.0.0'\nartifact-roots: [services]\ncontracts:\n  composition: '>=1.0'\n"), 0600))
		path := filepath.Join(dir, "services", "api", resources.ServiceConfigurationName)
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(body, []byte("endpoints:\n  - name: http\n    api: http\n    visibility: internal\n")...), 0600))
	}
	var err error
	workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &admissionPeer{}
	server := grpc.NewServer()
	compositionv1.RegisterCompositionServiceServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := solutionhost.NewInMemorySigner("test", key)
	require.NoError(t, err)
	vouch, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, t.TempDir())
	require.NoError(t, err)
	identities, err := gitops.ResolveNativeIdentities(t.Context(), workspace, env)
	require.NoError(t, err)
	flow := &admissionFlow{pairedLauncherFlow: pairedLauncherFlow{fakeLauncherFlow: fakeLauncherFlow{ready: func(string) bool { return true }}, address: listener.Addr().String()}, selected: []string{"model/model", "alpha/api", "beta/api"}}
	cluster := &launcher{workspace: workspace, env: env, signer: signer, vouch: vouch, identities: identities, stateDir: t.TempDir(), hostModule: "model", hostService: "model", hostEndpoint: "rest", flow: flow, running: map[string]bool{}, delivered: map[string]*solutionhost.ModuleManifest{}, schedule: testDeliverySchedule, stop: make(chan struct{})}
	cluster.declare(identities)
	t.Cleanup(cluster.close)
	return cluster, peer
}

// Exercise the same begin/admit/complete contract that Runner.Start owns.
func admitRuntimeAttempt(t *testing.T, cluster *launcher, service *resources.Service) (func(bool), error) {
	t.Helper()
	attempt, err := cluster.beginRuntimeStart(t.Context(), service)
	if err != nil {
		return attempt.Complete, err
	}
	if attempt.Admit != nil {
		err = attempt.Admit(t.Context())
	}
	return attempt.Complete, err
}

func TestAdmissionPrecedesStartAndFailureDoesNotWithdraw(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: "alpha", Name: "api"})
	require.NoError(t, err)
	finish, err := admitRuntimeAttempt(t, cluster, service)
	require.NoError(t, err)
	snapshots := peer.snapshots()
	require.Len(t, snapshots, 2, "both selected audiences must be applied before Start")
	for _, m := range snapshots {
		require.NotEmpty(t, m.Workloads)
		require.Empty(t, m.Endpoints)
		require.False(t, m.Removed)
	}
	finish(false)
	_, projected := cluster.vouch.Projected("alpha/api")
	require.False(t, projected)
	require.Len(t, cluster.declaredIdentities("alpha"), 1, "failed Start retains selected membership")
	finish, err = admitRuntimeAttempt(t, cluster, service)
	require.NoError(t, err)
	_, projected = cluster.vouch.Projected("alpha/api")
	require.True(t, projected)
	for _, m := range peer.snapshots() {
		require.False(t, m.Removed)
		require.EqualValues(t, 1, m.Generation)
	}
	finish(true)
	require.Eventually(t, func() bool {
		for _, m := range peer.snapshots() {
			if m.Alias == "alpha" && len(m.Endpoints) > 0 {
				return true
			}
		}
		return false
	}, time.Second, time.Millisecond)
}

func TestStaleStartCompletionCannotChangeNewAttempt(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: "alpha", Name: "api"})
	require.NoError(t, err)
	old, err := admitRuntimeAttempt(t, cluster, service)
	require.NoError(t, err)
	current, err := admitRuntimeAttempt(t, cluster, service)
	require.NoError(t, err)
	old(false)
	_, projected := cluster.vouch.Projected("alpha/api")
	require.True(t, projected)
	old(true)
	cluster.mu.Lock()
	ready := cluster.running["alpha/api"]
	cluster.mu.Unlock()
	require.False(t, ready)
	for _, m := range peer.snapshots() {
		require.Empty(t, m.Endpoints)
	}
	current(false)
}

func TestNativeAuthorityOrdersHostWithoutAuthoredDependency(t *testing.T) {
	cluster, _ := admissionLauncher(t)
	workspace := cluster.composed()
	module, err := workspace.LoadModuleFromName(t.Context(), "alpha")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, cluster.env, orchestration.RunMode,
		orchestration.WithRunModuleClosure("alpha"), NativeAuthorityPrerequisites(cluster.env, resources.RuntimeContextNative))
	require.NoError(t, err)
	run, err := flow.World().Dependencies.ForStage(resources.StageRun)
	require.NoError(t, err)
	order, err := run.OrderTo(t.Context(), "alpha/api")
	require.NoError(t, err)
	require.Len(t, order, 1)
	require.Equal(t, "model/model", order[0].Unique)
	build, err := flow.World().Dependencies.ForStage(resources.StageBuild)
	require.NoError(t, err)
	order, err = build.OrderTo(t.Context(), "alpha/api")
	require.NoError(t, err)
	require.Empty(t, order)
}

// writeTargetBinding gives alpha one binding, read-target, addressed to the
// audience the composition names.
func writeTargetBinding(t *testing.T, cluster *launcher, audience string) {
	t.Helper()
	contract := "schema: codefly/module-contract/v1\nprincipal: alpha\nnamespaces: []\nqueues: []\ndestinations: []\nscope_ceilings: []\nbindings:\n  - id: read-target\n    operations: [invoke]\n    audience: {from: target/read-audience}\n    scope_ceiling:\n      invoke:\n        - resource_kind: target.item\n          actions: [read]\n"
	configDir := filepath.Join(cluster.composed().Dir(), "configurations", "local")
	require.NoError(t, os.MkdirAll(configDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "target.env"), []byte("READ_AUDIENCE="+audience+"\n"), 0600))
	path := filepath.Join(cluster.composed().Dir(), "modules", "alpha", "module.contract.codefly.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contract), 0600))
}

// A binding to a module the run selects but declares to no authority (the
// host's own) could never be served: admission refuses the whole selection
// before sending any manifest.
func TestAdmissionRefusesUndeliveredBindingBeforeSending(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	writeTargetBinding(t, cluster, "model")
	err := cluster.admitSelected(t.Context())
	require.ErrorContains(t, err, "binding read-target targets model")
	require.Empty(t, peer.snapshots(), "validate the entire selection before applying any manifest")
}

// A run composes a unit and only the modules it consumes: a binding to a
// module outside the run does not stop startup. The delivered manifest
// carries no grant through it, every selected module is still admitted with
// its own workloads only, and the selected target's binding is kept.
func TestAdmissionWithholdsBindingToUnselectedModule(t *testing.T) {
	for _, tc := range []struct {
		audience string
		kept     bool
	}{{"missing", false}, {"beta", true}} {
		t.Run(tc.audience, func(t *testing.T) {
			cluster, peer := admissionLauncher(t)
			writeTargetBinding(t, cluster, tc.audience)
			require.NoError(t, cluster.admitSelected(t.Context()))
			snapshots := peer.snapshots()
			require.Len(t, snapshots, 2, "every selected module is admitted")
			for _, m := range snapshots {
				require.NotNil(t, m.Authority)
				for _, w := range m.Workloads {
					require.Equal(t, "api", w.Service, "an identity is declared for a selected workload only")
				}
				if m.Alias != "alpha" {
					continue
				}
				var audiences []string
				for _, binding := range m.Authority.Bindings {
					audiences = append(audiences, binding.Audience)
				}
				if tc.kept {
					require.Equal(t, []string{tc.audience}, audiences, "a binding to a selected module is delivered")
				} else {
					require.Empty(t, audiences, "a binding to a module outside the run carries no grant")
				}
			}
			// The ready delivery renders through the same path and withholds it too.
			cluster.running["alpha/api"] = true
			require.NoError(t, cluster.deliverReadiness(t.Context(), "alpha", false))
			latest := peer.snapshots()[len(peer.snapshots())-1]
			require.Equal(t, "alpha", latest.Alias)
			require.Equal(t, tc.kept, len(latest.Authority.Bindings) == 1)
		})
	}
}

func TestFailedPreparationRemovesPublishedRoutesAndKeepsSibling(t *testing.T) {
	for _, overlap := range []bool{false, true} {
		t.Run(fmt.Sprint(overlap), func(t *testing.T) {
			cluster, peer := admissionLauncher(t)
			load := func(name string) *resources.Service {
				service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: name, Name: "api"})
				require.NoError(t, err)
				return service
			}
			for _, name := range []string{"alpha", "beta"} {
				finish, err := admitRuntimeAttempt(t, cluster, load(name))
				require.NoError(t, err)
				finish(true)
			}
			latest := func() map[string]*solutionhost.ModuleManifest {
				out := map[string]*solutionhost.ModuleManifest{}
				for _, m := range peer.snapshots() {
					out[m.Alias] = m
				}
				return out
			}
			require.Eventually(t, func() bool {
				m := latest()
				return m["alpha"] != nil && m["beta"] != nil && len(m["alpha"].Endpoints) > 0 && len(m["beta"].Endpoints) > 0
			}, time.Second, time.Millisecond)
			ctx, cancel := context.WithCancel(t.Context())
			attempt, err := cluster.beginRuntimeStart(ctx, load("alpha"))
			require.NoError(t, err)
			if overlap {
				old := attempt
				attempt, err = cluster.beginRuntimeStart(ctx, load("alpha"))
				require.NoError(t, err)
				old.Complete(false)
			}
			cancel() // Preparation failed before Admit. Cleanup still has to replace routes.
			attempt.Complete(false)
			require.Eventually(t, func() bool {
				m := latest()
				return len(m["alpha"].Endpoints) == 0 && len(m["beta"].Endpoints) > 0
			}, time.Second, time.Millisecond)
			_, projected := cluster.vouch.Projected("alpha/api")
			require.False(t, projected)
			_, projected = cluster.vouch.Projected("beta/api")
			require.True(t, projected)
			for _, m := range latest() {
				require.False(t, m.Removed)
				require.Len(t, m.Workloads, 1)
			}

		})
	}
}

func TestRuntimeOverridesDoNotIssueOrRestoreCredentials(t *testing.T) {
	cluster, _ := admissionLauncher(t)
	service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: "alpha", Name: "api"})
	require.NoError(t, err)
	path := cluster.runtimeOverrides(service)[workloadTokenFileVariable]
	require.NotEmpty(t, path)
	require.NoFileExists(t, path)
	_, projected := cluster.vouch.Projected("alpha/api")
	require.False(t, projected)
	attempt, err := cluster.beginRuntimeStart(t.Context(), service)
	require.NoError(t, err)
	require.FileExists(t, path)
	attempt.Complete(false)
	require.Equal(t, path, cluster.runtimeOverrides(service)[workloadTokenFileVariable])
	_, projected = cluster.vouch.Projected("alpha/api")
	require.False(t, projected, "late environment preparation cannot restore refresh membership")
}

// A real Runner and the real launcher share a World here. The controlled peer
// observes signed delivery before the actual Runtime.Start RPC.
type admittedRuntimePeer struct {
	runtimev0.UnimplementedRuntimeServer
	composition   *admissionPeer
	starts, stops atomic.Int32
}

func (p *admittedRuntimePeer) Load(context.Context, *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	return &runtimev0.LoadResponse{Status: &runtimev0.LoadStatus{State: runtimev0.LoadStatus_READY}}, nil
}
func (p *admittedRuntimePeer) Init(context.Context, *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	return &runtimev0.InitResponse{Status: &runtimev0.InitStatus{State: runtimev0.InitStatus_READY}}, nil
}
func (p *admittedRuntimePeer) Start(_ context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	if len(p.composition.snapshots()) < 2 {
		return nil, fmt.Errorf("Start preceded selected declarations")
	}
	if _, err := os.Stat(req.GetOverrides()[workloadTokenFileVariable]); err != nil {
		return nil, err
	}
	p.starts.Add(1)
	return &runtimev0.StartResponse{Status: &runtimev0.StartStatus{State: runtimev0.StartStatus_STARTED}}, nil
}
func (p *admittedRuntimePeer) Stop(context.Context, *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	p.stops.Add(1)
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}

func TestRunnerFailureReplacesLauncherRoutes(t *testing.T) {
	cluster, composition := admissionLauncher(t)
	workspace := cluster.composed()
	module, err := workspace.LoadModuleFromName(t.Context(), "alpha")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, cluster.env, orchestration.RunMode)
	require.NoError(t, err)
	flow.WithTemporaryPorts(true)
	world := flow.World()
	world.BeginRuntimeStart, world.RuntimeOverrides = cluster.beginRuntimeStart, cluster.runtimeOverrides
	world.BeforeRuntimeStop = cluster.close
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	peer := &admittedRuntimePeer{composition: composition}
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, peer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	id, err := service.Identity()
	require.NoError(t, err)
	id.Workspace = workspace.Name
	instance := &coreservices.Instance{Workspace: workspace, Module: module, Service: service, Identity: id}
	instance.Runtime = &coreservices.RuntimeInstance{Instance: instance, Runtime: agentservices.NewRuntimeAgentClient(conn)}
	runner, err := orchestration.NewRunner(t.Context(), instance, world)
	require.NoError(t, err)
	runner.WithRuntimeContext(resources.RuntimeContextNative)
	_, err = runner.Load(t.Context())
	require.NoError(t, err)
	_, err = runner.Init(t.Context())
	require.NoError(t, err)
	_, projected := cluster.vouch.Projected("alpha/api")
	require.False(t, projected, "Init carries only a path")
	_, err = runner.Start(t.Context())
	require.NoError(t, err)
	latestReady := func() bool {
		ready := false
		for _, m := range composition.snapshots() {
			if m.Alias == "alpha" {
				ready = len(m.Endpoints) > 0
			}
		}
		return ready
	}
	require.Eventually(t, latestReady, time.Second, time.Millisecond)
	runner.WithOutputEnv(t.TempDir())
	_, err = runner.Start(t.Context())
	require.Error(t, err, "request preparation fails after stopping the previous process")
	require.EqualValues(t, 1, peer.starts.Load())
	require.EqualValues(t, 1, peer.stops.Load())
	require.Eventually(t, func() bool { return !latestReady() }, time.Second, time.Millisecond)
	_, projected = cluster.vouch.Projected("alpha/api")
	require.False(t, projected)
	for _, m := range composition.snapshots() {
		require.False(t, m.Removed)
	}
	require.NoError(t, flow.Stop())
	select {
	case <-cluster.stop:
	default:
		t.Fatal("Flow.Stop did not stop launcher")
	}
}

// A contract published after the hop's routes were frozen is never delivered
// behind them: startup admission and readiness delivery re-check the routing
// snapshot under the delivery lock and send nothing when it moved.
func TestAdmissionAndDeliveryRefuseAMovedRoutingSnapshot(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	flow, ok := cluster.flow.(*admissionFlow)
	require.True(t, ok)
	flow.routing = fmt.Errorf("published hop routes changed: stop and restart this Codefly run to adopt them")
	service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: "alpha", Name: "api"})
	require.NoError(t, err)
	_, err = admitRuntimeAttempt(t, cluster, service)
	require.ErrorContains(t, err, "deliver no policy table behind the hop's routes")
	require.ErrorContains(t, err, "stop and restart")
	cluster.delivered["alpha"] = &solutionhost.ModuleManifest{}
	cluster.running["alpha/api"] = true
	require.ErrorContains(t, cluster.deliverReadiness(t.Context(), "alpha", true), "stop and restart")
	require.Empty(t, peer.snapshots(), "nothing is sent behind moved routes")
}
