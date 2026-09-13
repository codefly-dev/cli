# Runnables in the CLI

A **Runnable** is a packaged implementation of a typed finite operation: typed
input in, typed output out, declared dependencies and declared effect
semantics. It is declared in `runnable.codefly.yaml` and owned by a module,
next to that module's services and jobs.

Core owns the resource, the wire contracts and the package/binding
verification — see [core's `docs/runnable.md`](https://github.com/codefly-dev/core/blob/main/docs/runnable.md)
for the declaration format, the bounded schema profile and the installation
facts. This page covers what **the CLI** does with them.

Runnables are experimental. This page states what is implemented and what is
not; nothing here is a promise about the unimplemented parts.

## What the CLI implements today

| Command | What it does |
| --- | --- |
| `codefly add runnable <name> --agent=<name>:<version> --handler=<path>` | Create a declaration and scaffold through the real Builder agent |
| `codefly build runnable <name> [--output=<new-directory>] [--json]` | Prepare and package through the agent, verify archive bytes, write `artifacts/runnable-package.json` |
| `codefly list runnables [--module=<m>] [--json]` | List the workspace's runnables with their immutable identity, pinned agent, execution facilities and timeout |
| `codefly show runnable <name> [--version=<v>] [--json]` | Show one runnable's contract, execution bounds and dependency resolution |
| `codefly agent install <language>:<version> --kind=runnable` | Download a released runnable language agent into the local Codefly cache |
| `codefly agent info runnable --agent=<language>:<version>` | Load that agent over gRPC and print what it reports |

The language is the **agent's name**, never a branch in the CLI: a runnable
agent resolves to the `runnable-<language>` repository and executable, and
installs under `runnables/`, through the same agent-kind registry that
resolves service agents.

### Identity and coexistence

A runnable is identified by workspace/module/name **@ version**. A version is
an immutable release, and two versions of one name coexist: both listings and
`show` report the version, and nothing in the CLI resolves a name to a
"latest" release.

Because a name can therefore stand for more than one release, `show runnable`
refuses an ambiguous name instead of picking one — it lists every candidate as
`module/name@version` and `--version` selects one. A name that resolves to a
single release needs no flag.

### One projection per runnable

`codefly list runnables --json`, `codefly show runnable --json` and the MCP
`list_runnables` tool emit the same identity, agent, protocol and execution
fields, from one shared projection (`pkg/runnables`). `show` adds the contract,
entrypoint and dependency report on top; it does not restate the shared fields
differently.

### Listing is strict

A runnable whose declaration does not load fails `codefly list runnables`
rather than shortening the list. A caller about to build or install what it
finds must not be told a broken runnable is absent.

### Dependency resolution is a report, not a gate

`codefly show runnable` resolves each declared service dependency against what
the workspace declares and reports it as resolved or unresolved with the
reason. It mirrors core's binding rule for runnables
(`runnable/package.go`): endpoints are matched by NAME, because
`resources.Runnable.Proto` flattens each selector to its endpoint name on the
wire; a runtime edge needs at least one endpoint, while a legacy or external
edge needs one only when it names endpoints explicitly. A selector that names
no endpoint (`- api: grpc`) is reported as `::grpc` and unresolved, since it
flattens to an empty name that binds nothing. It deliberately does not fail on an unresolved dependency: the
declaration is still valid, and a caller needs to see which of several
dependencies is missing. Reachability, credential resolution and platform
compatibility are the installer's and launcher's responsibilities, not this
command's.

## What the CLI does not implement yet

None of the following exists in the CLI. There are no stub commands for them,
because a stub would be indistinguishable from a capability:

- **The local runner.** Supervising an installed artifact through core's frozen
  request/result protocol, with process groups, deadlines, cancellation, exact
  log bounds and typed completion classification.
- **Image builds.** The current Runnable build command accepts native artifacts
  only. It rejects build/schema/completion prerequisites and internal libraries
  whose preparation is not implemented.
- **Register, activate, invoke, inspect.** These are Orchestration surfaces.
  The CLI calls them; it does not host a Task/effect scheduler.
- **The Kubernetes path.** Orchestration's adapter owns durable
  invocation-to-Job identity, result handling and attempt policy.

Completing these paths requires coordinated work:

- The shared contracts (create, build evidence, invocation framing) are tracked in [codefly-dev/core#472](https://github.com/codefly-dev/core/issues/472).
- The language agents live in [runnable-python](https://github.com/codefly-dev/runnable-python) and [runnable-go](https://github.com/codefly-dev/runnable-go). Python implements native Builder gRPC and the shared invocation framing; Go currently has typed bindings.
- The durable surfaces are Orchestration's, coordinated through
  [obin-ai/module-runtime#63](https://github.com/obin-ai/module-runtime/issues/63).

[codefly-dev/cli#638](https://github.com/codefly-dev/cli/issues/638) is the
milestone that tracks all of them.

## Native authoring and build

```sh
codefly add runnable word-count --agent=python:0.0.1 --handler=handler.py --json
# Edit runnables/word-count/runnable.codefly.yaml and its handler.
codefly build runnable word-count --output=/tmp/word-count-build --json
```

Both agent and handler are explicit; the CLI has no language-specific defaults.
A compatible development agent is required until the fleet publishes qualified
releases. Creation rolls back a failed agent call's module reference and source.
Building preserves a failed explicit `--output` for inspection; choose a new
directory to retry. The default output, `.codefly/build/runnables/module/name/version`,
is a CLI-owned scratch directory and is replaced on every build, so the ordinary
edit/rebuild loop never strands itself on its deterministic path. Release
immutability lives in the package digest, which core's `CompareRelease` uses to
tell an idempotent re-registration from a conflict.

The CLI sends `Load(RunnableLocation)`, `RunnableBuildInputs` and `Package` over
gRPC. It constructs the descriptor from the declaration, shared build evidence
and `PackageArtifact.command`, validates it through core, and hashes both the
declared build inputs and the actual archive before writing the descriptor. The
input hashes are what tie the evidence to the author's source: without them a
stale agent snapshot would report a stale handler digest inside a stale archive
whose digest matches it, and nothing would notice the current source never
shipped. Paths, interpreter names and language
build tools come from the agent. No agent implementation is imported.

The real-process integration test requires a separately built agent:

```sh
CODEFLY_RUNNABLE_AGENT_BINARY=/absolute/path/to/runnable-python \
  go test -tags=integration -v ./pkg/runnable
```

This test creates through the CLI, builds, relocates the archive, removes access
to source/prepared files, checks real typed I/O with core's codec, and checks
rollback, explicit-output refusal, default-output rebuild, evidence that no
longer matches the source, and archive tampering. It is a debugging
invocation, not an installed Orchestration task or a production CLI supervisor.
The dedicated CI workflow pins the agent source commit.

MCP create/build tools are deferred until the installation and execution surface
is qualified. The CLI commands are available for unattended authoring now.
