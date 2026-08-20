# AGENTS.md — codefly/cli

> **This file is the canonical instructions for AI coding agents and human contributors.**
> `CLAUDE.md` (and any other tool-specific file) is a thin forwarder that points here.
> The same layout is used in every codefly repo (`core`, `cli`, agents, …): one canonical
> `AGENTS.md` at the root, task procedures under `docs/runbooks/`, deep references under `docs/`.

## Module & Repository

- **Module:** `github.com/codefly-dev/cli`
- **Repository:** `https://github.com/codefly-dev/cli`
- **Language:** Go 1.26 (`go.mod` is the source of truth — see [Bump the Go version](docs/runbooks/bump-go-version.md))

The CLI is the primary interface for codefly. It orchestrates agent lifecycles, manages the
daemon, runs services with their dependency graphs, and exposes an MCP server for AI tool
integration.

---

## How-To Index (start here)

Task-oriented procedures. Each links to a full runbook under `docs/runbooks/`. Add a new
runbook whenever you do a multi-step operational task a second time.

### Toolchain & dependencies
- **Bump the Go version** (core + cli + agents + CI images) → [docs/runbooks/bump-go-version.md](docs/runbooks/bump-go-version.md)
- **Go standards** (formatting, linting) → [docs/go.md](docs/go.md)

### Shipping
- **Cut a release** (tag → GoReleaser → Homebrew cask) → [docs/runbooks/cut-a-release.md](docs/runbooks/cut-a-release.md)
- **Release the whole agent fleet** (re-pin every agent on a new core, publish in dependency order) → [docs/runbooks/release-the-fleet.md](docs/runbooks/release-the-fleet.md)
- **How releases & self-update work** → [docs/cli-updates.md](docs/cli-updates.md)

### Extending the CLI
- **Add a new command** (Cobra wiring, help, MCP exposure) → [docs/runbooks/add-a-command.md](docs/runbooks/add-a-command.md)
- **Rebuild the CLI and agents from local source** → [docs/runbooks/update-agents.md](docs/runbooks/update-agents.md)

### Reference (deep dives, not step-by-step)
- **All CLI commands, by category** → [docs/commands.md](docs/commands.md)
- **Orchestration engine** → [docs/orchestration.md](docs/orchestration.md)
- **Agent CI & port isolation** (why sequential agent CI must not share a host port) → [docs/agent-ci-port-isolation.md](docs/agent-ci-port-isolation.md)
- **Daemon** → [docs/daemon.md](docs/daemon.md)
- **MCP server** → [docs/mcp-server.md](docs/mcp-server.md)
- **Contributing / dev setup** → [docs/development.md](docs/development.md)
- **Design docs** → [docs/design/](docs/design/)

---

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

Entry point: `main.go` at the root re-exports `cmd/codefly/main.go`; GoReleaser builds
`./cmd/codefly`. Cobra command tree is rooted at `cmd/root.go` (`RootCmd`).

### Command groups (`cmd/`)

Top-level commands are registered in `cmd/root.go`'s `init()` via `RootCmd.AddCommand(...)`.
Each command with subcommands has a `cmd/<name>/` package. See
[docs/commands.md](docs/commands.md) for the full user-facing reference and
[docs/runbooks/add-a-command.md](docs/runbooks/add-a-command.md) to add one.

| Command | Description |
|---------|-------------|
| `run service` / `run job` | Run a service (with deps) or a one-shot job |
| `build` / `deploy` / `test` | Build images/binaries, deploy, run tests |
| `add` / `delete` / `initialize` | Create/remove workspace, module, service, endpoint |
| `generate` | Generate code (proto, grpc, swagger, templates) |
| `install` / `update` / `upgrade` / `agents` | Agent + library management |
| `list` / `show` / `status` / `explain` | Inspect resources and get help |
| `import` / `sync` / `expose` / `open` | Import projects, sync config, expose/open endpoints |
| `login` | Authenticate with the codefly platform |
| `daemon` | Background daemon (start/stop/logs/monitor/gateway) |
| `mcp` / `server` | MCP server / web companion server |
| `self` | Build/pull/update the CLI itself (and agents) from source |
| `ci` / `audit` / `sbom` / `verify` / `package` | CI/CD and supply-chain operations |
| `replay` / `version` / `clear` | Replay sessions, print version, clear state |

