# Orchestration Engine

The orchestration engine is the core of the codefly CLI. It coordinates multi-service lifecycles by building a dependency DAG and executing actions through a policy-driven playbook.

## Architecture Overview

```
                    ┌──────────────────────────────────────────────────────┐
                    │                      Flow                           │
                    │                                                      │
                    │  ┌─────────┐   ┌──────────┐   ┌──────────────────┐  │
                    │  │  World  │   │ Playbook │   │  StateManager    │  │
                    │  │         │   │          │   │                  │  │
                    │  │ - Env   │   │ - Policy │   │ - Endpoints      │  │
                    │  │ - Mode  │   │ - DAG    │   │ - NetworkMappings│  │
                    │  │ - Deps  │   │ - Actions│   │ - Configurations │  │
                    │  └─────────┘   └──────────┘   └──────────────────┘  │
                    │                     │                                │
                    │         ┌───────────┴──────────┐                    │
                    │         │         Hub           │                    │
                    │         │  ┌───────┐ ┌───────┐ │                    │
                    │         │  │Manager│ │Manager│ │                    │
                    │         │  │(svc A)│ │(svc B)│ │                    │
                    │         │  └───┬───┘ └───┬───┘ │                    │
                    │         └─────┼──────────┼─────┘                    │
                    └───────────────┼──────────┼──────────────────────────┘
                                    │          │
                              ┌─────┴──┐  ┌───┴────┐
                              │ Runner │  │ Builder│
                              │        │  │        │
                              │ Agent  │  │ Agent  │
                              │ (gRPC) │  │ (gRPC) │
                              └────────┘  └────────┘
```

## Flow

The `Flow` is the top-level coordinator. It holds the workspace context, the service dependency graph, and orchestrates everything.

**Creation:**

```go
flow, err := orchestration.NewFlow(ctx, workspace, module, service, env, mode)
```

**Modes:**

| Mode | Description | Terminal Action |
|------|-------------|-----------------|
| `RunMode` | Start services locally | `RuntimeStart` |
| `TestMode` | Start services, then run tests | `RuntimeTest` |
| `BuildMode` | Build container images | `BuilderBuild` |
| `SyncMode` | Sync service configs | `BuilderSync` |
| `DeployMode` | Deploy to environment | `BuilderDeploy` |

**Lifecycle:**

1. `NewFlow()` -- creates the flow with dependency graph, network manager, configuration manager
2. `InitManagers()` -- creates a Manager (Runner + Builder) for each service in dependency order
3. `Load()` -- loads configurations, creates the Playbook with the appropriate Policy
4. `Start()` / `Test()` / `Build()` / `Deploy()` -- executes the playbook
5. `Stop()` / `Shutdown()` -- graceful teardown

**Key options:**

```go
flow.WithStandAlone(true)           // Don't start dependencies
flow.WithExcludeRoot(true)          // Start dependencies only, skip target
flow.WithRuntimeContext("nix")      // Set runtime context
flow.WithFixture("seed")            // Use named test fixture
flow.WithRemotes([]*Remote{...})    // Use remote services for some deps
flow.WithOutputEnv(".env")          // Export the origin service's runtime environment
flow.WithOutputEnvService("api/web") // Or select one dependency explicitly
flow.WithLoadOnly(true)             // Stop after Load phase
flow.WithInitOnly(true)             // Stop after Init phase
```

## World

The `World` holds shared state that all managers can access:

- `Env` -- target environment (local, staging, production)
- `Mode` -- current execution mode
- `Workspace` -- the workspace resource
- `Dependencies` -- the service dependency DAG (`architecture.ServiceDependencies`)
- `SharedState` -- the `StateManager` for cross-service state
- `LocalNetworkManager` -- assigns ports and network mappings for local runs
- `RemoteNetworkManager` -- manages port-forwarding for remote services
- `ConfigurationManager` -- loads and distributes service configurations

## Playbook

The `Playbook` is the execution engine. It receives actions, executes them through a policy, and produces follow-up actions.

```go
playbook, err := NewPlaybook(ctx, world)
playbook.WithPolicy(policy)
playbook.WithStoppingAfter(func(ctx, action) bool { ... })  // When to stop
playbook.WithIgnore(func(ctx, action) bool { ... })          // What to skip
```

**How it works:**

1. `Begin()` restricts the dependency graph to the target service, then seeds the initial action
2. `Work()` runs the main loop: receive action groups from a channel, execute each through the policy
3. The policy returns follow-up actions, which are sent back into the channel
4. Execution stops when the `StopAfterFunc` returns true, or the context is cancelled

**Action flow through Work():**

```
Receive ActionGroup
    │
    ├── Check PauseManager (service failing? skip)
    ├── Check IgnoreFunc (standalone mode? skip non-target)
    ├── Check previously executed (idempotency)
    │
    ├── Record action
    ├── policy.Execute(action) → next actions
    │
    ├── If pause returned → log warning, signal, continue
    ├── Add next actions to plan
    ├── Signal completion
    ├── Check StopAfterFunc → return if done
    │
    └── Send plan's actions back to channel
```

**Concurrency:** Actions at the same DAG level are grouped and sent together. The policy determines which can execute in parallel based on the dependency graph.

## Actions

Actions are the atomic units of work. Each has a `Type` and targets a specific `Service`.

**Runtime actions (for `run` and `test`):**

```
RuntimeBegin → RuntimeLoad → RuntimeInit → RuntimeStart → [RuntimeTest]
```

