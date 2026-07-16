[![Go CLI](https://github.com/codefly-dev/cli/actions/workflows/go.yml/badge.svg)](https://github.com/codefly-dev/cli/actions/workflows/go.yml)
[![Release](https://github.com/codefly-dev/cli/actions/workflows/release.yaml/badge.svg)](https://github.com/codefly-dev/cli/actions/workflows/release.yaml)

# Welcome to the `codefly.ai` CLI 😇

> We are building the next generation of tools for developers.

![](docs/media/dragonfly.png)

---

# Development

## Repository layout

codefly is developed as several repos checked out **side by side**. Put this `cli`
repo next to `core` and the agent repos (one repo per agent):

```
codefly/
├── cli/            # this repo — the `codefly` binary
├── core/           # shared library (github.com/codefly-dev/core)
├── service-go/     # agents: service-*, toolbox-*, module-* (one repo each)
├── service-rust/
├── toolbox-web/
└── ...
```

`core/` must be a sibling of `cli/` and of every agent repo — the build tooling
locates it by walking **up** the directory tree looking for the `core/` module.

Pull/refresh every repo at once:

```shell
codefly self pull        # pull latest main into every codefly repo (non-destructive)
```

## Requirements

- Go **1.25+** (see `go.mod`)

Codefly resolves the pinned `govulncheck` revision through Go's module/build
cache when an operator-managed binary is not on `PATH`. Agent builds fail if a
required scan cannot run; `--skip-audit` is the explicit waiver. Every built
agent binary also receives a sibling `.cdx.json` CycloneDX SBOM.

## Building the CLI

Local development builds against your **local** `core` (and every other local
repo), not the pinned published versions in `go.mod`. codefly does this with a
single **shared `go.work` at the workspace root** — gitignored, so CI keeps
building against the pinned versions (`GOWORK=off`).

Bootstrap it once, then build:

```shell
cd <workspace>                       # the folder holding cli/ and core/
go work init ./cli ./core ./sdk-go   # one-time: seed the shared workspace
(cd cli && go build -o codefly .)    # build the CLI against local core
```

Once the CLI exists, it manages the workspace itself — link every agent's source
into the same `go.work` so inter-repo dependencies also resolve locally:

```shell
codefly agent deps --link --all      # add every agent to the shared go.work
```

Rebuild the CLI later with `codefly self build` (compiles from source and installs
over the binary currently on your PATH; picks up local `core` via the `go.work`).

## Building agents

Each agent lives in its own repo with an `agent.codefly.yaml` (publisher, kind,
name, version). Build one, or every agent in the workspace:

```shell
# a single agent
codefly agent build --dir ./service-go

# every agent under a directory (e.g. run from the codefly/ workspace root)
codefly agent build --all --dir .
```

`codefly agent build`:

- auto-detects the workspace root (the directory whose `core/` is the local core
  module) and adds a local `replace` so the agent compiles against **local core**;
- installs each binary to `~/.codefly/agents/<kind>/<publisher>/<name>__<version>`;
- also cross-builds a Linux/amd64 static binary for Docker-mode runs. Running
  natively on a mac? Add `--native-only` to skip that and roughly halve build time.

Agents whose manifest sets `quarantine: true` are skipped by `--all`; build them
explicitly with `--dir` to work on the migration.

> **Flat checkout note:** `codefly self build --with-agents` rebuilds the CLI and
> all agents in one step, but it expects the agents under an `agents/services/`
> subdirectory. With repos cloned flat (side by side), use
> `codefly agent build --all --dir <workspace>` instead.

> **Version drift:** an agent only builds against local `core` HEAD if it has been
> migrated to the current core API.
> - An agent that depends on an older *published* sibling agent (e.g.
>   `service-go-grpc` → `service-go`) builds once that sibling is linked into the
>   shared `go.work` — `codefly agent deps --link --all` handles this.
> - An agent that imports *removed* core packages still needs migration; until then,
>   pin it to a compatible published core with `codefly agent deps --pin <version>`.

## Using local (latest) agents at runtime

Building an agent installs it under `~/.codefly/agents/`. To make codefly use those
local builds instead of downloading published agents, resolve agents locally:

```shell
codefly run service --local-agents        # or: export CODEFLY_AGENT_SOURCE=local
```

`--local-agents` resolves agent versions from `~/.codefly/agents/` only (skips
GitHub); an agent referenced as `latest` resolves **local-first**.

## Managing an agent's `core` dependency

`codefly agent deps` controls how a single agent (or `--all` of them) resolves core:

| Command | Effect |
| --- | --- |
| `codefly agent deps` (or `--link`) | wire a local `go.work` → local core (default; no network pull) |
| `codefly agent deps --unlink` | remove the local `go.work` (revert to published core) |
| `codefly agent deps --pin <version>` | pin `go.mod` to a published core version + tidy + verify the standalone build (`latest` allowed; the **only** mode that pulls) |
| `codefly agent deps --ci` | scaffold/repair the agent's `.github/workflows/ci.yml` |

## Common commands

```shell
codefly init workspace                    # create a workspace
codefly add service my-svc --agent go     # add a service backed by the `go` agent
codefly run service -d                     # run a service (with debug)
codefly doctor                             # check your environment for common problems
```

> Tip: while developing in the `cli` directory you can replace `codefly` with
> `go run main.go` in any of the commands above.

## Debugging into agent code

You can attach a debugger and step into a running agent:

- run the CLI with a breakpoint,
- get the agent PID from the log,
- set a breakpoint in the agent code,
- attach to that process in your IDE.

The CLI also exposes an endpoint carrying the client process PID, so the same
technique works for debugging the client code.

## Generating client code

```shell
codefly generate proto --proto ../proto --output . --local
```
