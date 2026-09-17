# AGENTS.md — codefly/cli

> **Canonical instructions for AI coding agents and human contributors.** `CLAUDE.md` and any
> other tool-specific file is a thin forwarder pointing here. Every codefly repo uses this
> layout — see [Maintaining this doc set](#maintaining-this-doc-set).

## Module & Repository

- **Module:** `github.com/codefly-dev/cli`
- **Repository:** `https://github.com/codefly-dev/cli`
- **Language:** Go 1.26 (`go.mod` is the source of truth — see [Bump the Go version](docs/runbooks/bump-go-version.md))

The CLI is the primary interface for codefly. It orchestrates agent lifecycles, manages the
daemon, runs services with their dependency graphs, and exposes an MCP server for AI tool
integration.

---

## How to work here

These rules are fleet-wide (obin-ai/handbook#68) and outrank anything below them.

**A gap in the tooling is a bug in the tooling** — never a reason to reach around it. If
`codefly` cannot express what you need, the deliverable is the missing capability, not a
script that does it behind the CLI's back. Not as a "workaround", not "just this once", not
"until the verb lands".

**Never hack. Always provide the best fix, even when it spans repos.** The right fix living in
`core`, `wool`, or an agent repo is not a reason to work around it here — open the PR there.
When it genuinely cannot be fixed now, ship a precise issue against the owning repo *plus* an
explicitly labelled stopgap. Never an unlabelled one.

**Classify every change that makes something work**, in the PR body: a **fix** at the place
that owns the behaviour, or a **hack**. A hack does not become a fix by working, by being
small, by being local, or by the real fix belonging elsewhere.

**Never hardcode what the system resolves.** The CLI's whole job is resolving these — network
mappings into `CODEFLY__…` env vars, endpoint addresses, ports, agent binaries, credentials.
Hand-writing any of them makes the failure silent: a service that boots, serves, and never
registers looks identical to one that works. If you are typing a value the orchestration layer
is supposed to inject, you are encoding something true only on your machine for ten minutes.

**Diagnose, do not pattern-match.** "It started working when I set X" is not a diagnosis — set
X back and confirm it breaks. Do not trust an error message before checking it: a reported
digest mismatch has meant a missing token, with the digest verified correct by hand.

**Say what you did not verify.** Unverified is not the same as working. If you could not
exercise a path — no cluster, no credentials, agents not rebuilt — the PR says so.

---

## How-To Index

Task-oriented procedures. Each links to a full runbook under `docs/runbooks/` and is also
packaged as a skill in `.claude/skills/`, so it triggers without this file being read. Add a
new runbook (and its skill) whenever you do a multi-step operational task a second time.

### Toolchain & dependencies
- **Bump the Go version** (core + cli + agents + CI images) → [docs/runbooks/bump-go-version.md](docs/runbooks/bump-go-version.md)
- **Go standards** (formatting, linting) → [docs/go.md](docs/go.md)

### Shipping
- **Cut a release** (`codefly publish` → GoReleaser → Homebrew cask) → [docs/runbooks/cut-a-release.md](docs/runbooks/cut-a-release.md)
- **Release the whole agent fleet** (re-pin every agent on a new core, publish in dependency order) → [docs/runbooks/release-the-fleet.md](docs/runbooks/release-the-fleet.md)
- **How releases & self-update work** → [docs/cli-updates.md](docs/cli-updates.md)

### Extending the CLI
- **Add a new command** (Cobra wiring, help, MCP exposure) → [docs/runbooks/add-a-command.md](docs/runbooks/add-a-command.md)
- **Rebuild the CLI and agents from local source** → [docs/runbooks/update-agents.md](docs/runbooks/update-agents.md)
- **Point a workspace at a cell** (import a `codefly/cell/v1` contract instead of hand-typing cell facts) → [docs/commands.md#codefly-environment](docs/commands.md)
- **Export a module's API contracts** → [docs/commands.md#generate-contracts](docs/commands.md#generate-contracts)

### Reference (deep dives, not step-by-step)
- **All CLI commands, by category** → [docs/commands.md](docs/commands.md)
- **Orchestration engine** → [docs/orchestration.md](docs/orchestration.md)
- **Runnables** (what the CLI does with `runnable.codefly.yaml`, and what is deliberately not implemented yet) → [docs/runnable.md](docs/runnable.md)
- **Deployment completion stages** (rendered / applied / bootstrapped / healthy, bootstrap ordering, expand/contract schema rollout) → [docs/deployment-completion.md](docs/deployment-completion.md)
- **Agent CI & port isolation** (why sequential agent CI must not share a host port) → [docs/agent-ci-port-isolation.md](docs/agent-ci-port-isolation.md)
- **Supported CLI/core/agent combinations** (the conformance matrix, and why a required row cannot skip itself) → [docs/supported-matrix.md](docs/supported-matrix.md)
- **Container-recovery marker rollout** (which agents read the process marker, which create containers, what each needs before a fleet release) → [docs/container-recovery-rollout.md](docs/container-recovery-rollout.md)
- **Daemon** → [docs/daemon.md](docs/daemon.md)
- **Dashboard** → [docs/dashboard.md](docs/dashboard.md)
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

`codefly explain` and `codefly <verb> --help` are authoritative for the surface at any commit;
[docs/commands.md](docs/commands.md) groups it by category and flags what is only partially
implemented. Don't rely on a list kept here — it drifts the moment a command lands.

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
- **pkg/agentkinds/** — the one owner of the short agent kind a user types (`runnable`)
  ↔ the kind core registers (`codefly:runnable`). Used by `codefly agent install --kind`
  and the MCP `list_agents`/`agent_info` schemas so the convention has a single copy.
- **pkg/runnables/** — the one projection of a `resources.Runnable` that every listing
  surface emits (`list runnables --json`, `show runnable --json`, MCP `list_runnables`).
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

- **`AGENTS.md`** is the entry point — it carries the behavioural rules, the how-to index, and a
  skimmable architecture summary. **Hard cap ~200 lines**: past that it costs context on every
  request and adherence drops. When it grows, add a nested `AGENTS.md` in the subdirectory (the
  closest file to the edited file wins, like `.gitignore`) — never append here.
- **`.claude/skills/<name>/SKILL.md`** makes a runbook self-triggering: the frontmatter
  `description` is all an agent sees before loading it, so it must say *what it does* **and**
  *when to use it*. Keep the body to the decision — when this applies, what must not be
  skipped — and let it point at the runbook for the steps, so there is one copy to drift.
  Every runbook has one.
- **`docs/runbooks/*.md`** answer "how do I do X" as ordered, copy-pasteable steps.
- **`docs/*.md`** are reference deep-dives (concepts, not procedures).
- When a fact here (a version, a path, a flag) changes in code, update the doc in the same PR.
  Prose that drifts from `go.mod`/`root.go`/`.goreleaser.yaml` is worse than no prose.
