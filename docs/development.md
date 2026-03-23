# Contributing and Development Setup

## Prerequisites

- **Go 1.25+** (check with `go version`)
- **Docker** (for container builds, agent processes, and infrastructure)
- **codefly binary** (the CLI itself, for bootstrapping)

## Repository Structure

```
cli/
├── cmd/                    # Cobra command definitions
│   ├── root.go             # Root command + global flags
│   ├── run.go              # run → run/service.go, run/job.go
│   ├── build.go            # build → build/service.go
│   ├── test.go             # test → test/service.go
│   ├── deploy.go           # deploy → deploy/service.go, deploy/init.go
│   ├── add.go              # add → add/module.go, add/service.go, ...
│   ├── delete.go           # delete → delete/module.go, delete/service.go
│   ├── update.go           # update → update/service.go, update/workspace.go
│   ├── sync.go             # sync → sync/service.go, sync/library_dependencies.go
│   ├── list.go             # list → list/project.go, list/application.go, ...
│   ├── initialize.go       # init → initialize/workspace.go
│   ├── install.go           # install → install/library.go
│   ├── agent.go            # agent → agents/info.go, agents/build.go, agents/generate.go
│   ├── generate.go         # generate → generate/grpc.go, generate/proto.go, generate/swagger.go
│   ├── daemon.go           # daemon start/stop/restart/status/logs/monitor/gateway
│   ├── mcp.go              # mcp serve/tools
│   ├── server.go           # server (web companion)
│   ├── expose.go           # expose → expose/service.go
│   ├── ci.go               # ci → ci/test.go, ci/build.go, ci/deploy.go, ci/push.go
│   ├── login.go            # login
│   ├── version.go          # version
│   ├── clear.go            # clear
│   ├── replay.go           # replay
│   ├── import.go           # import
│   └── common/             # Shared utilities (context, workspace loading, logo)
│
├── pkg/                    # Core packages
│   ├── orchestration/      # Flow, Playbook, Runner, Builder, Policies, StateManager
│   ├── daemon/             # Background process management + monitor
│   ├── mcp/                # MCP server, tools, resources, protocol types
│   ├── gateway/            # Mind Gateway gRPC server
│   ├── builder/            # Docker build context utilities
│   ├── cli/                # CLI utilities (logging, errors, TUI communication)
│   ├── deployments/        # Deployment manager interface
│   ├── generators/         # Code generation utilities
│   ├── imports/            # Application import logic
│   ├── observability/      # OTEL integration
│   ├── platform/           # Platform login/token management
│   ├── types/              # Shared types
│   └── web/                # Web companion server
│
├── main.go                 # Entry point
├── go.mod                  # Module: github.com/codefly-dev/cli
├── go.work                 # Workspace: cli + core + wool + wool/otel
├── Makefile                # Build targets
├── scripts/                # Build and publish scripts
├── docs/                   # Documentation (you are here)
└── test/                   # Test fixtures and data
```

## Local Development Setup

### 1. Clone the repositories

The CLI depends on `core` and `wool` packages. For local development, use Go workspaces:

```bash
mkdir -p ~/Development/codefly.dev
cd ~/Development/codefly.dev

git clone https://github.com/codefly-dev/cli.git
git clone https://github.com/codefly-dev/core.git
git clone https://github.com/codefly-dev/wool.git
```

### 2. Verify go.work

The `go.work` file links local packages:

```
go 1.25

use (
    .
    ../core
    ../wool
    ../wool/otel
)
```

This means changes to `core` or `wool` are immediately reflected in the CLI without publishing.

### 3. Build

```bash
cd cli
go build ./...
```

### 4. Install locally

```bash
go install .
# or
go build -o $(go env GOPATH)/bin/codefly .
```

### 5. Run tests

```bash
go test ./...
```

### 6. Check coverage

```bash
make check-coverage
```

---

## Adding a New Command

### 1. Create the command file

For a top-level command, create `cmd/mycommand.go`:

```go
package cmd

import "github.com/spf13/cobra"

var MyCommandCmd = &cobra.Command{
    Use:   "mycommand",
    Short: "Description of mycommand",
    Run: func(cmd *cobra.Command, args []string) {
        // Implementation
    },
}
```