### Package hierarchy (`pkg/`)

- **pkg/orchestration/** — The heart of the CLI. `Flow` → `Playbook` → `Runner` manage the
  full Load → Init → Start service lifecycle over gRPC. `StateManager` tracks shared state;
  `PlaybookPolicy` gates transitions; `Builder`/`BuildExecutor` handle build/deploy;
  `Signaller` handles pause/resume; `Hub` distributes events. See [docs/orchestration.md](docs/orchestration.md).
- **pkg/daemon/** — Background process management. Re-execs the CLI with internal flags; PID
  file + logs under `~/.codefly/`. See [docs/daemon.md](docs/daemon.md).
- **pkg/mcp/** — Model Context Protocol server exposing codefly to AI tools. See [docs/mcp-server.md](docs/mcp-server.md).
- **pkg/cliupdate/** — Version stamping (`version`/`commit`/`buildDate` set via ldflags at
  release), self-update, and the release-signing certificate.
- **pkg/platform/** / **pkg/gateway/** — Platform ops and Mind Gateway gRPC server.
- **pkg/cli/**, **pkg/builder/**, **pkg/deployments/**, **pkg/generators/**, **pkg/imports/**,
  **pkg/observability/**, **pkg/types/**, **pkg/web/** — supporting packages.

---

## Build & Test

```bash
go build -o codefly ./cmd/codefly   # Build the CLI
codefly self build                  # Build from source and install over the running binary
codefly self build --with-agents    # ...also rebuild every canonical agent repo

go test ./...                       # All tests
go test ./pkg/orchestration/ -v     # Orchestration tests
make lint                           # golangci-lint (reproduces CI)
make check-coverage                 # Coverage gate
```

`go.work` (created by `scripts/bootstrap.sh`, git-ignored) links to local `core`/`wool` via
replace directives. When working on cli + core together, check them out at the expected
relative paths.

---

## Key Patterns & Rules

- **CLI ↔ Agent communication is ALWAYS gRPC.** Never import agent code, never call agent
  functions. The agent runs as a separate process. Clients: `runtimev0.RuntimeClient`,
  `builderv0.BuilderClient`, `agentv0.AgentClient`, `codev0.CodeClient`.
- **NEVER mock.** Tests use real agent processes and real infrastructure where possible.
- **The orchestration package is the most critical code.** Changes there affect every
  `codefly run`. Test thoroughly.
- **Configs flow as environment variables, not files.** Connection strings derived from network
  mappings are injected as `CODEFLY__SERVICE_...` env vars.
- **Daemon state lives in `~/.codefly/`** (override with `CODEFLY_HOME`). PID file, logs, agent
  binaries all live there.
- **MCP exposes codefly capabilities to AI agents.** When adding a CLI feature, consider whether
  it should also be an MCP tool.
- **Two-repo changes:** adding a core resource field from a CLI worktree needs a companion core
  PR + pseudo-version bump (core is read-only in CLI worktrees).

### Key environment variables
`CODEFLY_DEBUG` · `CODEFLY_SILENT` · `CODEFLY_WORKSPACE` · `CODEFLY_HOME` · `CODEFLY_HELP_PROVIDER`

---

## Maintaining this doc set

- **`AGENTS.md`** is the entry point — keep it short. It carries identity, the how-to index, and
  a skimmable architecture summary. Details belong in reference files.
- **`docs/runbooks/*.md`** answer "how do I do X" as ordered, copy-pasteable steps.
- **`docs/*.md`** are reference deep-dives (concepts, not procedures).
- When a fact here (a version, a path, a flag) changes in code, update the doc in the same PR.
  Prose that drifts from `go.mod`/`root.go`/`.goreleaser.yaml` is worse than no prose.
