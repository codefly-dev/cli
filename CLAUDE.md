# CLAUDE.md — codefly/cli

## Module & Repository

- **Module:** `github.com/codefly-dev/cli`
- **Repository:** `https://github.com/codefly-dev/cli`
- **Language:** Go 1.25

The CLI is the primary interface for codefly. It orchestrates agent lifecycles, manages the daemon, runs services with their dependency graphs, and exposes an MCP server for AI tool integration.

## Architecture Overview

```
User runs: codefly run service
    │
    ├── Load workspace/module/service from YAML
    ├── Build dependency graph (architecture pkg in core)
    ├── Create Flow → Playbook → Policy
    ├── For each service in dependency order:
    │     ├── Spawn agent process (gRPC server)
    │     ├── Load → Init → Start (with network mappings + configs)
    │     └── Monitor health + logs
    ├── Inject connection strings as env vars
    └── TUI or headless mode
```

## Command Structure (Cobra-based)

Root command: `codefly`

| Command | Description |
|---------|-------------|
| `run service` | Run a service with all dependencies |
| `run job` | Run a one-shot job |
| `build` | Build services (Docker images, binaries) |
| `deploy` | Deploy to target environment |
| `test` | Run service tests |
| `add` | Add workspace/module/service/endpoint |
| `delete` | Delete resources |
| `initialize` | Initialize a new workspace |
| `generate` | Generate code (proto, templates) |
| `install` | Install agents |
| `update` | Update agents |
| `list` | List resources |
| `import` | Import existing projects |
| `sync` | Sync service configuration |
| `expose` | Expose endpoints |
| `open` | Open service in browser |
| `login` | Authenticate with codefly platform |
| `daemon` | Manage background daemon (start/stop/logs/monitor) |
| `mcp` | Start MCP server |
| `server` | Start internal gRPC server |
| `agents` | Agent management |
| `ci` | CI/CD operations |
| `replay` | Replay recorded sessions |
| `version` | Print version |

Commands are in `cmd/` with subcommands in `cmd/{command}/`.

## Package Hierarchy (pkg/)

