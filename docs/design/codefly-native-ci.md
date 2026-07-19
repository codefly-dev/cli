# Codefly validation and CI orchestration

Status: workspace validation dispatches plugin RPCs; agent-source validation and
agent packaging still contain transitional host-side execution and are not yet
architecturally complete

## Purpose

Codefly plugins own development-operation semantics. Linting, compilation,
testing, generation/synchronization, compatibility checking, auditing, SBOM
creation, and packaging are ordinary typed plugin operations. They are not CI
operations.

Local commands, editor/Mind integrations, and CI must call the same RPCs. CI
adds change selection, dependency expansion, scheduling, caching, policy, and a
durable report; it does not introduce another execution API.

A CI provider may allocate a machine, check out a revision, and invoke Codefly,
but it must not decide which language commands to run, which resources are
affected, how dependencies start, or how results are interpreted.

The canonical provider-neutral entry points are:

```text
codefly ci plan ...
codefly ci run ...
codefly agent ci ...
```

Those entry points compose the ordinary plugin operations also exposed by
local Codefly commands. A stage being included in `codefly ci run` must never be
the only way to invoke that operation.

There must be no Codefly command that generates a workflow containing `go
test`, `go vet`, `npm test`, `npm run lint`, Docker build steps, or equivalent
language/tool-specific validation. Those choices belong to Codefly and its
agents.

## Implemented foundation

The first provider-neutral vertical slice is operational:

- `codefly ci plan` accepts Git revisions or provider-supplied changed paths,
  resolves workspace path overrides and symlinks, classifies direct/global
  changes, expands transitive service dependents, and emits deterministic
  versioned JSON;
- `codefly ci lint`, `compile`, `test`, and deployable `build` consume the same
  affected plan; `--all` is an explicit override;
- `codefly lint service` and `codefly compile service` expose lint and native
  compilation as ordinary local operations. Their implementation and the CI
  phases share one Runtime-plugin dispatcher; CI owns only target selection and
  scheduling;
- `codefly test service` remains the canonical focused test runner. `codefly ci
  test` and the test phase of `codefly ci run` add selection and scheduling,
  then dispatch the same agent Test RPC and honor the same structured result;
- focused and workspace test flows resolve the selected advertised suite before
  runtime initialization. `NONE`, `START_DEPENDENCIES`, and `START_STACK` now
  produce distinct, dependency-correct lifecycles; repeatable `--suite`
  overrides are available on both CI commands;
- affected-service phases use a bounded dependency-aware scheduler. Selected
  transitive prerequisites finish before dependents, independent targets run up
  to `--jobs`, failures are reported in stable plan order, and cancellation
  drains every active flow before exit;
- test scheduling locks each target's runtime dependency closure, preventing
  concurrent suites from sharing one service-scoped agent or runtime stack;
- static lint/compile flows initialize only the validation target. Dependency
  changes still select dependents as targets, but databases, caches, and other
  runtime prerequisites are not started for static work;
- `codefly ci run` is the single gate and executes `verify`, non-mutating
  `sync-drift`, `lint`, `compile`, `test`, `audit`, `sbom`, and deployable
  `build` in order. `--phase` supports focused debugging;
- `verify` is a workspace-scoped task backed by the canonical base-integrity
  verifier. `sync-drift` initializes dependency builders for endpoint context
  but dispatches dry-run Sync only to the validation target; dependencies are
  never synchronized as an accidental side effect;
- every executable CI command writes an atomic, schema-versioned
  `.codefly/ci/report.json` by default. The report retains the affected plan,
  requested phase/suite task identities, dependency prerequisites, runtime
  resource ownership, timings, deterministic outcomes, blocked-by details,
  and the final error. Typed evidence includes exact integrity divergence,
  generated-file drift, audit counts, and CycloneDX SBOM artifact paths and
  hashes. `--format json` suppresses normal narration, emits the same artifact
  payload on stdout, and remains machine-clean on a non-zero exit;
- every report task carries a schema-versioned, content-addressed cache
  identity. The key binds Codefly/Core versions, platform/runtime context,
  phase/suite, target agent metadata and resolved binary digest, workspace and
  module inputs, target source, transitive service dependencies, and internal
  library closure. Identity calculation is active; restore/store is not yet;
- runtime lint and native-build actions dispatch agent RPCs. Provider files do
  not contain language commands;
