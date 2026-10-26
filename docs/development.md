# Contributing and Development Setup

> This is the loop for changing **the CLI itself**. For the loop that carries a
> change to an application in a workspace into a running environment — render,
> build, deploy, and how to tell a stale build from a wrong one — see
> [the development loop](development-loop.md).

## Prerequisites

- **Go 1.27.3+** (check with `go version`; `go.mod` is the source of truth — see [runbooks/bump-go-version.md](runbooks/bump-go-version.md))
- **Docker** (for container builds, agent processes, and infrastructure)
- **codefly binary** (the CLI itself, for bootstrapping)

CI selects its exact Go toolchain with `go-version-file: go.mod`, including the
bootstrap install action used by audit and control integration. Raising the root
`go` directive therefore raises those CI compiler pins together; `GOTOOLCHAIN=local`
then uses the compiler already installed by setup-go. The deployment-contract
workflow reads `contracts/deployment/go.mod` separately, so update that directive
with the root when adopting a security patch.

The Docker-backed quality fixtures use explicit `mirror.gcr.io/library/` image
references: Postgres 16 Alpine for ordered teardown, Go 1.27 Alpine for the
credential-free module build, Alpine 3.22 for generate recovery and bind-mount
proofs, Alpine 3.21 for the gateway container transport, and Registry 2 for
authenticated OCI acquisition. CI pre-pulls the same
references as their tests. Hosted runners share Docker Hub's anonymous pull
quota; exhausting it otherwise fails a prerequisite and cancels the other
quality jobs through matrix fail-fast. Explicit references prevent Docker's
daemon mirror fallback from returning to that exhausted quota. Image versions,
assertions and all CI and conformance requirements remain unchanged; a failed
pull still fails the gate.

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
codefly self build
```

### 4. Install locally

```bash
codefly self build
```

`go install` produces an operator-managed development build. It does not carry
the authenticated release metadata and `codefly self update` will not replace
it. It needs read access to `codefly-dev/authority`, a private repository whose
libraries the CLI requires, so it is not supported from outside the codefly-dev
organization; releases are the install path there:

```bash
GOPRIVATE=github.com/codefly-dev/authority \
  go install github.com/codefly-dev/cli/cmd/codefly@<version>
```

For a self-updating direct installation, use a signed release archive as
described in [CLI releases and updates](cli-updates.md).

CI downloads these private libraries with the existing organization `GH_PAT`
secret, also used by companion checkout and release publication. The token is
passed only to the module download/bootstrap step; the download helper scopes
Git authentication to `codefly-dev/authority` without persisting Git settings.
Build, lint and test steps consume the populated module cache. Missing access
fails the download; no gate is skipped. The CI reader-app action remains
available to callers that configure its app credentials, but CLI workflows do
not depend on unconfigured reader-app secrets.

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

## Detailed log ownership

The CLI's existing JSON file logger allocates one exclusive invocation directory
beneath the resolved Codefly home `logs` directory. Nested command contexts reuse
that sink and register it with Core's agent log forwarding only once. Context
completion does not close it: preparation, delegated service execution and final
shutdown belong to the same invocation. The process owns its final descriptor
lifetime. Explicitly closing a standalone logger is terminal and idempotent.

Date/size rotation exclusively creates segments inside the original invocation
directory. Log discovery selects the latest-started invocation, then its latest
segment; delayed writes or rotations in an older process cannot change that
selection. Legacy root-level daily files remain a fallback and explicit paths
remain readable. A follower keeps its opened descriptor across rotation and later
runs; it does not automatically switch to a different file.

This isolates the detailed JSON files of separate CLI processes. It does not
change caller-owned stdout/stderr capture, daemon/service logs, or establish
per-flow isolation inside a long-lived control process whose agent forwarding is
still global. Tests cover separate subprocesses, late writes and forced rotation,
repeated contexts through Core's actual log fanout, legacy discovery and terminal
close behavior.

## Composition withdrawal policy

The composition adapter sends a terminal signed withdrawal without an authority
sidecar. It does not read the removed module's method policies, ceilings or
solution roles: withdrawal must work after those source files disappear, and
live declarations contradict the tombstone. Ordinary delivery still signs and
pairs those policies with its carrier. Transport regressions cover both module
and solution withdrawals through the generated composition client. Successful `ApplyManifest` responses must acknowledge the sent generation.
The adapter retains the acknowledged request's digest, release and removal flag
for the run's connection to the host. Native withdrawal completion uses the
`Applied` callback and requires that exact identity; it does not contact a
private HTTP route or mint an observer credential for an interface host. A
host without the interface retains the legacy HTTP observer until that path
is deleted. This is synchronous response evidence, not a remote-state query
after a host restart. Live launcher
shutdown/restart remains unqualified by these transport tests.
