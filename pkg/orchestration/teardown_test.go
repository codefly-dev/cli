package orchestration

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentservices "github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/architecture"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// edge builds the graph's own convention for "consumer depends on dependency":
// an edge pointing FROM the dependency TO the consumer.
func edge(dependency, consumer string) architecture.ServiceDependency {
	return architecture.ServiceDependency{
		From: architecture.Service{Unique: dependency},
		To:   architecture.Service{Unique: consumer},
	}
}

func layerNames(layers [][]teardownUnit) [][]string {
	out := make([][]string, 0, len(layers))
	for _, layer := range layers {
		out = append(out, layerUniques(layer))
	}
	return out
}

func managersFor(uniques ...string) []IManager {
	managers := make([]IManager, 0, len(uniques))
	for _, unique := range uniques {
		managers = append(managers, &recordingManager{unique: unique})
	}
	return managers
}

func TestTeardownLayersStopConsumersBeforeDependencies(t *testing.T) {
	tests := []struct {
		name     string
		managers []string
		edges    []architecture.ServiceDependency
		want     [][]string
	}{
		{
			name:     "no graph keeps a single layer",
			managers: []string{"app/api", "app/database"},
			want:     [][]string{{"app/api", "app/database"}},
		},
		{
			name:     "chain unwinds one service at a time",
			managers: []string{"app/database", "app/api", "web/frontend"},
			edges: []architecture.ServiceDependency{
				edge("app/database", "app/api"),
				edge("app/api", "web/frontend"),
			},
			want: [][]string{{"web/frontend"}, {"app/api"}, {"app/database"}},
		},
		{
			name:     "diamond stops the shared dependency once, after both branches",
			managers: []string{"app/database", "app/orders", "app/billing", "web/frontend"},
			edges: []architecture.ServiceDependency{
				edge("app/database", "app/orders"),
				edge("app/database", "app/billing"),
				edge("app/orders", "web/frontend"),
				edge("app/billing", "web/frontend"),
			},
			want: [][]string{{"web/frontend"}, {"app/orders", "app/billing"}, {"app/database"}},
		},
		{
			name:     "independent branches share a layer",
			managers: []string{"app/database", "app/api", "cache/redis", "cache/worker"},
			edges: []architecture.ServiceDependency{
				edge("app/database", "app/api"),
				edge("cache/redis", "cache/worker"),
			},
			want: [][]string{{"app/api", "cache/worker"}, {"app/database", "cache/redis"}},
		},
		{
			// Partial init: the consumer's manager was never created, so the
			// dependency has nothing to wait for and stops immediately.
			name:     "edges to absent managers are ignored",
			managers: []string{"app/database"},
			edges:    []architecture.ServiceDependency{edge("app/database", "app/api")},
			want:     [][]string{{"app/database"}},
		},
		{
			name:     "a cycle tears the remainder down together rather than leaking it",
			managers: []string{"app/a", "app/b", "app/leaf"},
			edges: []architecture.ServiceDependency{
				edge("app/a", "app/b"),
				edge("app/b", "app/a"),
			},
			want: [][]string{{"app/leaf"}, {"app/a", "app/b"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layers := teardownLayers(managersFor(test.managers...), test.edges)
			require.Equal(t, test.want, layerNames(layers))
		})
	}
}

// TestTeardownLayersIgnoreHubInsertionOrder proves the layering comes from the
// graph, not from the order the hub happens to hold its managers in: shuffling
// the hub changes which entries share a layer's slot, never which layer they
// land in.
func TestTeardownLayersIgnoreHubInsertionOrder(t *testing.T) {
	edges := []architecture.ServiceDependency{
		edge("app/database", "app/api"),
		edge("app/api", "web/frontend"),
	}
	forward := teardownLayers(managersFor("app/database", "app/api", "web/frontend"), edges)
	reversed := teardownLayers(managersFor("web/frontend", "app/api", "app/database"), edges)
	want := [][]string{{"web/frontend"}, {"app/api"}, {"app/database"}}
	require.Equal(t, want, layerNames(forward))
	require.Equal(t, want, layerNames(reversed))
}

// teardownAgent is a runtime agent served over a real loopback gRPC socket. The
// flow reaches it exactly as it reaches an out-of-process agent, so the times
// recorded here are the times a real service would observe.
type teardownAgent struct {
	runtimev0.UnimplementedRuntimeServer

	// drain runs inside Stop before the agent answers, standing in for a
	// service finishing its in-flight work. destroyDrain does the same for
	// Destroy.
	drain        func(context.Context) error
	destroyDrain func(context.Context) error

	mu           sync.Mutex
	stopCalls    int
	destroyCalls int
	stopBegan    time.Time
	stopEnded    time.Time
}

func (agent *teardownAgent) Stop(ctx context.Context, _ *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	agent.mu.Lock()
	agent.stopCalls++
	if agent.stopBegan.IsZero() {
		agent.stopBegan = time.Now()
	}
	agent.mu.Unlock()

	if agent.drain != nil {
		if err := agent.drain(ctx); err != nil {
			return nil, err
		}
	}

	agent.mu.Lock()
	agent.stopEnded = time.Now()
	agent.mu.Unlock()
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}

func (agent *teardownAgent) Destroy(ctx context.Context, _ *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	agent.mu.Lock()
	agent.destroyCalls++
	agent.mu.Unlock()
	if agent.destroyDrain != nil {
		if err := agent.destroyDrain(ctx); err != nil {
			return nil, err
		}
	}
	return &runtimev0.DestroyResponse{Status: &runtimev0.DestroyStatus{State: runtimev0.DestroyStatus_SUCCESS}}, nil
}

func (agent *teardownAgent) window() (began, ended time.Time) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.stopBegan, agent.stopEnded
}

