package orchestration

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The agent subprocess is this same test binary re-executed with
// realAgentEnv set: a real OS process serving the real Runtime gRPC contract
// over a real socket, so Init is exercised across the process boundary the
// CLI actually uses.
const (
	realAgentEnv     = "CODEFLY_TEST_REAL_AGENT"
	realAgentAddress = "AGENT_ADDRESS="

	// Agent behaviours, selected per subprocess.
	realAgentEcho      = "echo"      // accepts the proposal verbatim
	realAgentLegacy    = "legacy"    // returns no mappings at all (pre-contract)
	realAgentBind      = "bind"      // binds OS-assigned ports and reports them
	realAgentOmit      = "omit"      // drops one of the proposed endpoints
	realAgentForeign   = "foreign"   // claims another service's endpoint
	realAgentUnknown   = "unknown"   // invents an endpoint that was never proposed
	realAgentDuplicate = "duplicate" // returns two native views for one endpoint
)

// exposedConfigurationName marks the runtime configuration the agent exports on
// every Init. A rejected Init must not publish it.
const exposedConfigurationName = "runtime"

// realAgent answers Init with the mappings it intends to serve, shaped by the
// behaviour its process was started with.
type realAgent struct {
	runtimev0.UnimplementedRuntimeServer
	mode string
}

// boundMappings asks the kernel for a port per endpoint and keeps each listener
// open for the lifetime of the process, so the reported address is one a
// readiness probe can actually reach.
func boundMappings(proposed []*basev0.NetworkMapping) ([]*basev0.NetworkMapping, error) {
	accepted := make([]*basev0.NetworkMapping, 0, len(proposed))
	for _, mapping := range proposed {
		// The proposal's ports were bound then released, so the kernel can hand
		// one of them straight back. Keep asking until the port really differs,
		// or "the agent chose another port" is not what the test observes.
		port, err := bindPortOtherThan(proposedPorts(proposed))
		if err != nil {
			return nil, err
		}
		endpoint := mapping.GetEndpoint()
		accepted = append(accepted, &basev0.NetworkMapping{
			Endpoint: endpoint,
			Instances: []*basev0.NetworkInstance{
				network.Container(endpoint, port),
				network.Native(endpoint, port),
				network.PublicDefault(endpoint, port),
			},
		})
	}
	return accepted, nil
}

// bindPortOtherThan holds a kernel-assigned port for the life of the process,
// so the address it reports is one a readiness probe can actually reach.
func bindPortOtherThan(excluded map[uint16]bool) (uint16, error) {
	for attempt := 0; attempt < 50; attempt++ {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		if !excluded[port] {
			return port, nil
		}
		if err = listener.Close(); err != nil {
			return 0, err
		}
	}
	return 0, fmt.Errorf("no free port outside the proposed set")
}

func proposedPorts(proposed []*basev0.NetworkMapping) map[uint16]bool {
	ports := make(map[uint16]bool)
	for _, mapping := range proposed {
		for _, instance := range mapping.GetInstances() {
			ports[uint16(instance.GetPort())] = true
		}
	}
	return ports
}

func (agent *realAgent) Init(_ context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	mappings := req.GetProposedNetworkMappings()
	if agent.mode == realAgentLegacy {
		// A pre-contract agent answers READY without naming any mapping.
		return &runtimev0.InitResponse{Status: &runtimev0.InitStatus{State: runtimev0.InitStatus_READY}}, nil
	}
	if agent.mode != realAgentEcho {
		var err error
		if mappings, err = boundMappings(mappings); err != nil {
			return nil, err
		}
	}
	switch agent.mode {
	case realAgentOmit:
		mappings = mappings[:1]
	case realAgentForeign:
		mappings[0].Endpoint.Module = "billing"
		mappings[0].Endpoint.Service = "accounts"
	case realAgentUnknown:
		mappings[0].Endpoint.Name = "metrics"
	case realAgentDuplicate:
		mappings[0].Instances = append(mappings[0].Instances,
			network.Native(mappings[0].GetEndpoint(), 65000))
	}
	return &runtimev0.InitResponse{
		Status:          &runtimev0.InitStatus{State: runtimev0.InitStatus_READY},
		NetworkMappings: mappings,
		RuntimeConfigurations: []*basev0.Configuration{{
			Origin: "web/gateway",
			Infos: []*basev0.ConfigurationInformation{{
				Name: exposedConfigurationName,
				ConfigurationValues: []*basev0.ConfigurationValue{
					{Key: "session", Value: "exposed"},
				},
			}},
		}},
	}, nil
}