- Core now exposes an additive, authoritative validation contract covering
  operation support, semantic scopes, lint fix support, named test suites, and
  each suite's live dependency mode, including audit, artifact build, SBOM,
  and non-mutating Sync support. A nil contract remains the explicit
  legacy-compatibility signal;
- Runtime Lint and native Build now have authoritative package/file selection
  with echoed selection identities. Lint additionally carries normalized
  diagnostics and output-truncation metadata while retaining its compatible
  target/fix/status/output fields;
- the generic Next.js agent installs dependencies on a fresh checkout and owns
  runtime lint, typecheck/application build, structured tests, and Tooling
  delegation. It advertises workspace lint/compile/test/audit/SBOM/artifact
  support, package/file lint scopes, lint fixes, a dependency-free default unit
  suite, and a true non-mutating generated-client drift check;
- `codefly agent build` adapts a checkout into a generic source resource and
  dispatches ordinary Builder `Package`. The selected language plugin owns the
  build graph, target compilation, and CycloneDX generation; the CLI only
  installs the typed executable and SBOM artifacts returned by the plugin;
- `codefly agent ci` implements the generated-service conformance portion of
  the first service-agent slice: it creates a fresh workspace/module/service
  from the candidate agent, executes the complete workspace gate, proves
  repository restoration, and emits one schema-versioned report with
  binary/SBOM hashes and the nested workspace report. Source validation,
  security audit, packaging, and SBOM generation are ordinary Runtime `Test`,
  Builder `Audit`, and Builder `Package` operations on the generic source
  resource; the host contains no Go test/build/scanner command selection;
- Core exposes one generated `codefly.base.v0.Failure` taxonomy for plugin,
  host, report, editor, and automation boundaries. Lifecycle statuses and CI
  stages retain the stable reason, transport class, retryability, field/source
  diagnostics, process evidence, resource identity, details, and cause chain;
- the public agent-CI report contract is
  `core/proto/codefly/ci/v0/report.proto`; the CLI constructs generated
  `codefly.ci.v0` messages and serializes protobuf JSON. Handwritten report
  DTOs are not an alternate schema;
- the hardcoded workflow generator was removed from `codefly agent deps`, and
  the Next.js agent's repository-local workflow was deleted.

Still pending are capability and suite advertisement across the other
language-family agents, source-resource selection for non-Go checkouts, a
protobuf/schema plugin for lint/generate/compatibility, local command exposure
for every operation, agent orchestration for non-service agent kinds, cache
restore/store, and the thin provider adapter.
Agents without a validation contract retain explicit compatibility probing and
dependency-free tests. An agent that advertises an operation but returns
`UNIMPLEMENTED` now fails as a contract violation.

## Canonical schema plugin

Protobuf generation is one independent Codefly resource capability. It is not
a Go, Python, Rust, Next.js, gRPC-service, or CI implementation detail. Codefly
ships one schema plugin and every schema producer or consumer uses its typed
contract.

The schema contract is defined in Core protobuf before implementation and
exposes ordinary local-first operations:

- `lint` validates schema style and import correctness;
- `generate` resolves locked dependencies and emits declared generated output;
- `breaking` compares against an explicit baseline revision or descriptor;
- `drift` generates into an isolated destination and reports changed, missing,
  and unexpected owned files without mutating the checkout.

`codefly schema lint|generate|breaking|drift` and the corresponding workspace
gate tasks dispatch those same plugin RPCs. CI only selects affected schema
resources, expands their consumers, schedules tasks, applies policy, and saves
the typed result. It never invokes Buf or a language generator itself.

Each schema resource has a protobuf-backed configuration, rendered as a
Codefly manifest when a human-editable file is needed. The configuration owns:

- schema roots, import roots, module/lock files, and explicitly included files;
- a pinned execution bundle or OCI image digest and backend preference;
- ordered generator plugins identified by immutable artifact identity, with
  typed options rather than shell fragments;
- output roots and exclusive ownership globs for every generator;
- the compatibility baseline and policy for dependency updates;
- deterministic post-processing declared as plugin capabilities, never an
  arbitrary repository command.

The implementation reuses the existing proto companion as an execution
backend, but moves all Buf/protoc lifecycle details behind the schema plugin.
Docker, Nix, and local execution are backend choices of that plugin; their
observable operation contract and result schema are identical. Language and
service agents declare schema consumption and generated-output dependencies;
they do not carry Buf commands, generator installation, Docker mounting, or
compatibility logic.