func (agent *teardownAgent) calls() (stop, destroy int) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.stopCalls, agent.destroyCalls
}

// serveTeardownAgent runs agent on a loopback socket and returns the Manager
// the flow would hold for it.
func serveTeardownAgent(t *testing.T, unique string, agent *teardownAgent) *Manager {
	t.Helper()
	info, err := resources.ParseServiceWithOptionalModule(unique)
	require.NoError(t, err)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	runtimev0.RegisterRuntimeServer(server, agent)
	go func() { _ = server.Serve(listener) }()

	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = connection.Close()
		server.Stop()
	})

	instance := &coreservices.Instance{
		Identity: &resources.ServiceIdentity{Module: info.Module, Name: info.Name},
	}
	instance.Runtime = &coreservices.RuntimeInstance{
		Instance: instance,
		Runtime:  agentservices.NewRuntimeAgentClient(connection),
	}

	service := &resources.Service{Name: info.Name}
	service.WithModule(info.Module)
	runner := &Runner{instance: instance, stopped: make(chan struct{}, 1)}
	runner.isStarted.Store(true)
	return &Manager{service: service, Runner: runner}
}

// dependencyGraphFor lays a real workspace on disk whose services declare the
// given edges and loads it through the real architecture package, so these
// tests barrier on the same graph `codefly run` builds rather than on a
// hand-assembled stand-in.
func dependencyGraphFor(t *testing.T, edges []architecture.ServiceDependency) *architecture.ServiceDependencies {
	t.Helper()
	requires := map[string][]string{}
	var modules []string
	services := map[string][]string{}
	register := func(unique string) {
		if _, known := requires[unique]; known {
			return
		}
		requires[unique] = nil
		info, err := resources.ParseServiceWithOptionalModule(unique)
		require.NoError(t, err)
		if _, known := services[info.Module]; !known {
			modules = append(modules, info.Module)
		}
		services[info.Module] = append(services[info.Module], info.Name)
	}
	for _, dependency := range edges {
		register(dependency.From.Unique)
		register(dependency.To.Unique)
	}
	for _, dependency := range edges {
		requires[dependency.To.Unique] = append(requires[dependency.To.Unique], dependency.From.Unique)
	}

	root := t.TempDir()
	workspace := "name: teardown\nlayout: modules\nmodules:\n"
	for _, module := range modules {
		workspace += fmt.Sprintf("    - name: %s\n", module)
	}
	write := func(relative, content string) {
		path := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	write("workspace.codefly.yaml", workspace)
	for _, module := range modules {
		body := fmt.Sprintf("kind: module\nname: %s\nservices:\n", module)
		for _, service := range services[module] {
			body += fmt.Sprintf("    - name: %s\n", service)
		}
		write(filepath.Join("modules", module, "module.codefly.yaml"), body)
	}
	for unique, dependencies := range requires {
		info, err := resources.ParseServiceWithOptionalModule(unique)
		require.NoError(t, err)
		body := fmt.Sprintf("kind: service\nname: %s\nversion: 0.0.0\nmodule: %s\n"+
			"agent:\n    kind: codefly:service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n"+
			"endpoints:\n    - name: tcp\n", info.Name, info.Module)
		if len(dependencies) > 0 {
			body += "service-dependencies:\n"
			for _, dependency := range dependencies {
				dependencyInfo, err := resources.ParseServiceWithOptionalModule(dependency)
				require.NoError(t, err)
				body += fmt.Sprintf("    - name: %s\n      module: %s\n      endpoints:\n          - name: tcp\n",
					dependencyInfo.Name, dependencyInfo.Module)
			}
		}
		write(filepath.Join("modules", info.Module, "services", info.Name, "service.codefly.yaml"), body)
	}

	ctx := context.Background()
	loaded, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	dependencies, err := architecture.NewServiceDependencies(ctx, loaded)
	require.NoError(t, err)
	return dependencies
}

func teardownFlow(t *testing.T, managers []IManager, edges []architecture.ServiceDependency) *Flow {
	t.Helper()
	flow := &Flow{hub: &Hub{managers: managers}}
	if len(edges) > 0 {
		flow.world = &World{Mode: RunMode, Dependencies: dependencyGraphFor(t, edges)}
	}
	return flow
}

func entryFor(t *testing.T, receipt *TeardownReceipt, service string) TeardownEntry {
	t.Helper()
	require.NotNil(t, receipt)
	for _, entry := range receipt.Entries {
		if entry.Service == service {
			return entry
		}
	}
	t.Fatalf("no teardown entry for %s in %+v", service, receipt.Entries)
	return TeardownEntry{}
}

// TestFlowStopHoldsTheDatabaseUntilItsConsumerHasDrained is the acceptance
// case: the api agent holds its drain open and does its last write while the
// flow is already stopping. The database's Stop must not even begin until that
// drain has returned.
func TestFlowStopHoldsTheDatabaseUntilItsConsumerHasDrained(t *testing.T) {
	var writes []time.Time
	var writeMu sync.Mutex

	api := &teardownAgent{drain: func(context.Context) error {
		// A real service finishing an in-flight request: sleep long enough
		// that an unordered teardown would already have stopped the database.
		time.Sleep(150 * time.Millisecond)
		writeMu.Lock()
		writes = append(writes, time.Now())
		writeMu.Unlock()
		return nil
	}}
	database := &teardownAgent{}

	apiManager := serveTeardownAgent(t, "app/api", api)
	databaseManager := serveTeardownAgent(t, "app/database", database)

	flow := teardownFlow(t,
		[]IManager{databaseManager, apiManager},
		[]architecture.ServiceDependency{edge("app/database", "app/api")})

	require.NoError(t, flow.Stop())

	_, apiEnded := api.window()
	databaseBegan, _ := database.window()
	require.False(t, apiEnded.IsZero(), "api drain never completed")
	require.False(t, databaseBegan.IsZero(), "database was never stopped")
	require.True(t, databaseBegan.After(apiEnded),
		"database stop began at %s, before the api drain ended at %s", databaseBegan, apiEnded)

	writeMu.Lock()
	defer writeMu.Unlock()
	require.Len(t, writes, 1)
	require.True(t, databaseBegan.After(writes[0]),
		"database stop began at %s, before the api's final write at %s", databaseBegan, writes[0])

	receipt := flow.LastTeardown()
	require.Equal(t, 2, receipt.Layers)
	require.Equal(t, 0, entryFor(t, receipt, "app/api").Layer)
	require.Equal(t, 1, entryFor(t, receipt, "app/database").Layer)
	require.Equal(t, TeardownStopped, entryFor(t, receipt, "app/database").Outcome)
}

// TestFlowStopDrainsIndependentConsumersConcurrently covers the other half of
// the contract: the barrier must not serialize a diamond's independent
// branches, and the shared database is still stopped exactly once, after both.
func TestFlowStopDrainsIndependentConsumersConcurrently(t *testing.T) {
	// A rendezvous rather than a sleep: each branch must see the other inside
	// its drain before either may answer. Serialized teardown deadlocks here
	// and fails on the barrier's own budget instead of flaking on timing.
	arrived := make(chan struct{}, 2)
	both := make(chan struct{})
	var once sync.Once
	drain := func(ctx context.Context) error {
		arrived <- struct{}{}
		if len(arrived) == 2 {
			once.Do(func() { close(both) })
		}
		select {
		case <-both:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("independent consumers never drained concurrently: %w", ctx.Err())
		}
	}
	orders := &teardownAgent{drain: drain}
	billing := &teardownAgent{drain: drain}
	database := &teardownAgent{}

	flow := teardownFlow(t,
		[]IManager{
			serveTeardownAgent(t, "app/database", database),
			serveTeardownAgent(t, "app/orders", orders),
			serveTeardownAgent(t, "app/billing", billing),
		},
		[]architecture.ServiceDependency{
			edge("app/database", "app/orders"),
			edge("app/database", "app/billing"),
		})

	require.NoError(t, flow.Stop())

	_, ordersEnded := orders.window()
	_, billingEnded := billing.window()
	databaseBegan, _ := database.window()

	require.True(t, databaseBegan.After(ordersEnded) && databaseBegan.After(billingEnded),
		"database stop at %s did not wait for both branches (orders %s, billing %s)",
		databaseBegan, ordersEnded, billingEnded)

	stops, _ := database.calls()
	require.Equal(t, 1, stops, "the shared dependency must be stopped once")
}

// TestFlowStopForcesAStuckConsumerAndStillStopsItsDependency proves the budget
// is real: a consumer that never drains is recorded as forced, and the flow
// still moves on and stops the dependency it was holding.
func TestFlowStopForcesAStuckConsumerAndStillStopsItsDependency(t *testing.T) {
	api := &teardownAgent{drain: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	database := &teardownAgent{}

	flow := teardownFlow(t,
		[]IManager{
			serveTeardownAgent(t, "app/database", database),
			serveTeardownAgent(t, "app/api", api),
		},
		[]architecture.ServiceDependency{edge("app/database", "app/api")})
	flow.teardownPhaseBudget = 250 * time.Millisecond

	started := time.Now()
	err := flow.Stop()
	elapsed := time.Since(started)

	require.Error(t, err)
	require.ErrorContains(t, err, "app/api")
	require.Less(t, elapsed, 2*time.Second, "teardown ran past its budget")

	receipt := flow.LastTeardown()
	require.Equal(t, TeardownTimedOut, entryFor(t, receipt, "app/api").Outcome, "err=%v", entryFor(t, receipt, "app/api").Err)
	require.Equal(t, TeardownStopped, entryFor(t, receipt, "app/database").Outcome)

	databaseBegan, _ := database.window()
	require.False(t, databaseBegan.IsZero(), "the dependency was never stopped")
	require.GreaterOrEqual(t, databaseBegan.Sub(started), flow.teardownPhaseBudget,
		"the dependency was stopped before the stuck consumer's budget expired")
}

// TestFlowStopRecordsAFailedDrainSeparatelyFromAForcedOne keeps the two
// failure kinds distinguishable in the receipt: an agent that refuses to stop
// answered, a forced one did not.
func TestFlowStopRecordsAFailedDrainSeparatelyFromAForcedOne(t *testing.T) {
	refusing := &teardownAgent{drain: func(context.Context) error {
		return fmt.Errorf("still holding a transaction")
	}}
	flow := teardownFlow(t, []IManager{serveTeardownAgent(t, "app/api", refusing)}, nil)

	err := flow.Stop()
	require.ErrorContains(t, err, "still holding a transaction")

	entry := entryFor(t, flow.LastTeardown(), "app/api")
	require.Equal(t, TeardownFailed, entry.Outcome)
	require.Error(t, entry.Err)
}

// TestFlowStopAndShutdownAreRepeatable covers the repeated-teardown path:
// Stop/Shutdown may be called again (a failed run stops, then the caller
// shuts the flow down) and must keep honoring the ordering each time.
func TestFlowStopAndShutdownAreRepeatable(t *testing.T) {
	api := &teardownAgent{}
	database := &teardownAgent{}
	flow := teardownFlow(t,
		[]IManager{
			serveTeardownAgent(t, "app/database", database),
			serveTeardownAgent(t, "app/api", api),
		},
		[]architecture.ServiceDependency{edge("app/database", "app/api")})

	require.NoError(t, flow.Stop())
	require.NoError(t, flow.Stop())
	require.NoError(t, flow.Shutdown())
	require.NoError(t, flow.Shutdown())

	apiStops, apiDestroys := api.calls()
	databaseStops, databaseDestroys := database.calls()
	require.Equal(t, 2, apiStops)
	require.Equal(t, 2, databaseStops)
	require.Equal(t, 2, apiDestroys)
	require.Equal(t, 2, databaseDestroys)

	receipt := flow.LastTeardown()
	require.Equal(t, "Shutdown", receipt.Operation)
	require.Equal(t, 0, entryFor(t, receipt, "app/api").Layer)
	require.Equal(t, 1, entryFor(t, receipt, "app/database").Layer)
}

// TestFlowShutdownDestroysConsumersBeforeDependencies applies the same barrier
// to Destroy, which RunnerDoDestroy delegates to.
func TestFlowShutdownDestroysConsumersBeforeDependencies(t *testing.T) {
	var destroyed []string
	var mu sync.Mutex
	// The consumer is the slow one, so an unordered fan-out would record the
	// dependency first.
	record := func(unique string, hold time.Duration) func(context.Context) error {
		return func(context.Context) error {
			time.Sleep(hold)
			mu.Lock()
			destroyed = append(destroyed, unique)
			mu.Unlock()
			return nil
		}
	}
	flow := teardownFlow(t,
		[]IManager{
			serveTeardownAgent(t, "app/database", &teardownAgent{destroyDrain: record("app/database", 0)}),
			serveTeardownAgent(t, "app/api", &teardownAgent{destroyDrain: record("app/api", 150*time.Millisecond)}),
		},
		[]architecture.ServiceDependency{edge("app/database", "app/api")})

	require.NoError(t, flow.Shutdown())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"app/api", "app/database"}, destroyed)
}