For a subcommand (e.g., `codefly run job`), create `cmd/run/job.go`:

```go
package run

import "github.com/spf13/cobra"

var JobCmd = &cobra.Command{
    Use:   "job [name]",
    Short: "Run a job",
    Run: func(cmd *cobra.Command, args []string) {
        // Implementation
    },
}
```

### 2. Register in the parent command

In `cmd/root.go` (for top-level) or the parent command file:

```go
func init() {
    RootCmd.AddCommand(MyCommandCmd)
}
```

Or for subcommands, in the parent (e.g., `cmd/run.go`):

```go
func init() {
    RunCmd.AddCommand(run.JobCmd)
}
```

### 3. Common patterns

**Loading workspace context:**

```go
ctx, done := common.NewContext()
defer done()

workspace, module, service := common.LoadRequired(ctx, args)
```

**Signal handling:**

```go
ctx, stop := signal.NotifyContext(ctx, os.Interrupt, os.Kill)
defer stop()
```

**Agent cleanup:**

```go
cli.RegisterCleanup(services.ClearAgents)
```

**Error handling:**

```go
cli.ExitOnError(err, "Cannot do the thing")
```

---

## Key Packages to Understand

### `pkg/orchestration/`

The heart of the CLI. Start with:

1. `action.go` -- Action types and the ActionManager channel system
2. `flow.go` -- Flow creation and lifecycle
3. `playbook.go` -- The execution loop
4. `runner.go` -- Service runtime lifecycle (Load/Init/Start/Test/Stop)
5. `builder.go` -- Build lifecycle (Load/Init/Build/Deploy)
6. `runtime_start_policy.go` -- State machine transitions for `run`

### `pkg/daemon/`

Two files:

- `daemon.go` -- Process start/stop/status, PID file management
- `monitor.go` -- Process health monitoring, CPU/memory thresholds

### `pkg/mcp/`

- `protocol.go` -- JSON-RPC 2.0 and MCP type definitions
- `server.go` -- MCP server with stdio transport
- `tools.go` -- Workspace-level tool registrations
- `service_tools.go` -- Per-service tools (read/write/build/test)
- `symbol_tools.go` -- LSP-backed symbol intelligence tools
- `resources.go` -- Resource registrations (workspace/module/service configs)

### `pkg/gateway/`

Mind Gateway gRPC server for the Mind engineering platform.

---

## Dependencies

The CLI depends on two sibling packages:

### `core` (github.com/codefly-dev/core)

- Resource model: `resources.Workspace`, `resources.Module`, `resources.Service`
- Architecture: `architecture.ServiceDependencies` (DAG)
- Agent lifecycle: `services.Instance`, `services.Runtime`, `services.Builder`
- Network management: `network.RuntimeManager`, `network.RemoteManager`
- Configuration management: `configurations.Manager`
- gRPC proto definitions: `codefly/base/v0`, `codefly/services/runtime/v0`, etc.

### `wool` (github.com/codefly-dev/wool)

Structured logging library with:
- Scoped loggers (`wool.Get(ctx).In("scope")`)
- Typed fields (`wool.Field("key", value)`)
- Error wrapping (`w.Wrapf(err, "context")`)
- Log levels: TRACE, DEBUG, INFO, WARN, ERROR, FOCUS

---

## Testing

### Unit tests

```bash
go test ./pkg/orchestration/...
go test ./pkg/mcp/...
go test ./pkg/daemon/...
```

### MCP server tests

The MCP server has integration tests that use `ServeIO()` with in-memory readers/writers:

```bash
go test ./pkg/mcp/... -v
```

### Orchestration tests

Playbook sync and policy tests:

```bash
go test ./pkg/orchestration/... -v
```

---

## Build and Release

### Build scripts

```bash
ls scripts/build/     # Build scripts
ls scripts/publish/   # Publish/release scripts
```

### Makefile targets

```bash
make check-coverage   # Run tests with coverage report
```

---

## Code Style

- **Cobra** for all CLI commands
- **wool** for all logging (never `fmt.Printf` or `log.Printf` in packages)
- **gRPC** for all agent communication (never shell exec to agents)
- **Context propagation** through all functions
- **Signal handling** in all long-running commands
- **Error wrapping** with `w.Wrapf(err, "context message")`
