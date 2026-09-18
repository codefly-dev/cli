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
| `codefly generate runnables [module] [--check]` | Derive a SERVICE-facility package for every gRPC method a module's contracts mark with the operation option |
| `codefly list runnables [--module=<m>] [--json]` | List the workspace's runnables with their immutable identity, pinned agent, execution facilities and timeout |
| `codefly show runnable <name> [--version=<v>] [--json]` | Show one runnable's contract, execution bounds and dependency resolution |
| `codefly agent install <language>:<version> --kind=runnable` | Download a released runnable language agent into the local Codefly cache |
| `codefly agent info runnable --agent=<language>:<version>` | Load that agent over gRPC and print what it reports |

The language is the **agent's name**, never a branch in the CLI: a runnable
agent resolves to the `runnable-<language>` repository and executable, and
installs under `runnables/`, through the same agent-kind registry that
resolves service agents.

### Derived SERVICE-facility packages

A unary, idempotent method an owner service already publishes becomes a
Runnable by **derivation, not by authoring**. The method carries core's
`codefly.runnable.v0.operation` option; `codefly generate runnables` reads it
off the `contract.binpb` that `generate contracts` already published and writes
a SERVICE-facility package per marked method under `contracts/runnables`. Core
owns the option, the descriptor-to-bounded-schema projection and
`runnable.PackageFromMethod`; the CLI owns running them over a module's
published contracts, the layout on disk and the drift gate. Nothing about the
contract is authored twice — it is the projection of the method's own messages.

This is a separate path from `build runnable`, not a branch in it. A SERVICE
package has no archive and no launch command: the implementation is the method
itself, reached on the owner's endpoint, inside the process the owner already
operates. `pkg/runnable.assemble` only ever emits NATIVE artifacts and is
untouched.

The package is the contract; the execution policy and the authority a binding
is minted for sit **beside** it in `operation.json`, because they are installed
with the binding — two installations of one contract may run under different
ones, and digesting them in would make those two installations two releases.

See [`generate runnables`](commands.md#generate-runnables) for the output
layout, the refusal rules and how the release version is derived. What this
does *not* do is invoke anything: no CLI command invokes a Runnable, derived or
authored, and preparing a binding from `index.json` is the composition's
tooling.

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

Derived operations are listed through that same projection, so an operator sees
what a module exposes without reading the generated JSON. They carry facility
`service` and a `source` of `<service>/<endpoint>/<Method>`, which is empty for
an authored runnable — the distinction a reader needs, since a derived row is
regenerated from a contract rather than edited. `show` renders a derived
operation's method, the published messages it reuses, and the policy and
authority in its `operation.json`; it renders no handler and no dependency
report, because a derived operation is reached on a service the workspace
already runs.

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

- **Image builds.** The current Runnable build command accepts native artifacts
  only. It rejects build/schema/completion prerequisites and internal libraries
  whose preparation is not implemented. (SERVICE-facility packages are
  implemented — see [above](#derived-service-facility-packages) — and need no
  build, because the owner's own builder already built the method.)
- **Register, activate, invoke, inspect.** These are Orchestration surfaces.
  The CLI calls them; it does not host a Task/effect scheduler. The local
  runner below is the process boundary they dispatch *through*, not a way to
  invoke installed work from the command line: no CLI command invokes a
  Runnable.
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

## The local runner

`pkg/runnable.NativeLauncher` is the CLI's side of the physical process
boundary: the `os/exec` equivalent for one invocation of an installed native
binding. It is a package, not a command — Orchestration's native compute
adapter is its caller, and this milestone's durable path is not finished until
that adapter dispatches through it.

`NewNativeLauncher(package, binding, root)` accepts an installation, where
`root` is the directory the binding's artifact was unpacked into. Everything it
checks is an installation fact rather than a property of one invocation, so a
launcher that exists can dispatch: core's `VerifyBinding` ties the binding to
the package it installs, the facility is native, the artifact is built for this
host's `os/arch`, and the launch command is an executable file inside the
installed root. A Kubernetes binding, another platform's artifact or a command
pointing outside the package is refused here rather than at launch.

`Invoke` supervises exactly one invocation. An error from it means nothing was
dispatched, unless it carries `ErrDispatched`. A launcher failure *after* the
process started — a process group that would not end, log pipes that would not
drain, a result document that cannot be read — is recorded in the completion's
message rather than returned in place of the completion: a caller told an
invocation was never dispatched may run its effect a second time. Every way a
dispatched process can end is a completion:

- Core validates the invocation against the package (`PrepareInvocation`) and
  classifies the observed process (`Complete`), so a timeout, a crash and a
  harness that never wrote its result mean the same thing in every launcher.
  The launcher additionally refuses a budget longer than the package's declared
  timeout, and does not dispatch an invocation whose deadline has already
  passed — spending the attempt would make a timeout look like a crash.
- The framing is core's `codefly.runnable/v1`: three environment variables and
  two proto3-JSON documents, written into a caller-chosen directory as
  `invocation.json` and `result.json` and kept there as evidence. One directory
  holds one invocation; a directory that already has either document is
  refused, so a stale result is never read as this run's outcome.
- The process is started as its own process group, and the **group** is the
  unit of both supervision and cleanup: a descendant that outlives the process
  the launcher started is still a resource this invocation acquired. Nothing
  outside the group is touched.
- The deadline is the launcher's own, enforced with SIGTERM then SIGKILL. A
  result the harness already proved wins over the launcher ending the process,
  so a harness that completed and then failed to exit still reports its outcome.
  Waiting for the log pipes to drain after the process exits is bounded
  separately, because they stay open until every inheritor closes them: a
  handler that leaves a background child behind reports on that bound instead of
  being pinned to its whole deadline.
- Cancellation follows the declared capability. Only a package declaring
  `cancellation: signal` is interrupted when the caller's context is cancelled;
  one declaring `none` runs to its deadline, because advertising a cancellation
  the harness does not implement would report live work as abandoned.
- `stdout` and `stderr` are diagnostics, captured separately and bounded by
  `max_log_bytes`. Exceeding that bound truncates the stream and records that it
  was truncated; it never changes the outcome, unlike `max_output_bytes`, whose
  payload is completion data. A stream given no writer reports no captured
  bytes, since nothing was kept for a reader of the completion to go find.
  Completion data never travels as process output.
- The child's environment is the framing plus exactly what the caller resolved
  for the package's declared dependencies and configurations. Nothing of the
  CLI's own environment is inherited, so an ambient credential that happens to
  sit in it never reaches a handler.

The launcher never retries. A completion that is not `SUCCEEDED` or `FAILED`
leaves the operation's effect unproven, and resolving it — by recomputation or
by an effect receipt — belongs to whoever owns the package's recovery policy.

The real-process integration test requires a separately built agent:

```sh
CODEFLY_RUNNABLE_AGENT_BINARY=/absolute/path/to/runnable-python \
  go test -tags=integration -v ./pkg/runnable
```

This test creates through the CLI, builds, relocates the archive, removes access
to source/prepared files, and then drives the real generated harness through the
launcher: two inputs producing distinct typed outputs, an input of the wrong
shape refused before the handler runs, and a live handler interrupted through
the declared signal cancellation. It also checks rollback, explicit-output
refusal, default-output rebuild, evidence that no longer matches the source, and
archive tampering. What it proves is the launcher/harness boundary; it is not an
installed Orchestration task, and unpacking the archive is still the test's own
`tar` rather than an installer the CLI owns. The dedicated CI workflow pins the
agent source commit.

MCP create/build tools are deferred until the installation and execution surface
is qualified. The CLI commands are available for unattended authoring now.