The affected graph treats a schema resource as a producer. A schema or lock
change selects its lint/generate/breaking/drift tasks and every transitive
consumer whose generated contract may change. A generated-only consumer change
does not regenerate an unrelated schema. Cache identity includes normalized
schema/config/lock bytes, import closure, baseline descriptor, Codefly protocol
version, backend/tool bundle digest, generator artifact digests and options,
and output-ownership policy. Cache restoration is safe only when every declared
output is content-addressed and no undeclared file was written.

Clean migration removes, rather than preserves, the current direct paths:

- the CLI-local plugin installer and direct `buf generate` implementation;
- service-agent commands that spawn `buf` themselves;
- per-agent calls that each assemble proto companion mounts and generation;
- documentation or provider workflows containing raw `buf`, `protoc`, or
  language generator commands.

The first vertical slice is Core's own schema: prove lint, generation,
compatibility, non-mutating drift, unavailable-backend failure, and cache-key
stability through the plugin. Only after that proof should the gRPC service
agents and generated dependency clients move to the shared schema resource.

## Two orchestration contexts

### Workspace CI

`codefly ci` validates an application workspace. Codefly loads the workspace,
maps changed files to Codefly resources, expands changes through the service and
library dependency graphs, asks each service agent which validations it
supports, and executes the resulting task DAG.

### Agent CI

`codefly agent ci` validates a Codefly agent repository and proves a generated
service. It is an orchestration context, not the owner of Go, TypeScript,
protobuf, or packaging commands. Agent source trees and schemas must be modeled
as Codefly resources and dispatched to the applicable language/schema plugins.

Agent orchestration must:

1. ask language/schema plugins to validate the agent source and its SDK/Core
   compatibility;
2. ask the language plugin to package the agent, then install the returned
   artifact through Codefly;
3. create an ephemeral workspace using the agent with non-interactive defaults;
4. run workspace CI against the generated service through the agent's own RPCs;
5. build the deployable artifact and, where supported, run a minimal runtime
   smoke test;
6. fail if generation or validation changes tracked template output.

This replaces per-agent GitHub workflows. The implementation language belongs
to the selected plugin; neither `codefly agent ci` nor provider YAML may contain
language-specific execution.

## Audit findings that motivated the implementation

The pre-change implementation had useful pieces, but they were not connected
into a correct CI product:

- `codefly ci test`, `build`, `deploy`, and `push` existed.
- `codefly ci test` and `build` executed every service. They did not accept or
  expose a changed/affected selection.
- the CLI documentation claimed the CI commands accepted a service argument,
  but the commands rejected all positional arguments;
- the Core service graph already has dependency-to-dependent edges, topological
  ordering, direct dependents, and a generic graph projection;
- the runtime contract already has `Lint`, `Test`, and native `Build` RPCs;
- the CLI had no workspace `lint` command and no `ci lint` phase;
- only the Go and Python runtime families currently implement `Lint`; the
  Next.js agent implements structured `Test` but not runtime `Lint` or native
  `Build`;
- the runtime `LintResponse` currently contains status and raw output, while the
  transitional Tooling contract also models structured diagnostics;
- agent information advertises Builder/Runtime/HotReload but does not advertise
  validation operations, suites, scope support, or dependency requirements;
- `codefly ci test` creates a standalone flow for each service. Dependencies
  are ordered as separate test targets, but they are not kept running for an
  integration or end-to-end test of the dependent service;
- `codefly agent build` builds and audits an agent but does not run the agent's
  source tests, generated-service conformance, or generated artifact proof;
- `codefly agent deps --ci` generated a hardcoded GitHub Actions workflow. That
  generator has been removed;
- the existing `codefly-dev/github-action` invokes only `codefly ci test` from a
  stale container image and cannot provide change bounds or a complete CI run.

## Canonical change model

### Inputs

Change selection must be provider-neutral. `codefly ci plan` and `codefly ci
run` accept:

```text
--base <git-revision>
--head <git-revision>            # defaults to HEAD
--changed-file <path>            # repeatable; bypasses Git discovery
--all                            # explicit full workspace selection
```

Environment aliases may be provided for automation:

```text
CODEFLY_CI_BASE
CODEFLY_CI_HEAD
CODEFLY_CI_CHANGED_FILES
```