func (agent *realAgent) Stop(context.Context, *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}

// TestRealAgentProcess is the agent subprocess entry point, not a test: it only
// does anything when the parent re-executes this binary with realAgentEnv set.
func TestRealAgentProcess(t *testing.T) {
	mode := os.Getenv(realAgentEnv)
	if mode == "" {
		t.Skip("agent subprocess entry point")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, &realAgent{mode: mode})
	fmt.Printf("%s%s\n", realAgentAddress, listener.Addr().String())
	_ = os.Stdout.Sync()
	require.NoError(t, server.Serve(listener))
}

// startRealAgent re-executes this test binary as an agent process and returns a
// Runtime client connected to it over a real socket.
func startRealAgent(t *testing.T, mode string) *agentservices.RuntimeAgent {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestRealAgentProcess", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), realAgentEnv+"="+mode)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, realAgentAddress) {
				address <- strings.TrimPrefix(line, realAgentAddress)
				return
			}
		}
		close(address)
	}()

	var served string
	select {
	case served = <-address:
	case <-time.After(60 * time.Second):
		t.Fatal("agent process never reported its address")
	}
	require.NotEmpty(t, served, "agent process exited before serving")

	conn, err := grpc.NewClient(served, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return agentservices.NewRuntimeAgentClient(conn)
}

// gatewayRunner wires a Runner for testdata/module-layout's web/gateway service
// against a real configuration manager, state manager and port allocator.
func gatewayRunner(t *testing.T, runtime *agentservices.RuntimeAgent) (*Runner, *World) {
	t.Helper()
	ctx := context.Background()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, "testdata/module-layout")
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, "web")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, "gateway")
	require.NoError(t, err)
	identity, err := service.Identity()
	require.NoError(t, err)
	identity.Workspace = workspace.Name

	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	configurationManager, err := configurations.NewManager(ctx, workspace)
	require.NoError(t, err)
	require.NoError(t, configurationManager.Load(ctx, env))
	dependencies, err := architecture.NewServiceDependencies(ctx, workspace)
	require.NoError(t, err)
	sharedState, err := NewStateManager(ctx, configurationManager, dependencies)
	require.NoError(t, err)
	networkManager, err := network.NewRuntimeManager(ctx, configurationManager)
	require.NoError(t, err)
	// Kernel-assigned proposals keep the test off the deterministic ports a
	// developer's own `codefly run` may be holding.
	networkManager.WithTemporaryPorts()

	world := &World{
		Env:                  env,
		Workspace:            workspace,
		Dependencies:         dependencies,
		SharedState:          sharedState,
		LocalNetworkManager:  networkManager,
		ConfigurationManager: configurationManager,
	}

	instance := &coreservices.Instance{
		Workspace: workspace,
		Module:    module,
		Service:   service,
		Identity:  identity,
	}
	instance.Runtime = &coreservices.RuntimeInstance{Instance: instance, Runtime: runtime}

	runner, err := NewRunner(ctx, instance, world)
	require.NoError(t, err)
	runner.runtimeContext = resources.RuntimeContextNative
	endpoints, err := service.LoadEndpoints(ctx)
	require.NoError(t, err)
	runner.endpoints = endpoints
	require.NoError(t, sharedState.RecordEndpoints(ctx, identity, runner.endpoints))
	return runner, world
}

func nativeAddressFor(t *testing.T, mappings []*basev0.NetworkMapping, name string) string {
	t.Helper()
	for _, mapping := range mappings {
		if mapping.GetEndpoint().GetName() != name {
			continue
		}
		instance := resources.FilterNetworkInstance(context.Background(), mapping.Instances, resources.NewNativeNetworkAccess())
		require.NotNil(t, instance, "no native instance for %s", name)
		return instance.Address
	}
	t.Fatalf("no mapping for endpoint %s", name)
	return ""
}

