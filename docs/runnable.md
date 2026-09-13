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
| `codefly list runnables [--module=<m>] [--json]` | List the workspace's runnables with their immutable identity, pinned agent, execution facilities and timeout |
| `codefly show runnable <name> [--json]` | Show one runnable's contract, execution bounds and dependency resolution |
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

### Listing is strict

A runnable whose declaration does not load fails `codefly list runnables`
rather than shortening the list. A caller about to build or install what it
finds must not be told a broken runnable is absent.

### Dependency resolution is a report, not a gate

`codefly show runnable` resolves each declared service dependency against what
the workspace declares and reports it as resolved or unresolved with the
reason. It deliberately does not fail on an unresolved dependency: the
declaration is still valid, and a caller needs to see which of several
dependencies is missing. Reachability, credential resolution and platform
compatibility are the installer's and launcher's responsibilities, not this
command's.

## What the CLI does not implement yet

None of the following exists in the CLI. There are no stub commands for them,
because a stub would be indistinguishable from a capability:

- **Create.** Scaffolding a runnable through the agent over gRPC.
  `Builder.LoadRequest` still carries a `ServiceIdentity`, so there is no way
  for an agent to receive a Runnable's resource, location and identity without
  a pretend service declaration. That handoff has to be defined in core's
  shared contract first.
- **Build.** Requesting build material from the agent, preparing identified
  native artifacts and assembling a `RunnablePackage`. `Builder.PackageResponse`
  returns artifacts with kind/path/target/digest but not the native launch
  command, generated harness identity, toolchain or effective configuration
  evidence, so the CLI cannot receive them from trusted agent output. Deriving
  them from `python` or a template name is exactly what this must not do.
- **The local runner.** Executing an installed native artifact under an
  approved execution specification. The `codefly.runnable/v1` launcher/harness
  exchange — request/result transport, identity and deadline fields,
  output-versus-log separation, byte limits, failure and cancellation
  behavior — is named by the protocol but not frozen anywhere. Inventing it in
  the CLI would make the CLI and the language agent's harness two independent
  guesses at one boundary.
- **Register, activate, invoke, inspect.** These are Orchestration surfaces.
  The CLI calls them; it does not host a Task/effect scheduler.
- **The Kubernetes path.** Orchestration's adapter owns durable
  invocation-to-Job identity, result handling and attempt policy.

Completing these paths requires coordinated work:

- The three shared-contract gaps (create, build evidence, invocation framing)
  are [codefly-dev/core#472](https://github.com/codefly-dev/core/issues/472).
- The language agents live in [runnable-python](https://github.com/codefly-dev/runnable-python) and [runnable-go](https://github.com/codefly-dev/runnable-go). Python has a harness and packaging implementation; Go currently has typed bindings. The shared gRPC lifecycle is still to implement.
- The durable surfaces are Orchestration's, coordinated through
  [obin-ai/module-runtime#63](https://github.com/obin-ai/module-runtime/issues/63).

[codefly-dev/cli#638](https://github.com/codefly-dev/cli/issues/638) is the
milestone that tracks all of them.