Provider integrations translate provider event metadata into these generic
inputs. Selection logic must never read GitHub-specific event JSON in the core
planner.

If Codefly cannot resolve the requested revisions or classify a change, it must
fail closed by selecting a conservative superset. It must never report an empty
plan because history is shallow or event metadata is missing.

### File-to-resource classification

Paths are normalized relative to the Git root, then compared with the absolute
workspace, module, service, application, job, and library directories loaded by
Core. Path overrides are therefore handled by the same resource loader used by
normal Codefly commands.

The initial classifier is deliberately conservative:

| Changed resource | Directly changed targets |
| --- | --- |
| file below a service directory | that service |
| service manifest | that service |
| file below a workspace library | services that declare that library, plus consumers of dependent libraries |
| module manifest or unclassified module-level file | every service in that module |
| workspace manifest, configurations, environment, or unclassified workspace-level file | every service in the workspace |
| documentation and provider metadata outside the workspace | no service task |
| unclassified executable/configuration input outside the workspace | every service in the workspace |

Rename and copy discovery must consider both old and new paths. Deleted
resources are classified using the old path; when the old graph cannot be
reconstructed, the containing module or workspace is selected conservatively.

### Affected closure

The service graph represents `dependency -> dependent`. For every directly
changed service, Codefly takes the transitive graph reachable from that service.
The union is topologically sorted, dependencies before dependents.

The plan distinguishes:

- `direct`: source, manifest, configuration, or library input changed;
- `dependent`: the service consumes a directly or transitively changed service;
- `global`: a workspace-level change selected the service conservatively.

Dependencies required to execute a test are not automatically validation
targets. They are runtime prerequisites for that target and are started by the
test flow only when the suite requires them.

### Public plan

`codefly ci plan` is the stable inspection and automation boundary. Text output
is for humans; JSON is versioned and machine-readable:

```json
{
  "schema_version": 1,
  "workspace": "example",
  "base": "...",
  "head": "...",
  "changed_files": ["modules/api/services/users/code/user.go"],
  "services": [
    {
      "service": "api/users",
      "classification": "direct",
      "reasons": ["service source changed"],
      "paths": ["modules/api/services/users/code/user.go"]
    },
    {
      "service": "web/frontend",
      "classification": "dependent",
      "reasons": ["depends on api/users"]
    }
  ]
}
```

The first implementation exposes service selection. A later compatible schema
addition exposes the per-phase task DAG and cache decisions.

## Validation operations

Codefly must keep these semantics distinct:

| Phase | Canonical owner | Meaning |
| --- | --- | --- |
| generate/sync | Builder `Sync` | regenerate dependency-derived source or configuration and prove no tracked drift |
| lint | Runtime `Lint` | static lint/format policy, optionally scoped, never silently fixes in CI |
| compile | Runtime `Build` | native compile/typecheck/application build without producing a deployable image |
| test | Runtime `Test` | structured unit/integration/e2e/smoke results |
| audit | Builder `Audit` | dependency vulnerability and outdated-package report |
| sbom | Builder `SBOM` | deterministic CycloneDX inventory and dependency graph |
| build | Builder `Build` | deployable artifact/container build |
| verify | CLI/Core | workspace/module composition integrity and global invariants |

`Lint` must not absorb typechecking just because a provider workflow used to run
both in one job. A Next.js agent, for example, owns `npm run lint` in `Lint`,
native typecheck/application compilation in runtime `Build`, structured Vitest
or Playwright execution in `Test`, and the deployable Docker image in Builder
`Build`.

### Agent-advertised validation capabilities

Calling an RPC and treating `UNIMPLEMENTED` as feature detection is too late and
produces poor plans. `AgentInformation` now has an additive validation section:

```text
ValidationCapabilities
  lint
    supported
    supports_fix
    scopes: WORKSPACE | PACKAGE | FILE
  compile
    supported
    scopes: WORKSPACE | PACKAGE | FILE
  test
    supported
    scopes: WORKSPACE | PACKAGE | FILE | CASE
    suites[]
      name
      dependency_mode: NONE | START_DEPENDENCIES | START_STACK
  audit
    supported
  artifact_build
    supported
  sbom
    supported
  sync
    supported
```

The advertisement helper in Core accepts the authoritative contract so agents
build it uniformly. Nil remains reserved for legacy compatibility during
migration; an explicit empty contract means the agent supports no validation
operations. Language-family agents declare shared capabilities once and their
specializations inherit them.