// A real agent process that binds OS-assigned ports must have those ports —
// not the proposed ones — reach the runner, the shared state every consumer
// reads (dependents' Start requests, readiness, the dashboard) and the
// exported environment.
func TestInitPublishesTheAgentAcceptedPortsEverywhere(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentBind))
	outputEnv := filepath.Join(t.TempDir(), "runtime.env")
	runner.outputEnv = outputEnv

	proposed, err := world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env, world.Workspace, runner.instance.Identity, runner.endpoints, resources.NewRuntimeContextNative())
	require.NoError(t, err)
	proposedREST := nativeAddressFor(t, proposed, "rest")

	_, err = runner.Init(ctx)
	require.NoError(t, err)

	acceptedREST := nativeAddressFor(t, runner.networkMappings, "rest")
	require.NotEqual(t, proposedREST, acceptedREST, "agent accepted a different port but the runner kept the proposal")

	recorded, err := world.SharedState.GetNetworkMappings(ctx, runner.instance.Identity)
	require.NoError(t, err)
	require.Equal(t, acceptedREST, nativeAddressFor(t, recorded, "rest"))

	// The dependent view: what web/frontend would receive in its StartRequest.
	frontend, err := world.Dependencies.ServiceFromUnique("web/frontend")
	require.NoError(t, err)
	dependencyMappings, err := world.SharedState.GetDependenciesNetworkMappings(ctx, frontend)
	require.NoError(t, err)
	require.Equal(t, acceptedREST, nativeAddressFor(t, dependencyMappings, "rest"))

	// Readiness and the dashboard both answer from the shared state.
	flow := &Flow{SharedState: world.SharedState, begun: map[string]bool{"web/gateway": true}}
	address, err := flow.GetAddressForEndpoint(ctx, "web", "gateway", "rest")
	require.NoError(t, err)
	require.Equal(t, acceptedREST, address)
	require.True(t, flow.ServiceReachable("web/gateway"), "the accepted port is bound by the agent process")

	// Environment projection: Start writes the runner's own mappings out.
	require.NoError(t, AppendRuntimeEnvironmentToFile(ctx, outputEnv, &basev0.ServiceIdentity{
		Workspace: world.Workspace.Name, Module: "web", Name: "gateway",
	}, resources.NewRuntimeContextNative(), "", nil, runner.networkMappings))
	exported, err := os.ReadFile(outputEnv)
	require.NoError(t, err)
	require.Contains(t, string(exported), "CODEFLY__ENDPOINT__WEB__GATEWAY__REST__REST="+acceptedREST)

	// The rest of the Init publication still happens on the accepted path.
	shared, err := world.ConfigurationManager.GetSharedServiceConfiguration(ctx, "web/gateway")
	require.NoError(t, err)
	require.Len(t, shared, 1)
	require.Equal(t, exposedConfigurationName, shared[0].Infos[0].Name)
}

// An agent that echoes the proposal keeps working, and the native/container
// views it echoed stay distinct.
func TestInitAcceptsAnEchoingAgent(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentEcho))

	_, err := runner.Init(ctx)
	require.NoError(t, err)

	recorded, err := world.SharedState.GetNetworkMappings(ctx, runner.instance.Identity)
	require.NoError(t, err)
	for _, name := range []string{"rest", "grpc"} {
		mapping, err := resources.FindNetworkMapping(ctx, recorded, gatewayEndpointNamed(t, runner.endpoints, name))
		require.NoError(t, err)
		native := resources.FilterNetworkInstance(ctx, mapping.Instances, resources.NewNativeNetworkAccess())
		container := resources.FilterNetworkInstance(ctx, mapping.Instances, resources.NewContainerNetworkAccess())
		require.NotNil(t, native)
		require.NotNil(t, container)
		require.NotEqual(t, native.Hostname, container.Hostname)
		require.Equal(t, native.Port, container.Port)
	}
}

func gatewayEndpointNamed(t *testing.T, endpoints []*basev0.Endpoint, name string) *basev0.Endpoint {
	t.Helper()
	for _, endpoint := range endpoints {
		if endpoint.Name == name {
			return endpoint
		}
	}
	t.Fatalf("no endpoint named %s", name)
	return nil
}

