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
| `codefly generate runnable-bindings [--env=<env>] [--check]` | Resolve the environment's scope selections and owner endpoints into prepared bindings |
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

An `index.json` is data on disk, and in a composed workspace it arrives inside
a third-party module package, so every path read out of one is resolved inside
the module directory through `os.Root` — a row naming `../../../.ssh/id_rsa`,
or an ordinary-looking directory that is a symlink out of the tree, is refused
rather than followed.

See [`generate runnables`](commands.md#generate-runnables) for the output
layout, the refusal rules and how the release version is derived. What this
does *not* do is invoke anything: no CLI command invokes a Runnable, derived or
authored. Preparing each operation's binding for an environment is
[`generate runnable-bindings`](commands.md#generate-runnable-bindings).

### Required scope slots and tool exposure

`generate runnables` preserves every field of Core's `Operation` in
`operation.json`, including `required_scope_slots` and optional `tool` exposure.
A slot-only operation may declare no fixed scopes. Generation keeps its slots
unresolved; it cannot choose resource IDs for the installing composition.

The composition answers each slot in the existing `workspace.codefly.yaml`
environment selected by `generate runnable-bindings --env`. Under
`runnable-scope-selections`, the key is the output binding key:
`<MODULE>__<OPERATION>`, upper-cased with non-alphanumeric characters replaced
by underscores. The operation name comes from `contracts/runnables/index.json`.
Each value is a list of Core `ScopeSelection` messages (`slot`, `invoke`,
`lookup`), using Core's scope fields. For example:

```yaml
environments:
  - name: staging
    runnable-scope-selections:
      SAAS_STARTER__ACCOUNTS_AUTHORITY_INVOKE_SOURCE_OPERATION: &source-scopes
        - slot: source
          invoke:
            - resource_kind: datasource.sources
              actions: [invoke, read]
              resource_ids: [source-a]
          lookup:
            - resource_kind: datasource.sources
              actions: [read]
              resource_ids: [source-a]
      SAAS_STARTER__ACCOUNTS_CONNECT_INVOKE_SOURCE_OPERATION: *source-scopes
```

`source-a` is an illustrative ID; select the exact resources this installation
may use. Quote IDs that look like YAML numbers. The owner of
`InvokeSourceOperation` requires `source` with actions `invoke` and `read`, and
`lookup: true`, so its lookup must cover the same exact IDs with action `read`.
When a module publishes the same operation on several endpoints, each index row
is a binding and needs its own selection. The saas-starter catalog publishes
this descriptor on both `authority` and `connect`; select
`SAAS_STARTER__ACCOUNTS_CONNECT_INVOKE_SOURCE_OPERATION` as well.

```sh
codefly generate runnables saas-starter
codefly generate runnable-bindings --env staging
codefly generate runnable-bindings --env staging --check
```

Preparation calls Core's `OperationSpec.ResolveScopeSlots` once per operation
and delivers its `Policy()`: fixed scopes stay in place, selected scopes are
appended, and no unresolved slot reaches a prepared binding. Missing selections
name the operation, slot and `runnable-scope-selections` key to set. An owner
declaring neither fixed scopes nor slots is still refused as declaring no
authority. Wildcards (including omitted IDs), collisions with a fixed scope's
kind or another slot's kind, duplicate/undeclared slots, unknown fields and
selection keys naming no derived operation are refused. A failed preparation
writes nothing and preserves the previous output.

The generated `configurations/<profile>/runnable-bindings.env` carries Core's
`codefly.runnable-prepared/v3` bindings: owner coordinates, the resolved JSON
call target, bounded contract and digest, and concrete policy. Tool metadata
survives preparation; absence remains `ErrNotATool`, and exposure grants no
authority. A gRPC operation is called on the owner's Connect endpoint; an
operation published directly on Connect uses that endpoint. Endpoint
addresses still come from the existing network resolver.

Changing a selection requires regeneration, and `--check` detects that change.
The configuration profile chooses the output directory; selections belong to
the named environment even when environments share a profile. These declarations
are installation input and are not sent to agents as runtime configuration.
There is no additional selection file, CLI policy vocabulary or CLI scope
resolver. Tests verify the prepared binding and the identical policy as a Core
resolved-policy receipt; this command emits the binding configuration only.

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

**Removed.** `pkg/runnable.NativeLauncher` was the CLI's side of a physical
process boundary: it started an installed native binding's command once per
invocation, wrote `invocation.json`, read `result.json`, supervised the process
group, enforced the deadline with SIGTERM then SIGKILL, and classified what it
observed. It is gone with the native placement it served, and so is the
`Runnable native lifecycle` workflow that qualified it.

Nothing replaces it here, because nothing needs to. A Runnable someone runs
locally is a service they run locally: the archive `codefly build runnable`
produces carries the generated harness, and the harness **serves** the contract
over Connect/HTTP JSON. It is called the way an invocation Job's container is
called — same transport, same headers, same Work Context — so the local path and
the production path are one path instead of two. The launcher framing
(`codefly.runnable/v1`, three `CODEFLY__RUNNABLE_*` variables and two documents
on disk) existed only to bridge the difference that no longer exists.

The one thing the launcher owned that mattered beyond it was the line between a
proven and an unproven effect: a handler that failed, a process that wrote
nothing, an invalid document, a crash, a timeout, a cancellation, and the
precedence among them. That distinction is what recovery depends on, so its
served equivalent — `RunnableServedOutcome`, `runnable.ClassifyServed` and
`runnable.ServedOutcomeIsCertain` — was written in core **before** any of this
was deleted, not after.

What the CLI still owes, and does not yet have, is the command that unpacks a
`generated-service` binding's archive and runs its harness. Until that lands,
`codefly build runnable` produces and verifies the archive and nothing in the
CLI starts it.

MCP create/build tools are deferred until the installation and execution surface
is qualified. The CLI commands are available for unattended authoring now.