**Builder actions (for `build`, `sync`, `deploy`):**

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderBuild → [BuilderSync] → [BuilderDeploy]
```

**Action structure:**

```go
type Action struct {
    Type    ActionType  // e.g. RuntimeLoad, BuilderBuild
    Service string      // unique service identifier: "module/service"
    Failed  bool        // marks a failing action (triggers pause/retry)
    Round   int         // execution round for ordering
}
```

## Policies

Policies implement the `PlaybookPolicy` interface and define the state machine transitions.

```go
type PlaybookPolicy interface {
    ExecutorManager                                           // Maps action → executor function
    Execute(ctx context.Context, action Action) ([]Action, error)  // Run action, return next actions
    Restrict(ctx context.Context, service string) error       // Scope the dependency graph
}
```

### RuntimeStartPolicy

Used for `codefly run service`. State machine:

```
RuntimeBegin → RuntimeLoad (for all services in dependency order)
RuntimeLoad  → RuntimeInit (once load completes)
RuntimeInit  → RuntimeStart (once init completes)
RuntimeStart → done (service is running)
```

At each transition, the policy checks the dependency graph: if service B depends on service A, then B's `RuntimeLoad` won't fire until A's `RuntimeLoad` completes. Services at the same DAG level transition in parallel.

If an action fails (executor returns `Wait`), the action is marked as `Failing` and the PauseManager handles retry.

### RuntimeTestPolicy

Same as RuntimeStartPolicy, but adds `RuntimeTest` after `RuntimeStart` for the target service. The playbook stops after the test action completes.

### BuilderBuildPolicy (BuildPolicy)

Used for `codefly build service`. State machine:

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderBuild → done
```

### BuilderDeployPolicy (DeployPolicy)

Used for `codefly deploy service`:

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderBuild → BuilderDeploy → done
```

### SyncPolicy

Used for `codefly sync service`:

```
BuilderBegin → BuilderLoad → BuilderInit → BuilderSync → done
```

## StateManager

The `StateManager` tracks shared state across services during orchestration:

- **Endpoints** -- each service registers its endpoints after `Load`
- **NetworkMappings** -- each service registers its port assignments after `Init`
- **Configurations** -- runtime configurations exposed by services for their dependents

```go
type StateManager struct {
    endpoints       map[string][]*basev0.Endpoint
    networkMappings map[string][]*basev0.NetworkMapping
    configurationManager *providers.Manager
    dependencies         *architecture.ServiceDependencies
}
```

Key methods:

- `RecordEndpoints()` -- called after a service loads, stores its API endpoints
- `RecordNetworkMappings()` -- called after init, stores host:port assignments
- `GetDependenciesEndpoints()` -- returns endpoints of a service's direct dependencies
- `GetDependenciesNetworkMappings()` -- returns network addresses of dependencies
- `GetDependentConfigurationsFor()` -- returns configurations from dependency services

## Runner

The `Runner` manages a single service's runtime lifecycle via its agent (a gRPC plugin process).

**Lifecycle phases:**

| Phase | What happens | Agent gRPC call |
|-------|-------------|-----------------|
| **Load** | Agent starts, reports endpoints | `Runtime.Load()` |
| **Init** | Receives dependency info, network mappings, configurations. Returns its own mappings. | `Runtime.Init()` |
| **Start** | Receives dependency network addresses. Service starts listening. | `Runtime.Start()` |
| **Test** | Runs the service's test suite | `Runtime.Test()` |
| **Follow** | Polls agent for desired state changes (hot reload) | `Runtime.Information()` |
| **Stop** | Graceful shutdown (10s timeout, then SIGKILL) | `Runtime.Stop()` |
| **Destroy** | Clean up resources | `Runtime.Destroy()` |

**Hot reload:** The `Follow` phase polls the agent every second. If the agent signals `DesiredState.LOAD`, `INIT`, or `START`, the runner sends a callback action to the playbook, triggering a re-execution of that phase.

## Builder

The `Builder` manages build/deploy lifecycle via the agent's builder API.

**Lifecycle phases:**

| Phase | What happens | Agent gRPC call |
|-------|-------------|-----------------|
| **Load** | Builder starts, reports endpoints | `Builder.Load()` |
| **Init** | Receives dependency endpoints | `Builder.Init()` |
| **Sync** | Synchronizes service configuration | `Builder.Sync()` |
| **Build** | Builds container image (Docker context) | `Builder.Build()` |
| **Deploy** | Deploys to target environment | `Builder.Deploy()` |

## Service Dependency Resolution

The DAG is built from `service.codefly.yaml` files. Each service declares its dependencies:

```yaml
service_dependencies:
  - name: db
    module: backend
    endpoints:
      - postgres
```

The `architecture.ServiceDependencies` package resolves these into a topological order. For example:

```
frontend → api → db
                → cache
```

Results in execution order: `[db, cache]` (parallel) → `[api]` → `[frontend]`.

The `Restrict()` method scopes the graph to only the services needed for a given target.

## Error Handling

- **Agent load failure:** The `RunnerLoadManager` captures the error. The policy returns a `Failing` action, which the `PauseManager` handles. The service enters a wait-and-retry loop.
- **Context cancellation:** All gRPC calls check for `codes.Canceled` and return gracefully.
- **Partial failures:** Each manager stops independently during `Flow.Stop()`. Errors are collected via `go-multierror` and returned together.
- **Init failure:** If `Init` returns a non-READY status, the output manager marks the result as failing, triggering a pause and retry from the Load phase.