// An agent response the CLI cannot validate fails Init outright, and nothing is
// published: no mappings for consumers to read, no exported runtime
// configuration, and the ports this runner reserved stay reserved to it and
// reusable by the retry that follows.
func TestInitRejectsInvalidAgentMappingsWithoutPublishing(t *testing.T) {
	for _, test := range []struct {
		mode    string
		message string
	}{
		{mode: realAgentOmit, message: "omit proposed endpoint"},
		{mode: realAgentForeign, message: "was never proposed to web/gateway"},
		{mode: realAgentUnknown, message: "was never proposed"},
		{mode: realAgentDuplicate, message: "two native instances"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			ctx := context.Background()
			runner, world := gatewayRunner(t, startRealAgent(t, test.mode))
			// Pin the proposal so a retry after the rejection is expected to
			// reuse the very ports this runner already reserved.
			reserved := map[string]uint16{
				"web/gateway/rest": freePort(t),
				"web/gateway/grpc": freePort(t),
			}
			world.LocalNetworkManager.WithPortOverrides(reserved)

			_, err := runner.Init(ctx)
			require.ErrorContains(t, err, test.message)
			require.Nil(t, runner.networkMappings)

			recorded, err := world.SharedState.GetNetworkMappings(ctx, runner.instance.Identity)
			require.NoError(t, err)
			require.Empty(t, recorded, "a rejected Init published mappings")

			exposed, err := world.ConfigurationManager.GetSharedServiceConfiguration(ctx, runner.instance.Identity.Unique())
			require.NoError(t, err)
			require.Empty(t, exposed, "a rejected Init exposed configuration")

			// No port-reservation leak: the endpoints are still allocatable to
			// this runner, on the same ports.
			retry, err := world.LocalNetworkManager.GenerateNetworkMappings(ctx, world.Env, world.Workspace, runner.instance.Identity, runner.endpoints, resources.NewRuntimeContextNative())
			require.NoError(t, err)
			for name, port := range map[string]uint16{"rest": reserved["web/gateway/rest"], "grpc": reserved["web/gateway/grpc"]} {
				require.Equal(t, fmt.Sprintf("localhost:%d", port), hostOfNativeInstance(t, retry, name))
			}
		})
	}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	require.NoError(t, listener.Close())
	return port
}

func hostOfNativeInstance(t *testing.T, mappings []*basev0.NetworkMapping, name string) string {
	t.Helper()
	for _, mapping := range mappings {
		if mapping.GetEndpoint().GetName() != name {
			continue
		}
		instance := resources.FilterNetworkInstance(context.Background(), mapping.Instances, resources.NewNativeNetworkAccess())
		require.NotNil(t, instance)
		return instance.Host
	}
	t.Fatalf("no mapping for endpoint %s", name)
	return ""
}

// A pre-contract agent that names no mappings has the proposal published on its
// behalf — and must stay reloadable. Populating runner.networkMappings for it
// (where the field used to be left nil) armed the first-start port guard, which
// Stop re-arms by clearing isStarted before every hot reload: the guard then
// fired against the service's own teardown.
func TestInitLegacyAgentAdoptsTheProposalAndStaysReloadable(t *testing.T) {
	ctx := context.Background()
	runner, world := gatewayRunner(t, startRealAgent(t, realAgentLegacy))

	_, err := runner.Init(ctx)
	require.NoError(t, err)
	adoptedREST := nativeAddressFor(t, runner.networkMappings, "rest")

	// The agent named nothing, so the runner and every consumer take the
	// address the CLI proposed — the same one, not two views of it.
	recorded, err := world.SharedState.GetNetworkMappings(ctx, runner.instance.Identity)
	require.NoError(t, err)
	require.Equal(t, adoptedREST, nativeAddressFor(t, recorded, "rest"))
	require.Len(t, runner.networkMappings, len(runner.endpoints))

	// The service starts, then a watch-driven reload stops it and re-enters
	// Init. Something still holding the port at that moment is this runner's own
	// teardown, not a ghost from a previous run.
	runner.markStarted()
	listener, err := net.Listen("tcp", strings.TrimPrefix(adoptedREST, "http://"))
	require.NoError(t, err)
	defer listener.Close()
	_, err = runner.stop(ctx)
	require.NoError(t, err)
	require.False(t, runner.isStarted.Load())

	_, err = runner.Init(ctx)
	require.NoError(t, err, "hot reload rejected the service's own port as stale")
}

// The guard still has to catch a real ghost: a process squatting the port
// before this runner has ever started the service.
func TestInitRejectsAStalePortBeforeTheFirstStart(t *testing.T) {
	ctx := context.Background()
	runner, _ := gatewayRunner(t, startRealAgent(t, realAgentEcho))

	// The guard probes the addresses a previous Init accepted, so run one first.
	_, err := runner.Init(ctx)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", strings.TrimPrefix(nativeAddressFor(t, runner.networkMappings, "rest"), "http://"))
	require.NoError(t, err)
	defer listener.Close()

	_, err = runner.Init(ctx)
	require.ErrorContains(t, err, "already in use")
}