### Lint contract

The existing runtime `Lint` RPC is the canonical workspace CI operation. It has
been extended additively rather than replaced:

- keep `target` and `fix` for compatibility;
- add structured diagnostics with file, line, column, severity, rule, and
  message;
- add an echoed selection identity when an authoritative scope is provided, as
  structured Test already does;
- keep bounded raw output for tools that cannot produce diagnostics;
- return an operation status for lint findings; reserve gRPC errors for RPC,
  setup, or execution failures.

The transitional Tooling service delegates its lint/build/test methods to the
runtime implementation. CI must not depend on Tooling because Tooling is being
collapsed into the Toolbox surface.

## Phase invalidation

The planner begins with safe service-level rules. Agents can later refine test
selection using project metadata and file hashes, but they cannot broaden less
than the service-level safety floor.

### Direct service change

Run supported `sync`, `lint`, `compile`, `test:unit`, `audit`, and artifact
`build`. Run integration/e2e suites when the repository or CI policy enables
them.

### Downstream dependent

Run `sync`, `compile`, compatibility/unit tests selected by the agent, and
enabled integration/e2e suites. Build the deployable artifact when generated
clients, vendored internal libraries, or build inputs can incorporate upstream
content. Lint is skipped when no files in the dependent changed.

### Library change

Treat direct library consumers as directly changed for compile/test/build.
Static lint runs on the changed library through its language agent once library
agents expose validation. Service lint does not run unless the service source
also changed.

### Configuration or agent-version change

Run sync, compile, test, audit, and build for every service in scope. An agent
version change invalidates every phase cache for services using that agent.

### Documentation-only change

The service plan may be empty, but repository-level integrity checks still run.

## Dependency-correct test execution

Test suites need explicit dependency semantics:

- `NONE`: load/init only the target and run its test RPC;
- `START_DEPENDENCIES`: start the target's service dependencies in topological
  order, then run the target's test RPC without starting the target service;
- `START_STACK`: start dependencies and the target service, then invoke the test
  runner or probe described by the agent.

The runtime test policy now branches on the suite's advertised dependency mode
and keeps one flow alive until the target test finishes. For
`START_DEPENDENCIES`, the target's synthetic start action is a sequencing
barrier after its dependencies are ready; the target process is not started.
For `START_STACK`, the target remains running through its Test RPC. Invalid
suite names, missing defaults, multiple defaults, and unspecified dependency
modes fail before runtime initialization.

Unit tests may run concurrently across independent services. Integration and
e2e targets with overlapping dependency closures are serialized; disjoint
runtime stacks may run concurrently without sharing service-scoped agents,
ports, containers, or configuration ownership.

## Execution and scheduling

`codefly ci run` constructs a task DAG from the plan and agent capabilities.
Independent tasks run concurrently up to `--jobs`; graph prerequisites and
phase prerequisites remain ordered. The scheduler also preserves transitive
ordering when an intermediate graph service is not selected.

`--jobs 0` chooses an automatic concurrency bounded at four workers. The
default fail-fast policy stops dispatching new work after the first failure and
drains tasks already running. With `--fail-fast=false`, independent work
continues while dependents of a failed prerequisite are skipped. Concurrent
failures are aggregated in deterministic plan order.

Recommended default gates:

```text
verify
sync-drift
lint
compile
test:unit
audit
sbom
build
```

Integration/e2e suites are policy-controlled because they may require secrets
or expensive infrastructure. They are still invoked through Codefly, not
provider YAML.

The command supports:

```text
--phase <name>                 # repeatable override
--suite <name>                 # repeatable test-suite override
--jobs <n>                     # 0 = auto, capped at four workers
--fail-fast[=true|false]
--format text|json
--output <directory>           # reports and artifacts
--runtime-context free|local|nix|docker
```

The default is one complete Codefly-owned run. Individual phase subcommands
remain useful for local development and debugging, but provider integrations
should invoke `codefly ci run`.

## Reports

`codefly ci run`, `lint`, `compile`, `test`, and `build` always write
`report.json` under the `--output` directory. A relative output path is
resolved from the workspace root, making the artifact location independent of
the directory from which Codefly was invoked. Writes are atomic so a provider
never uploads a partially serialized report.

Report schema version 1 contains:

