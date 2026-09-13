# Container-recovery marker rollout inventory

The CLI projects a versioned container-recovery marker into every agent it
spawns. This page is the inventory the coordinated release gate in
[cli#640](https://github.com/codefly-dev/cli/issues/640) needs: which agent
binaries read that marker, which of them create containers, and what each one
needs before the new CLI/fleet combination is published.

`pkg/conformance/container_recovery_rollout_test.go` holds this page to the rest
of the repository. Every language agent in the source-workspace compatibility
roster and every agent a conformance row pins must be listed here, and a listed
agent that creates containers cannot be marked as needing no rebuild.

## What crosses the boundary

`CODEFLY_CONTAINER_RECOVERY_SCOPE` carries `<pid>:v2:<scope>:<namespace>` from
the CLI to each agent it spawns. Its parser is `runners/dockerrun` inside the
**agent binary's own Core**, not the CLI's, so the CLI's core pin says nothing
about whether an agent understands what it was handed. Two consumers matter:

- **Container creation.** `dockerrun` stamps the resolved identity onto every
  container it creates. A binary that cannot parse the marker creates unlabeled
  containers no later sweep can collect.
- **Acknowledgement.** `agents/container_recovery.go` echoes the resolved
  identity in the `codefly-container-recovery-scope` header of
  `GetAgentInformation`. `Runner.validateContainerRecovery` compares it against
  what the CLI projected and refuses `Init` when they differ.

## Where the fleet sits

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
`TestContainerRecoveryRejectsAReleasedLegacyAgent` records that outcome against
the published `go:0.0.47`.

Native and Nix are exempt from the guard, which is where the quiet failures
live: an agent selected for a native backend that still reaches Docker through
a Core companion creates its containers with no recovery label and nothing
fails loudly. Those are the `companion` rows below.

## Inventory

Every `codefly-dev` repository carrying an `agent.codefly.yaml`, read from its
default branch on 2026-09-13. Archived repositories (`service-s3`,
`service-minio`) are excluded.

*Creates containers* is `runtime` when the agent's own code builds a
`dockerrun` environment, `companion` when it reaches one only through a Core
package, and `none` when no path does. *Backends* is the advertised
`runnersbase.BackendSupport`, or `—` for a kind that declares none.

| Agent | Kind | Repository | Creates containers | Backends | Rebuild |
|---|---|---|---|---|---|
| `codefly.dev/clickhouse` | service | `service-clickhouse` | runtime | nix, docker | required |
| `codefly.dev/dynamodb` | service | `service-dynamodb` | runtime | docker | required |
| `codefly.dev/envoy` | service | `service-envoy` | runtime | docker | required |
| `codefly.dev/generic` | service | `service-generic` | none | — | required |
| `codefly.dev/go` | service | `service-go` | companion | local, nix, docker | required |
| `codefly.dev/go-grpc` | service | `service-go-grpc` | companion | local, nix, docker | required |
| `codefly.dev/mssql` | service | `service-mssql` | runtime | docker | required |
| `codefly.dev/neo4j` | service | `service-neo4j` | runtime | nix, docker | required |
| `codefly.dev/nextjs` | service | `service-nextjs` | runtime+companion | local, nix, docker | required |
| `codefly.dev/object-storage` | service | `service-object-storage` | runtime | docker | required |
| `codefly.dev/postgres` | service | `service-postgres` | runtime | nix, docker | required |
| `codefly.dev/python` | service | `service-python` | none | local, nix, docker | required |
| `codefly.dev/python-fastapi` | service | `service-python-fastapi` | runtime+companion | local, nix, docker | required |
| `codefly.dev/redis` | service | `service-redis` | runtime | nix, docker | required |
| `codefly.dev/rust` | service | `service-rust` | companion | local, nix, docker | required |
| `codefly.dev/swift` | service | `service-swift` | none | local, docker | required |
| `codefly.dev/temporal` | service | `service-temporal` | none | local, docker | required |
| `codefly.dev/vault` | service | `service-vault` | runtime | nix, docker | required |
| `codefly.dev/docker` | toolbox | `toolbox-docker` | none | — | not-required |
| `codefly.dev/grpc` | toolbox | `toolbox-grpc` | none | — | not-required |
| `codefly.dev/nix` | toolbox | `toolbox-nix` | none | — | not-required |
| `codefly.dev/python-repl` | toolbox | `toolbox-python-repl` | none | — | not-required |
| `codefly.dev/web` | toolbox | `toolbox-web` | none | — | not-required |
| `codefly.dev/sentry` | provider | `provider-sentry` | none | — | not-required |
| `codefly.dev/stripe` | provider | `provider-stripe` | none | — | not-required |
| `codefly.dev/unleash` | provider | `provider-unleash` | none | — | not-required |
| `codefly.dev/saas-starter` | module | `module-saas-starter` | none | — | not-required |
| `codefly.dev/go` | runnable | `runnable-go` | none | — | not-required |
| `codefly.dev/python` | runnable | `runnable-python` | none | local, docker | not-required |

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
# runtime: the agent creates its own environment
grep -rn 'dockerrun\.NewDocker' --include='*.go' .
# companion: it reaches one through Core
grep -rln 'core/runners/companion\|core/runners/golang\|core/runners/rust\|core/runners/testmatrix\|core/companions/' --include='*.go' .
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
3. Publish the rebuilt agents before or together with the CLI, then move
   `pkg/sourceworkspace/compatibility.json`, the agent pins in
   `pkg/conformance/matrix.json`, and `module-saas-starter`'s composed pins onto
   the published versions.
4. Record the qualified combinations in [the supported matrix](supported-matrix.md).

Mixed generations are unsupported in the other direction too, and they fail
differently. An old CLI carrying the untagged generation projects a two-field
marker, which the v2 parser refuses outright rather than guess whether the
trailing field is a group or a namespace, so container creation fails loudly. A
released CLI projects no marker at all, and a new-core agent under it resolves
an empty scope and creates its containers with no recovery label and no error.
Upgrade the pair together.

The ordering and publishing mechanics live in
[the fleet release runbook](runbooks/release-the-fleet.md).
