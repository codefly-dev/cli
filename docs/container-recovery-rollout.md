# Container-recovery marker rollout inventory

`codefly` projects a versioned container-recovery marker into the agents it
spawns, from `Flow.InitManagers` — so every command that spawns one projects it,
not only `codefly run`. This page is the inventory the coordinated release gate in
[cli#640](https://github.com/codefly-dev/cli/issues/640) needs: which agent
binaries read that marker, which of them create containers, and what each one
needs before the new CLI/fleet combination is published.

[`pkg/conformance/container_recovery.json`](../pkg/conformance/container_recovery.json)
is the machine-readable inventory; the table below is its view, and
`pkg/conformance` fails if the two disagree. The rules are enforced there, not
here: an agent that creates containers cannot be marked as needing no rebuild,
every agent a conformance row selects must be
classified, and a pin that cannot say which kind of agent it means is an error
rather than a guess.

## What crosses the boundary

`CODEFLY_CONTAINER_RECOVERY_SCOPE` carries `<pid>:v2:<scope>:<namespace>` to each
agent a command spawns, and to the containers the CLI process builds itself.
Its parser is `runners/dockerrun` inside the
**agent binary's own Core**, not the CLI's, so the CLI's core pin says nothing
about whether an agent understands what it was handed. Two consumers matter:

- **Container creation.** `dockerrun` stamps the resolved identity onto every
  container it creates. A binary that cannot parse the marker creates unlabeled
  containers no later sweep can collect.
- **Acknowledgement.** `agents/container_recovery.go` echoes the resolved
  identity in the `codefly-container-recovery-scope` header of
  `GetAgentInformation`. `Runner.validateContainerRecovery` compares it against
  what the CLI projected and refuses `Init` when they differ.

## Versioned API adoption

The CLI checks `AgentInformation.contract`, independently of the Core version
linked into the agent. Discovery requires lifecycle protocol 1 and startup
protocol 2, including for native agents. Container/free runtime initialization
and builder operations additionally require `container-recovery-scope/v1`,
then an exact acknowledgement of the flow's captured recovery identity.
An absent flow identity is an error on those paths. Native/Nix runtime
initialization does not require the recovery capability.

Agents with no declaration are rejected during discovery. Publishing support
for a selected agent requires qualification at its owner; it does not require
blanket fleet updates or block the CLI implementation PR's required host tests.
Neither a Core bump nor a matching recovery header proves that an undeclared
agent implements the contract.

CLI releases attach the consumed Core `contract.json` and an
`agent-requirements.json` using the same protobuf schema. The latter lists
capabilities needed by container/free and builder operations, not requirements
for every runtime context. Release notes compare both protocol versions and
these requirements with the previous stable CLI release. Unchanged requirements
need no agent rebuild solely for a CLI/Core bump. Missing or malformed artifacts
from a release that already publishes the contract fail preparation.

## Historical pre-marker fleet snapshot

This snapshot predates published Core v0.3.41. It explains the original rollout
exposure, not current compatibility policy or the current release inventory.

| Core revision | Recovery | Marker written | Acknowledgement |
|---|---|---|---|
| `v0.3.27` (`9267a219`) and earlier | none | none | none |
| `a26839c0` (#464) through `f91c021f` | exact scope | untagged | none |
| `471a8578` (#466) and later, including the CLI's `6a40c4bf28ac` | scope + namespace | `pid:v2:scope:namespace` | yes |

**No Core tag contains any recovery marker.** `v0.3.27` is the newest tag and
recovery first landed after it, so an agent pinned to a Core *tag* neither
labels its containers nor answers the acknowledgement. At the snapshot below,
the only repositories pinning a revision that carries the marker are the two
runnable agents, and neither is published.

The exposure is wider than Docker. The CLI sets the marker for every selected
runtime context except `native` and `nix`, and the default context is `free`,
so a service agent built before `471a8578` fails `Runner.Init` on the default
path whether or not it creates containers itself.
The former released-agent regression recorded that outcome against `go:0.0.47`.
Its replacement, `TestContainerRecoveryRejectsUndeclaredPeerBeforeLifecycle`,
uses a CLI-owned test peer and verifies rejection at discovery before lifecycle
calls. The required CLI suite no longer downloads the historical agent.

Native and Nix are exempt from the guard, which is one place the quiet failures
live: an agent selected for a native backend that still reaches Docker through
a Core companion creates its containers with no recovery label and nothing
fails loudly. Those are the `companion` rows below.

The other place is not a runtime context at all. **Every command that spawns an
agent projects the marker**, because projection happens in `Flow.InitManagers`
(`pkg/orchestration`) rather than in any one command: `codefly run`, `build`,
`test`, `ci`, `deploy`, `sync` and `validation` all funnel through it, as do
`pkg/gitops` and the SDK/daemon run path in `pkg/control`. `marker_projected_by`
in the inventory records the projecting package, and a test fails if any other
package starts or stops projecting one. Until that moved, only `codefly run`
projected: an agent spawned by `codefly build` or `test` inherited no marker,
resolved an empty scope, and `dockerrun` skipped both recovery labels without an
error — so `companions/proto` under build, `runners/testmatrix` under test, and
`service-mssql`'s Alembic migration runner all created unlabeled containers.

`codefly generate` was the one command still projecting nothing, and it is a
different shape of hole: it spawns no agent at all. `generate proto`,
`generate contracts` and `generate client` build their containers in the CLI
process itself (`runners.NewDockerEnvironment` in `cmd/generate/proto.go` and
`cmd/generate/contracts.go` — the latter reached by both `contracts` and
`client` through `buildDescriptorSet`), so no flow ever projects for them and
`companions/proto` created unlabeled containers there however new its Core was.
**Rebuilding that agent could not fix it** — there was no marker for the new
parser to read. `cmd/generate` now projects directly, which is why it joins
`pkg/orchestration` in `marker_projected_by`.

What it projects is the identity a later `codefly run` resolves, because both go
through the same `orchestration.ContainerRecoveryScopeFor`. That single resolver
is the point: the identity is a hash of home, workspace and naming scope, and a
second assembly of those inputs would drift silently — nothing fails when a
label merely matches nothing. `TestContainerRecoveryScopeHasOneResolver` holds
the recipe to one site.

Matching the exact scope is necessary but **not sufficient**, because a run need
not keep the naming scope `generate` saw. `--naming-scope`, a non-local `--env`,
and the invocation id `--temporary-ports` generates each change it, and
`ReapStaleContainers` compares a hash that includes it — so a leftover labeled
under the declared scope would survive every such run. The containers `generate`
builds are pure throwaways, so they are created ephemeral (`WithEphemeral`).
That brings them under `ReapDisposableContainers`, which is keyed on the durable
namespace of home and workspace alone and therefore collects a leftover from any
later run in the workspace, whatever naming scope it chose. Only a container
whose owning process is already gone is reaped (`shouldReapContainer` returns
false while the owner is alive), so a concurrent `generate` is never disturbed.

Outside a workspace there is no ownership to resolve, and the command warns once
and creates unlabeled containers rather than stamping a different identity no
sweep would ever match.

One further hole is in the projection decision itself rather than in an agent:
`codefly run` decides whether to project from the run's *launch* context, while
`preferences.codefly.yaml` overrides that context **per service**. A run
launched native with one service pinned to a container context used to project
no marker and skip the guard, so that service created unlabeled containers.
`cmd/run` now decides over every context the run can select, not just the one
it was launched with.

## Inventory

Every `codefly-dev` repository carrying an `agent.codefly.yaml`, read from its
default branch on 2026-09-13. Archived repositories (`service-s3`,
`service-minio`) are excluded.

*Creates containers* is `runtime` when the agent's own code builds a
`dockerrun` environment, `companion` when it reaches one only through a Core
package, and `none` when no path does — verified by resolving each repository's
full dependency closure, not by reading its imports. *Backends* is the
advertised `runnersbase.BackendSupport`, or `—` for a kind that declares none.

| Agent | Kind | Repository | Creates containers | Backends | Rebuild |
|---|---|---|---|---|---|
| `codefly.dev/clickhouse` | codefly:service | `service-clickhouse` | runtime | nix, docker | required |
| `codefly.dev/dynamodb` | codefly:service | `service-dynamodb` | runtime | docker | required |
| `codefly.dev/envoy` | codefly:service | `service-envoy` | runtime | docker | required |
| `codefly.dev/generic` | codefly:service | `service-generic` | none | — | required |
| `codefly.dev/go` | codefly:service | `service-go` | companion | local, nix, docker | required |
| `codefly.dev/go-grpc` | codefly:service | `service-go-grpc` | companion | local, nix, docker | required |
| `codefly.dev/mssql` | codefly:service | `service-mssql` | runtime | docker | required |
| `codefly.dev/neo4j` | codefly:service | `service-neo4j` | runtime | nix, docker | required |
| `codefly.dev/nextjs` | codefly:service | `service-nextjs` | runtime+companion | local, nix, docker | required |
| `codefly.dev/object-storage` | codefly:service | `service-object-storage` | runtime | docker | required |
| `codefly.dev/postgres` | codefly:service | `service-postgres` | runtime | nix, docker | required |
| `codefly.dev/python` | codefly:service | `service-python` | none | local, nix, docker | required |
| `codefly.dev/python-fastapi` | codefly:service | `service-python-fastapi` | runtime+companion | local, nix, docker | required |
| `codefly.dev/redis` | codefly:service | `service-redis` | runtime | nix, docker | required |
| `codefly.dev/rust` | codefly:service | `service-rust` | companion | local, nix, docker | required |
| `codefly.dev/swift` | codefly:service | `service-swift` | none | local, docker | required |
| `codefly.dev/temporal` | codefly:service | `service-temporal` | none | local, docker | required |
| `codefly.dev/vault` | codefly:service | `service-vault` | runtime | nix, docker | required |
| `codefly.dev/docker` | codefly:toolbox | `toolbox-docker` | none | — | not-required |
| `codefly.dev/grpc` | codefly:toolbox | `toolbox-grpc` | none | — | not-required |
| `codefly.dev/nix` | codefly:toolbox | `toolbox-nix` | none | — | not-required |
| `codefly.dev/python-repl` | codefly:toolbox | `toolbox-python-repl` | none | — | not-required |
| `codefly.dev/web` | codefly:toolbox | `toolbox-web` | none | — | not-required |
| `codefly.dev/sentry` | codefly:provider | `provider-sentry` | none | — | not-required |
| `codefly.dev/stripe` | codefly:provider | `provider-stripe` | none | — | not-required |
| `codefly.dev/unleash` | codefly:provider | `provider-unleash` | none | — | not-required |
| `codefly.dev/saas-starter` | codefly:module | `module-saas-starter` | none | — | not-required |
| `codefly.dev/go` | codefly:runnable | `runnable-go` | none | — | not-required |
| `codefly.dev/python` | codefly:runnable | `runnable-python` | none | local, docker | not-required |

Every service agent is `required` because the guard is on `Runner.Init` for any
service resolved to a context other than `native` or `nix` — the default `free`
included. Toolboxes, providers and the module agent are never driven by
`orchestration.Runner` and create no containers, so the marker never reaches a
consumer inside them. Both runnable agents already pin `6a40c4bf28ac`; neither
is published yet.

Observed Core pins at the snapshot: `runnable-go` and `runnable-python` at
`v0.3.28-0.20260912222100-6a40c4bf28ac`, and every other repository at a tag of
`v0.3.27` or earlier — except `service-go`, whose pin
`v0.3.28-0.20260912122621-177cb87e85ee` is a commit on the unmerged
`fix/recipe-only-runners-461` branch that forks from `v0.3.27` and carries no
recovery code either. No repository sits on the untagged intermediate
generation.

### How the classification was derived

Core creates a recovery-labeled container in two constructors,
`dockerrun.NewDockerEnvironment` and `dockerrun.NewDockerHeadlessEnvironment`.
An agent reaches them either from its own code or through `runners/companion`,
which `runners/golang`, `runners/rust`, `runners/testmatrix`,
`companions/proto` and `companions/dap` all call. `runners/base` — the native
and Nix runners — reaches neither, and `agents/helpers/docker` only builds and
inspects images. So, per repository:

```bash
# runtime: the agent's own code constructs the environment
grep -rn 'dockerrun\.NewDocker' --include='*.go' .
# companion: Core reaches one on its behalf. Resolve the dependency CLOSURE,
# not the imports — every agent pulls dockerrun transitively through
# agents/services for the acknowledgement interceptor, so an import of it
# proves nothing about container creation.
go list -deps ./... | grep -E 'core/runners/(companion|testmatrix)$'
```

Only `toolbox-docker` uses the Docker SDK outside this set, and its tools list
and inspect containers without creating any.

## What the rollout still needs

1. Rebuild every `required` row on `6a40c4bf28ac` or later and qualify scope
   acknowledgement, ownership labels, interruption and scoped cleanup through
   real processes and containers.
2. Qualify the `companion` rows under a **native** backend specifically. The
   guard is exempt there, so a stale binary is invisible to it, and no CLI test
   can stand in for that path.
   [The native qualification](../.github/workflows/container-recovery-native.yml)
   now rebuilds and qualifies every row that reaches a container through a Core
   companion, not only `service-go`, and `pkg/conformance` fails if its matrix
   and this inventory disagree about which rows those are. What it establishes
   is acknowledgement; the ownership labels, interruption and scoped cleanup in
   item 1 remain unqualified for every row.
3. Qualify the `companion` rows under `codefly build` and `test` as well as
   `codefly run`. Those commands now project a marker, so a rebuilt agent is
   what makes them label correctly — and that pairing is unqualified, on a path
   no row in the table above records. `codefly generate` is the exception that
   is now **qualified**: it builds its containers in-process, so no agent
   rebuild is involved, and
   `TestAnInterruptedGenerateLeavesARecoverableContainer` drives one against a
   real daemon — an interrupted generate's container carries
   `codefly.recovery-scope`, the exact-scope sweep of a run that renamed the
   naming scope walks past it, and the disposable sweep collects it.
4. Publish agents implementing the required protocol and capabilities, then
   qualify explicit artifact selections in `pkg/conformance/matrix.json` and
   update any user-owned selections that need those features. The CLI has no
   source-agent compatibility roster; unchanged protocols require no repinning.
5. Record the qualified combinations in [the supported matrix](supported-matrix.md).

Mixed generations are unsupported in the other direction too, and they fail
differently. An old CLI carrying the untagged generation projects a two-field
marker, which the v2 parser refuses outright rather than guess whether the
trailing field is a group or a namespace, so container creation fails loudly. A
released CLI projects no marker at all, and a new-core agent under it resolves
an empty scope and creates its containers with no recovery label and no error.
Upgrade the pair together.

Both of those are decided inside the agent, at container creation. What reaches
the CLI is only the resolved identity, which is what an agent echoes as its
acknowledgement and all `Runner.Init`'s guard compares — and a refused marker
and a missing one both resolve to nothing. **The guard cannot tell the two
apart, and catches neither**, which is why this gate is a rebuild of the fleet
rather than a check the CLI could make on its own.
`TestPinnedCoreMarkerResolutionsMatchTheRollout` pins each generation's
resolution against the Core this CLI pins, including the untagged marker that
carried the exact scope alone — the one field every revision agreed on, and so
the only one still honored. The creation-time half, where the refusal and the
missing marker diverge, belongs to Core and is covered there
(`runners/dockerrun`, `TestContainerRecoveryScopeAgentProcess`); it is not
reachable from this repository, whose only exported route to a container pings a
daemon. `TestFlowProjectsOverAnInheritedForeignMarker` covers the case where
this CLI is itself launched under another generation's marker, refused or
well-formed: it must project its own ownership over what it inherited rather
than adopt it.

The ordering and publishing mechanics live in
[the fleet release runbook](runbooks/release-the-fleet.md).