### Orchestration — The Core Engine
- **pkg/orchestration/** — The heart of the CLI. Manages the full service lifecycle.
  - `Flow` — Top-level orchestrator. Holds the workspace, origin service, playbook, policy, state manager, and configuration manager. Entry point for `run service`.
  - `Playbook` — Ordered execution plan. Contains `Runner` instances for each service in dependency order.
  - `Runner` — Wraps a single agent instance. Manages Load → Init → Start lifecycle via gRPC. Collects endpoints, network mappings, and output properties.
  - `PlaybookPolicy` — Controls execution order and conditions (e.g., `RuntimeStartPolicy` gates on dependencies being ready).
  - `StateManager` — Tracks shared state across all runners (which services are loaded, initialized, started).
  - `Builder` / `BuildExecutor` — Build-phase orchestration for `codefly build` and `codefly deploy`.
  - `Signaller` — Pause/resume management for hot-reload.
  - `Hub` — Event distribution between runners.

### Daemon
- **pkg/daemon/** — Background process management.
  - Re-execs the CLI binary with internal flags, detaches from terminal.
  - PID file and logs under `~/.codefly/`.
  - `daemon.go` — Start/Stop/WritePID/ReadPID/IsRunning.
  - `monitor.go` — Health monitoring.

### MCP Server
- **pkg/mcp/** — Model Context Protocol server for AI tool integration.
  - `server.go` — MCP server lifecycle.
  - `tools.go` — Tool registry.
  - `service_tools.go` — Service-level tools (run, stop, status, logs).
  - `symbol_tools.go` — Code symbol tools (list symbols, get symbol details).
  - `resources.go` — MCP resource exposure.
  - `protocol.go` — MCP protocol implementation.

### Platform & Gateway
- **pkg/platform/** — Platform-level operations (cloud deployment targets).
- **pkg/gateway/** — API gateway configuration and management.

### Supporting
- **pkg/cli/** — CLI utilities: initialization, cleanup registration, error handling, output formatting.
- **pkg/builder/** — Build coordination utilities.
- **pkg/deployments/** — Deployment target management.
- **pkg/generators/** — Code generation orchestration.
- **pkg/imports/** — Project import logic.
- **pkg/observability/** — Tracing and metrics setup.
- **pkg/types/** — Shared type definitions.
- **pkg/web/** — Embedded web server for headless/programmatic mode.

## The "codefly run service" Flow — Step by Step

1. **Load resources:** Parse `workspace.codefly.yaml` → `module.codefly.yaml` → `service.codefly.yaml`.
2. **Build dependency graph:** Use `architecture` package from core to resolve all transitive dependencies.
3. **Create Flow:** `orchestration.NewFlow(workspace, module, service)`.
4. **Create Playbook:** Ordered list of `Runner` instances, one per service in the graph.
5. **Apply Policy:** `RuntimeStartPolicy` ensures dependencies start before dependents.
6. **For each Runner (in order):**
   - **Spawn agent:** Find agent binary, start as gRPC server process.
   - **Load:** Send `LoadRequest` with identity, settings, environment.
   - **Init:** Send `InitRequest` with dependency endpoints and network mappings.
   - **Start:** Send `StartRequest`. Agent starts the actual service process.
7. **Inject configs:** Connection strings set as environment variables (`CODEFLY__SERVICE_...`).
8. **Monitor:** Health checks, log collection, hot-reload signals.
9. **TUI or headless:** Either interactive terminal UI or silent mode with web server.

## Build & Test

```bash
# Build
go build -o codefly .
./scripts/dev/install.sh          # Build and install to PATH

# Test
go test ./...                      # All tests
go test ./pkg/orchestration/ -v    # Orchestration tests
go test ./pkg/mcp/ -v              # MCP server tests

# Coverage
make check-coverage

# go.work
# The repo uses go.work to link to local core. Check go.work for replace directives.
```

## Key Patterns

### Agent Communication is ALWAYS gRPC
The CLI never calls agent code directly. Every interaction goes through gRPC:
- `runtimev0.RuntimeClient` — Load, Init, Start, Stop, Destroy, Test, Information
- `builderv0.BuilderClient` — Load, Init, Create, Update, Sync, Build, Deploy
- `agentv0.AgentClient` — Communicate (interactive Q&A)
- `codev0.CodeClient` — ListSymbols, GetSymbol

### State Management
`StateManager` in orchestration tracks:
- Which runners have completed Load
- Which runners have completed Init
- Which runners are Started and healthy
- Policies gate transitions: a runner cannot Start until all its dependencies are Started.

### Network Flow
1. Flow creates a `RuntimeManager` from core/network.
2. Each runner gets network mappings: its own endpoints + all dependency endpoints.
3. Mappings include access type (Native/Container/Public) based on context.
4. Connection strings derived from mappings are injected as env vars.

### Configuration Flow
`ConfigurationManager` from core/configurations:
1. Collects configs from all started services.
2. Routes configs to dependent services based on declared dependencies.
3. Configs flow as environment variables, not files.

## Key Environment Variables

- `CODEFLY_DEBUG` — Enable debug logging
- `CODEFLY_SILENT` — Suppress output
- `CODEFLY_WORKSPACE` — Override workspace path
- `CODEFLY_HOME` — Override codefly home directory (default: `~/.codefly`)

## Important Rules

- **CLI to Agent communication is ALWAYS gRPC.** Never import agent code directly. Never call agent functions. The agent runs as a separate process.
- **NEVER mock.** Tests use real agent processes and real infrastructure where possible.
- **The orchestration package is the most critical code.** Changes here affect every `codefly run` invocation. Test thoroughly.
- **go.work and replace directives** point to local paths. When working on CLI + core together, ensure both are checked out at the expected relative paths.
- **Daemon state lives in `~/.codefly/`.** PID file, logs, and agent binaries are all under this directory.
- **MCP server exposes codefly capabilities to AI agents.** When adding new CLI features, consider whether they should also be exposed as MCP tools.