- the exact versioned affected-service plan and Codefly CLI version;
- command, phase, and named-suite context;
- stable logical task IDs in `phase[:suite]:module/service` form; the default
  test suite uses the explicit `test:default:` identity;
- plan classification/reasons, selected prerequisites, and the complete
  runtime resource lock set for each task;
- plan-ordered `passed`, `failed`, `skipped`, or `cancelled` outcomes, with
  `failed_prerequisite`, `fail_fast`, and cancellation reason codes;
- task/run timestamps, elapsed milliseconds, deterministic summary counts,
  blocked prerequisite identities, and retained errors;
- workspace/service scope and resource identity, exact base-integrity and
  generated-drift file lists, audit severity/outdated counts, and produced
  artifact media types, paths, and SHA-256 digests;
- the cache identity schema, canonical SHA-256 key, detailed input digests,
  current cache status, and any limitation that prevents safe reuse.

Tasks are serialized in requested phase/suite order and affected-plan order,
never goroutine completion order. `codefly ci run` registers every requested
phase before execution, so a failure in an early phase leaves later tasks
visible as skipped. This logical task model is also the attachment point for
the content-addressed cache identity described next.

## Caching

Cache identity belongs to Codefly because only Codefly knows the complete input
set. A phase key includes:

- Codefly CLI/Core protocol version;
- agent publisher/name/version and resolved binary digest;
- phase and suite;
- service manifest and selected source hashes;
- relevant workspace/module configuration hashes;
- lockfile and toolchain/runtime image digests;
- internal library hashes;
- upstream contract or generated-client hashes for dependent validations.

Provider caches may store Codefly's cache directory, but must not construct
cache keys themselves. A cache hit is part of the JSON report and never hides
which task would have run.

Cache identity schema version 1 is now emitted on every report task. Keys use
canonical JSON inputs and are prefixed with `sha256:`. Directory digests bind
relative paths, executable bits, symlink targets, and file bytes; Git-ignored
files and known transient dependency/build directories are excluded. This
means renames invalidate a key, while `node_modules`, `.next`, `.codefly`,
incremental TypeScript state, and equivalent runtime output cannot create
spurious misses. In a non-Git workspace Codefly falls back to the same
conservative transient-directory exclusions.

Transitive service dependencies are hashed even for standalone lint/compile
execution: runtime scheduling and content invalidation are distinct concerns.
Internal library dependencies are expanded recursively. Agent identity includes
publisher, name, version, kind, and the installed executable digest; an
unresolved binary is surfaced in `limitations` rather than silently treated as
equivalent.

The current task status is `identity_only`: Codefly computes and reports the
complete key but does not skip execution or restore artifacts yet. This avoids
claiming a cache hit before agent-declared output and environment contracts are
available. The restore/store milestone will add `hit`, `miss`, `stored`, and
`bypassed` outcomes while retaining the same identity input schema.

## Agent CI pipeline

`codefly agent ci` is the only CI command a service-agent repository needs.

The operational Go service-agent vertical slice runs these default stages:

1. detect and validate `agent.codefly.yaml`;
2. adapt the checkout into a generic source resource and dispatch its selected
   plugin's Runtime `Test` operation;
3. dispatch Builder `Package` for native and Linux targets and install only its
   returned executable artifacts;
4. retain the plugin-generated CycloneDX artifact associated with each binary
   digest;
5. dispatch Builder `Audit` as a distinct reported stage and apply the
   release policy to its typed findings;
6. create an isolated temporary Codefly home plus a fresh workspace, module,
   and service with `--default` using the newly
   built local agent;
7. run the full `verify`, `sync-drift`, `lint`, `compile`, `test`, `audit`,
   `sbom`, and deployable `build` gate through `codefly ci run --all`;
8. compare the complete pre-existing Git worktree state after validation so
   already-dirty development repositories are supported but new drift fails;
9. persist the agent binaries, CycloneDX documents, copied workspace evidence,
   and a self-contained outer `report.json`, then remove the isolated runtime.

`--native-only`, `--skip-audit`, and `--skip-conformance` are explicit local
development waivers. The default remains the complete release-oriented gate.
`--format json` emits only the report payload and still exits non-zero on a
failed stage. The current source runner supports Go service agents; other
implementation languages and application/module agent kinds must register
equivalent Codefly-owned runners rather than adding repository shell workflows.

An advertised smoke suite is the next compatible conformance addition. The
current vertical slice proves the deployable artifact through Builder `Build`
but does not claim runtime smoke coverage unless the workspace report contains
such a suite.

An agent may add conformance fixtures or opt into suites in
`agent.codefly.yaml`, but it may not provide shell command sequences. The agent
RPC implementation remains the source of truth for how work is performed.

`codefly publish` must run or require a successful agent-CI attestation before
tagging an agent release.

## Provider integration

The GitHub integration should perform infrastructure only:

1. checkout enough history for the requested base/head;
2. install or run a digest-pinned Codefly distribution;
3. call `codefly ci run` or `codefly agent ci` with generic base/head inputs;
4. upload the Codefly report/artifact directory.

No language setup, service matrix, test command, Docker build recipe, or
dependency selection belongs in the action. The same Codefly command must run on
GitHub Actions, GitLab CI, Buildkite, a local machine, or a self-hosted worker.

## Failure behavior

- Missing/shallow Git history: report the reason and select all, unless the user
  requested strict planning, in which case fail before execution.
- Invalid workspace or dependency cycle: fail before starting any task.
- Unsupported required validation: fail the plan unless policy explicitly marks
  it optional.
- Agent returns `UNIMPLEMENTED` despite advertising support: contract failure.
- Agent omits capability advertisement: compatibility mode explicitly skips an
  unimplemented optional static phase; required testing still fails. It never
  claims that the agent executed the phase.
- Validation finding: structured failed task, not an RPC transport failure.
- Cleanup failure: retained alongside the original task failure.
- Empty affected set: run global verify and emit a successful, explicit no-op
  service plan.

## Implementation sequence

1. In progress: remove generated and repository-local hardcoded plugin
   workflows. The generator and Next.js workflow are gone; remaining agent
   workflows stay until the replacement agent gate exists.
2. Completed: add `codefly ci plan` with deterministic text/JSON changed and affected
   service output.
3. Completed: refactor existing CI commands to consume the shared plan and explicit service
   selection.
4. Completed: add runtime lint/native-build flow modes and `codefly ci lint` / `compile`.
5. In progress: Core's validation capability contract, CLI enforcement, and
   Next.js advertisement are complete; migrate the remaining language-family
   agents.
6. Completed: implement Next.js dependency installation, runtime Lint, native Build, and
   Tooling delegation.
7. Completed: implement dependency-correct suite modes and repeatable CI suite
   selection. Remaining agents must advertise their integration/e2e suites to
   enable those flows.
8. In progress: `codefly ci run` has the complete ordered workspace gate,
   bounded dependency-aware scheduling, schema-versioned structured evidence,
   deterministic cache identities, and non-mutating sync drift. Cache
   restore/store remains.
9. Completed for the first Go service-agent vertical slice: `codefly agent ci`
   and fresh generated-service conformance. Add implementation runners for the
   remaining agent languages/kinds and advertised smoke suites.
10. Replace the legacy GitHub action with the provider-thin Codefly invocation.
11. Make `codefly publish` require the Codefly-native gate.
12. Delete remaining per-agent CI workflows after their repositories are on the
    new gate.

## Acceptance criteria

- A direct backend change selects that backend and every transitive service
  dependent, but not an unrelated service.
- A test-only frontend change does not lint or build unrelated backends.
- A shared library change selects every consumer and their transitive
  dependents.
- A workspace configuration change selects all services.
- `codefly ci plan --format json` is deterministic across repeated runs.
- an executable CI report is schema-versioned and plan-ordered, records every
  requested phase/suite task even after fail-fast, and can be uploaded without
  provider-specific interpretation.
- repeated runs over identical inputs produce identical cache task keys; target,
  workspace, agent/tool, suite, and transitive dependency changes invalidate
  them while unrelated services and ignored build output do not.
- unit tests do not start infrastructure; integration tests start exactly the
  required dependency closure; e2e starts the declared stack.
- every executed phase is performed through an agent or Codefly-owned global
  validator.
- the Next.js service passes dependency setup, lint, compile, structured tests,
  audit, deployable build, and smoke proof without provider-specific commands.
- `codefly agent ci` validates the Next.js agent repository and a freshly
  generated Next.js service with one command.
- no Codefly command can regenerate the removed hardcoded GitHub workflow.
- a CI provider file contains no language, package-manager, service, dependency,
  or build logic.
