# CLI Command Reference

Codefly formalizes development operations as typed gRPC APIs. The CLI is the user-facing entry point that orchestrates agents (gRPC plugin processes) to execute these operations.

This page is the narrative guide: what a verb is for, and the chains it takes
part in. For the complete list of every command and flag, see
[cli-reference.md](cli-reference.md), which is generated from the command tree —
a command cannot exist without appearing there, and its text is the same one
`--help` prints.

## Quick Start Workflow

```bash
codefly init workspace my-project        # 1. Create a workspace
codefly add module backend               # 2. Add a module
codefly add service api --agent=go-grpc  # 3. Add a service with an agent
codefly run service api                  # 4. Run the service locally
codefly test service api                 # 5. Test the service
codefly build service api               # 6. Build container image
codefly deploy service api              # 7. Deploy to environment
```

## Global Flags

| Flag | Description |
|------|-------------|
| `--debug`, `-d` | Enable debug log output |
| `--trace` | Enable trace log output (very verbose) |
| `--focus` | Enable focus log mode |
| `--track <name>` | Enable action tracker (advanced usage) |

---

## Help and contextual explanations

Every command provides complete static help without network access:

```bash
codefly build --help
codefly help build service
```

`codefly explain [command...]` prints that same static help and optionally asks
an installed help provider for a workspace-aware explanation:

```bash
codefly explain build service
codefly explain deploy module
CODEFLY_HELP_PROVIDER=/path/to/provider codefly explain run service
```

The provider is a separate executable rather than an LLM client embedded in
the CLI or the Mind Gateway. This keeps the Homebrew CLI independent of a
running Mind process, model SDK, network connection, or API key. When no
provider is installed, or when it fails, `codefly explain` still prints the
static help and exits successfully.

By default Codefly looks for `codefly-help` on `PATH`.
`CODEFLY_HELP_PROVIDER` can select another executable.

```bash
go install github.com/codefly-dev/cli/cmd/codefly-help@latest
export OPENAI_API_KEY=...
codefly explain build service
```

The reference `codefly-help` provider is built and installed separately from
the main CLI. It calls the OpenAI Responses API without an SDK, defaults to
`gpt-5.6-luna`, and accepts `CLI_HELP_MODEL` and `CLI_HELP_API_URL` overrides.
Its protocol and implementation are CLI-agnostic so they can move into a
standalone repository without changing the Codefly integration.

Codefly writes one JSON request to the provider's standard input:

```json
{
  "protocol_version": 1,
  "application": "codefly",
  "command": "codefly build service",
  "static_help": "Usage: ...",
  "context": {
    "workspace": "storefront",
    "layout": "modules",
    "modules": ["backend"],
    "services": ["backend/api"],
    "jobs": ["backend/migrate"],
    "environments": ["staging"]
  }
}
```

The provider returns JSON on standard output:

```json
{
  "protocol_version": 1,
  "explanation": "Use this command when ..."
}
```

The protocol is intentionally CLI-agnostic: another application can provide
its own name, command help, and contextual JSON. Codefly reads only resource
names from `workspace.codefly.yaml` and `module.codefly.yaml`, caps each name
list at 50 entries, and never sends source files or unrelated configuration.

---

## Execution

### `codefly run service [name...]`

Run one or more services locally with their dependency graph.

Naming several services runs them as roots of a single graph: the services they
share are resolved once and started once, and each root is wired to them exactly
as it would be on its own. This is what a product composition needs — several
solutions against one host — because a graph per solution collides on ports and
on container-recovery scope, and `--stand-alone` cannot stand in for it (it skips
the dependency *wiring* too, so a second solution has no host endpoints to
resolve).

`--service-path`, `--stand-alone` and `--exclude-root` each select or carve out a
single service, so naming more than one root with any of them is rejected.

```bash
codefly run service api
codefly run service lastlogin-go/backend wiki/backend   # One graph, shared host started once
codefly run service api --standalone              # Run without dependencies
codefly run service api --runtime-context nix     # Use nix runtime context
codefly run service api --service-path ./my-svc   # Override service path for this run
codefly run service api --fixture seed            # Use a named fixture
codefly run service api --remote backend/db:staging  # Use remote dependency
codefly run service api --output-env .env         # Write the full owner-only SDK/runtime env
codefly run service web --output-env .env --output-env-service backend/api
codefly run service api --exclude-root            # Only run dependencies, not the service itself
codefly run service api --profile local           # Use a named workspace run profile
codefly run service api --exclude-dependency infra/temporal  # Omit optional dependency
codefly run service api --silent backend/db       # Suppress log output for a dependency
codefly run service api --cli-server --open       # Run headless with the local dashboard and open it
codefly run service api --readiness-timeout 10m   # Extend the startup readiness deadline
```

**Key flags:**

| Flag | Description |
|------|-------------|
| `--standalone` | Don't start dependency services |
| `--exclude-root` | Start dependencies only, skip the target service; when the root owns `--output-env`, compose its SDK environment without loading its agent or process |
| `--profile` | Select a named run profile from `workspace.codefly.yaml` |
| `--exclude-dependency` | Exclude optional dependency services from this run. Repeatable; accepts `module/service` or an unambiguous service name. |
| `--service-path` | Override the path to the service directory, for this run only and for the service being launched. To override a service durably, on any layout, see [Overriding one service of a composed module](#overriding-one-service-of-a-composed-module) |
| `--runtime-context` | Runtime context (`native`, `nix`, `container`, or `free`; `free` picks each agent's first advertised backend) |
| `--fixture` | Named fixture for test data |
| `--remote` | Use a remote service instead of local (format: `module/service:environment`) |
| `--silent` | Suppress output for named services |
| `--output-env` | Write the root service's full SDK/runtime environment (including configured secrets and dependency connections) to an owner-only file |
| `--output-env-service` | Export a specific running service (`module/service`) instead of the root |
| `--load-only` | Stop after Load phase |
| `--init-only` | Stop after Init phase |
| `--cli-server` | Start the CLI gRPC/Connect server and the embedded dashboard (implies headless output). The address is derived from the workspace name and `--naming-scope`; the run claims it before building the flow and fails if another process holds it |
| `--open` | Open the dashboard in the browser (requires `--cli-server`) |
| `--temporary-ports` | Run as a disposable invocation: ephemeral ports plus a generated naming scope isolating the run's agents, containers and runtime state. The Codefly SDK sets it for test-owned dependency stacks; see [disposable invocations](agent-ci-port-isolation.md#disposable-invocations) |
| `--naming-scope` | Fold a caller-chosen label into port derivation and resource names. Wins over the scope `--temporary-ports` would generate; passing it empty asks for no scope at all |
| `--readiness-timeout` | Maximum time the flow may remain not-ready (default `5m`). On expiry the run fails and names the service, endpoint, predicate, and last probe reason that held readiness. |

Run profiles define intentional local runtime shapes in
`workspace.codefly.yaml`:

```yaml
run-profiles:
  local:
    exclude-dependencies:
      - users/accounts
      - coordination/work-coordinator
    exclude-workspace-configurations:
      - internal-auth
      - forge-edge-auth
  saas: {}
```

`exclude-dependencies` contains service references (`module/service` or an
unambiguous service name). `exclude-workspace-configurations` contains names
declared by a service under `workspace-configuration-dependencies`. Codefly
validates the selected profile and all references before starting agents.
Repeatable `--exclude-dependency` values add to the profile's service
exclusions.

The two lists are coupled when a workspace configuration names an excluded
service: excluding a producer from the run leaves any
`${endpoint:<module>/<service>/<endpoint>}` reference to it with nothing to
resolve against, so the run is refused naming the group, the key and the
excluded producer. Exclude that group under `exclude-workspace-configurations`
as well (or stop excluding the producer) — a reference is never silently
dropped.

Profiles affect run composition only: they do not rewrite service or workspace
manifests, and build and deployment operations ignore them. In-process callers
select the identical resolver through
`control.RunRequest{Service: "mind/mind", Profile: "local"}`.

### `codefly run solution`

Boot a whole solution as a unit from its root. A solution root is a workspace
whose module declares a `service-entry` — the single runnable service the rest
of the composition hangs off. `run solution` resolves that entry and delegates
to the same dependency-graph orchestration as `run service <entry>`, so a
solution boots exactly the way its entry service does — no second sequencing
engine.

```bash
codefly run solution                         # From a solution root
codefly run solution --fixture dev-admin     # With a named fixture
codefly run solution --env local --headless  # Headless (CI, MCP, pipes)
```

The composed modules resolve through the [module resolver](#module-composition):
committed identity (`source` + `version`) plus a gitignored `codefly.local.yaml`
overlay, so the identical command runs in CI (everything pinned, no sibling
checkouts) and against your local worktrees.

The root is the module declaring a `service-entry` that no other
entry-declaring module depends on. A composed host declares an entry of its own
(the module a browser reaches first), but a solution's entry depends on the host
— through its services' `service-dependencies`, transitively, or through its
manifest's `api.consumes` — which makes the host a dependency, not a competing
root. That holds whether the solution is the workspace's own module or one
composed by `source` + `version` beside the host, so a product composition that
composes both runs the solution without naming it. When more than one entry is
left with nothing depending on it, the workspace's own module (`path: .`, or the
one named like the workspace) is the root; it errors clearly when no module
declares a `service-entry`, or when several do and none depends on another.

A solution entry — the `service-entry` of a module shipping a
`solution.codefly.yaml` — boots with what the composition would otherwise have
to hand-set, derived from that manifest wherever the module is (the workspace
root, or the cache checkout of a composed one): `CODEFLY__API_CONSUMES` and a
per-run registration secret for every facade prefix it consumes, and the
solution's own `CODEFLY__SOLUTION_REGISTRATION_SECRET`, minted per run and
declared to the host's `federation` group as `<module>:sha256hex` under
`SOLUTION_REGISTRATION_SECRETS` beside the module keys. The identity a solution
registers under is its module name. The run does not invent endpoints: the host
addresses the entry resolves (the gateway's `rest`, the frontend's `http`) are
the `service-dependencies` its `service.codefly.yaml` declares, and an address it
does not declare is not injected.

The fixture this run uses is resolved against the composed packages' manifests
before anything boots, so a typo fails at load naming the fixtures that do exist
rather than starting the whole stack and failing somewhere inside it. What is
checked is the *resolved* selection, not the flag: an environment declaring
`fixture:` in `workspace.codefly.yaml` is verified the same way when `--fixture`
is absent. The same check runs on `codefly test service`/`test solution` and on
the control-plane and MCP `test_service` path. Run
[`codefly show fixtures`](#codefly-show-fixtures) to see what a workspace
declares.

Only the selected name is judged. A name two composed packages declare
*differently* is refused, because that selection would no longer name one seed;
a clash on some other name is not this run's problem and does not block it. One
package composed under two module references declares its fixtures twice,
identically, which still names one seed and is not a clash.

Two cases pass without verifying anything, and both say so rather than passing
silently. A composed package that could not be read may be the one declaring the
selection, so the run proceeds with a warning naming what went unverified. And a
workspace whose readable packages declare no fixture at all is unaffected — the
name travels to the runtime as `CODEFLY__FIXTURE` as before.

**Behavior change:** once every composed package is readable and at least one
declares a fixture, that set is authoritative, so a name none of them declare is
refused — including one a service implements itself. Select such a fixture for
the service that implements it with
`--set <module>/<service>:CODEFLY__FIXTURE=<name>`, which is layered last and is
authoritative by construction.

Each run mints two independent secrets per consumed facade prefix — one the
consuming backend registers the route with, one the consumed module proves its
own identity with — and provisions every end of the federation exchange.
Provisioning writes nothing to disk, so a secret lives in the environment of the
processes that spend it and does not outlive the run — unless you ask for it with
`--output-env`, which exports a service's whole runtime environment, overrides
included:

| End | Carrier | Value |
|-----|---------|-------|
| Solution entry service | `CODEFLY__MODULE_REGISTRATION_SECRETS` | `prefix:secret,…` — presented to register each consumed module's routes |
| Consumed module's services | `CODEFLY__MODULE_IDENTITY_PREFIX` | the module's declared federation prefix, which can differ from its module name |
| Consumed module's services | `CODEFLY__MODULE_IDENTITY_SECRET` | that module's own identity secret, presented to mint its service-principal work context |
| Consumed module's services (deprecated alias) | `CODEFLY__MODULE_REGISTRATION_SECRET` | the same module identity secret, retained for existing runtimes; never the backend's registration secret |
| Registrar | `MODULE_REGISTRATION_SECRETS` in the `federation` workspace configuration group | `prefix:sha256hex` — the digests the registering backend is checked against |
| Registrar | `MODULE_IDENTITY_SECRETS` in the same group | `prefix:sha256hex` — the digests a module's own work-context exchange is checked against |

A module that federates no facade can still be a principal of its host — a
worker that mints its own module Work Context or runs a delegated exchange. Such a
service declares it in its `service.codefly.yaml`:

```yaml
module-identity: true
```

Every run — not only a solution entry's — then mints that module an identity
secret, hands it to the declaring services under the same three identity
carriers, and adds its digest to the registrar's `MODULE_IDENTITY_SECRETS`. The
identity is the facade prefix a solution in the workspace consumes the module
under, or the module name when none does. No registration secret is minted for
it: those exist for facade prefixes only. Which host admits the module, and with
what authority, remains that host's own configuration. When one run has several
roots, the first root's identity for a module is the one its services receive
and the registrar holds. A render does not yet derive declared identities; only
facade-consumed modules are rendered.

The two declarations are what let the registrar tell the backend registering
`documents` apart from the service principal of `documents`. A backend holds the
registration plaintext for every prefix it consumes and the identity plaintext for
none, so it cannot mint a consumed module's work context.

The registrar is whichever service declares the `federation` group. Two rules
follow from one prefix being one identity, and the run reports what it did with
every consumed module rather than leaving a gap to be discovered at runtime:

- Two `api.consumes` entries claiming the same `as` prefix are rejected — they
  would hand two modules one credential, letting either mint the other's work
  context. (`sync` and `package` already reject this; the run's lenient decode
  skips the full schema check, so it is enforced here too.)
- A consumed module that itself declares the `federation` group is the authority
  the exchange runs against, so it is not given a plaintext whose digest it holds.
- A consumed module the workspace cannot resolve, and a run with no registrar at
  all, both leave federation unconfigured: the run warns and boots, and the
  solution still serves its own routes.

**In a GitOps render** (`codefly deploy gitops render`, restricted profile) the
same carriers reach the deployed services, but nothing is minted: a render is
committed, so a secret written into it is published. Public carriers
(`CODEFLY__API_CONSUMES`, `CODEFLY__MODULE_IDENTITY_PREFIX`) are rendered into
the service's ConfigMap; every secret carrier is rendered only as a
`secretKeyRef` on `secret-<service>`, which the projected ExternalSecret
materializes from the environment's `service-secrets` store under the key named
by the carrier (the deprecated alias resolves to the stored
`CODEFLY__MODULE_IDENTITY_SECRET`). `codefly deploy secrets` writes each secret
and its digest (the registrar's `federation` group, already delivered by
reference) into the store, deriving both from one credential. A render that needs a secret but whose environment declares no
secret store fails instead of rendering a dangling reference or a value.

Every run and render also carries a service's **self endpoint** —
`CODEFLY__SELF_ENDPOINT__<MODULE>__<SERVICE>__<ENDPOINT>__<API>`, core's carrier
— beside its listen address `CODEFLY__ENDPOINT__…`: the in-cluster address in a
render, the address matching the runtime context in a run.

### `codefly run job [name]`

Run a job (scheduled or one-shot task).

```bash
codefly run job db-migration --module=backend
codefly run job db-migration --module=backend --with-services  # Start service dependencies first
```

### `codefly build runnable <name>`

Build and verify a native Runnable through its pinned Builder agent. `name` may
be `module/name` or an unambiguous bare name. `--output` selects a new directory
(the CLI-owned default is replaced on each build);
`--json` emits the verified package descriptor. This does not install or invoke.
See [Runnables](runnable.md) for prerequisites, evidence and current limits.

```sh
codefly build runnable word-count --output=/tmp/word-count-build --json
```

### `codefly build service [name]`

Build a service container image via the agent's builder.

```bash
codefly build service api
codefly build service api --standalone  # Build without dependency resolution
```

To build and push a single service's deployable image for a target environment —
the targeted fix for one image on a live cell, without re-rendering the module's
whole manifest tree — add `--push --env <env>`. The build owns the deployment
platform (`linux/amd64`) and build env exactly as a module render does, and on
success prints the pushed image's immutable digest (`Image digest sha256:…`).
Pass `--stand-alone` to build only the named service. To run the amd64 build on a
native/remote builder instead of local QEMU emulation, point it at a preexisting
docker buildx builder with `--builder <name>`. A service importing a private Go
module builds from modules the host fetched before the build, see [Private Go
modules in image builds](#private-go-modules-in-image-builds):

```bash
codefly build service front --env production --push --stand-alone --builder amd64-remote
```

A successful build also archives the generated build recipe (the `builder/`
Dockerfile and its sibling files) into `services/<svc>/build-recipes/<agent-version>/`,
with a `recipe.codefly.json` manifest recording the producing agent and per-file
digests. The live `builder/` tree is transient — re-rendered per machine and
excluded from composed modules — so this committed archive keeps the reproducible
recipe durable and inspectable for a consumer without the codefly toolchain. The
Dockerfile expects the service directory as the build context (it `COPY`s
`builder/…` paths); restore `builder/` from the archive, then rebuild directly,
e.g. `docker buildx build --platform linux/amd64 -f services/<svc>/builder/Dockerfile services/<svc>`.

### `codefly test service [name]`

Run one service's tests through its agent Test RPC. This is the focused local
runner and supports target, filter, suite, timeout, race, coverage, and native
runner arguments. Unit/default execution initializes only the selected target;
suite dependency modes will explicitly control live dependencies for
integration and end-to-end tests.

```bash
codefly test service api
codefly test service api --filter TestAuth --coverage
codefly test service frontend --suite e2e
```

The test path takes the same composition flags as `run service` — `--env`,
`--fixture`, `--profile`, `--exclude-dependency`, `--output-env`,
`--naming-scope` — so a test boots the graph the way a run does. One flag
behaves differently from `run`:

| Flag | Description |
|------|-------------|
| `--temporary-ports` | **On by default here**, where `run` defaults it off. Each invocation takes ephemeral ports plus a generated naming scope isolating its agents, containers and runtime state — overriding any scope the environment declares — so two concurrent `codefly test` runs in one workspace cannot collide. Pass `--temporary-ports=false` to keep the declared scope and deterministic names, or `--naming-scope` to name the scope yourself |

A suite whose agent advertises `START_DEPENDENCIES` or `NONE` never starts the
service under test. The fixture and the process overrides
(`CODEFLY__API_CONSUMES`, the federation registration secrets) still reach it:
Codefly delivers both on Init, which every service receives, and an agent takes
the first non-empty of the Init and Start values. `--output-env` is the one
input that cannot follow, because the exported environment is composed inside
Start — only there are the origin's endpoints and its dependencies' connections
final — so Codefly refuses that combination rather than writing a file that
silently omits them.

### `codefly test solution`

Test a solution as a unit from its root. It resolves the same `service-entry`
[`run solution`](#codefly-run-solution) boots, materializes composed pinned
modules, and delegates to the `test service` path with that entry — so a
solution is tested through exactly the orchestration that runs it, with the
solution-derived inputs (`CODEFLY__API_CONSUMES`, the federation registration
secrets) injected on the origin either way.

```bash
codefly test solution                          # The entry's default suite
codefly test solution --fixture dev-admin      # Against a named fixture
codefly test solution --suite e2e --headless   # A named suite, headless (CI)
```

It also runs the **composition tests the composed modules contribute**. A module
that composes a package declares them under `contributions.tests` in its
`module.codefly.yaml`, and each runs in that module's own checkout at the
declared path — the same directory the composition renderer runs it in. This is
how a module ships a test that every solution composing it runs ("a solution
registered with me is reachable through my gateway") rather than one only its
own repository executes.

Every contributed suite runs even after one fails, and a failure names the
module that contributed it. A contributed failure does not suppress the entry's
own tests: both results are reported, and the command exits non-zero if either
failed. A workspace whose modules contribute no suites is unaffected.

### `codefly deploy service [name]`

Deploy a service to a target environment.

```bash
codefly deploy service api
codefly deploy service api --standalone
codefly deploy service api --env production --render-only
codefly deploy service api --wait-for healthy --wait-timeout 5m
```

`--render-only` writes a validated, inventoried service-owned tree without
calling Kubernetes.

`--wait-for` selects the completion stage the deployment must establish before
it is reported as successful — `applied` (the default: `kubectl apply` succeeded
and nothing more), `bootstrapped` (the owned schema-preparation Jobs also completed) or `healthy`
(the owned workloads also finished rolling out). `--wait-timeout` (default 10m)
is the total observation budget for the command, shared across every service it
observes. The same flags apply to `codefly deploy module`. See
[deployment completion stages](deployment-completion.md).

For a complete module promotion, use the GitOps lifecycle:

```bash
codefly deploy gitops render payments --env production --app-project payments
codefly deploy gitops plan payments --env production
codefly deploy gitops publish payments --env production

# After review and merge:
codefly deploy gitops observe payments --env production \
  --app-project payments \
  --application payments-api

# Recovery is another reviewed promotion, never a direct cluster mutation:
codefly deploy gitops rollback payments --env production \
  --to-revision <previous-reviewed-commit>
```

`render`, `snapshot`, `plan` and `publish` evaluate the workspace readiness
verdict for the target environment **before** they do any expensive work, and
refuse with the `codefly doctor workspace` diagnostics and fix hints when it is
not ready. That check is the one in [`codefly doctor
workspace`](#codefly-doctor-workspace), scoped with `--module` to the module the
verb acts on: a configuration only a sibling module requires does not block this
one. Without it, a missing configuration group surfaced minutes later as `no
configuration found for <group>` from deep inside the builder, after modules had
been resolved and container images built and pushed.

```
☠️ workspace configurations — required workspace configuration "openrouter" is
   neither under configurations/local nor shipped by a composed module
   (required by demo/backend/api)
   fix: add configurations/local/openrouter.env (or openrouter.secret.env
   holding provider references)
☠️ Workspace is NOT ready for environment "local" — fix the items marked ✗ above.
Error: workspace is not ready for environment "local": … — refusing to render
(override with --skip-workspace-readiness)
```

`--skip-workspace-readiness` proceeds anyway. It exists for one situation — an
operator mid-repair who must render a module whose *sibling* is unready — is
never the default, and announces itself in the output when taken. The gate is a
precondition of the delivery verbs only: `observe`, `rollback` and the `remote`
verbs act on an already-reviewed revision and are exactly what an operator
reaches for while the workspace is broken, so they are not gated.

The gate runs after the module is resolved, because resolving it materializes
the workspace's composed pinned modules — which is what the readiness check
itself needs in order to judge configuration at all, and which is free once it
has happened. Everything the failure used to hide behind still comes after.

**In-cluster ports are allocated, never declared.** Each endpoint's in-cluster
port is core's `network.DeployedEndpointPorts`: the conventional endpoint of each
API takes the canonical port (`grpc` 9090, `rest` 8080, `connect` 8081, …) and
every other endpoint a stable port hashed from the name the workspace composes the
module under, the service and the endpoint. A tool outside the CLI reads the
allocation from [`codefly show network --json`](#codefly-show-network) or calls
that function. This is the Service port. A workload's container (pod) port is
the agent's to choose and may differ (a database agent keeps its engine's port
behind the allocated Service port), so the allocation does not replace a
module's declared pod ports.

**Declared container ports are checked against what the agent rendered.** A
service may declare `spec.deployment.endpoint-ports` — endpoint name to the
container port its pods listen on, which a module's own manifests (a
NetworkPolicy, say) are written against. No agent reports its container ports
over the builder protocol, so the render reads them from the tree the agent
wrote: it builds the unit's environment overlay with Kustomize, finds the
Service port entry publishing the endpoint's in-cluster port (the one its agent
was handed), and takes its `targetPort` — resolved through the pods that Service
selects when it names a container port. After every service is rendered and
before anything is installed, the render refuses a declaration that differs
from that port, one naming an endpoint that is not rendered in-cluster in the
environment, and one whose in-cluster port no rendered Service publishes,
listing every violation of the service at once:

```
service shop/api: spec.deployment.endpoint-ports disagrees with the manifests its agent rendered for environment staging:
  authority = 9091, but its pods listen on container port 19043 (the Service publishes port 19043 with targetPort 19043)
```

A go-grpc agent binds a named endpoint on its allocated port, so a declaration
there must be that port; a postgres agent publishes the allocated port and
targets 5432, so `tcp: 5432` passes. A service without a declaration is not
read, and a managed service (no workload) is skipped. The check adds nothing to
the rendered output. `codefly show network --json --rendered` reports the same
container ports to tools outside the CLI.

**Image platforms follow the cell.** The images a render (or a deploy) builds
are built for the architectures the environment's cluster nodes run, and for
nothing else, so every image runs natively on every node and no architecture
the cell does not run is emulated:

```yaml
environments:
  - name: staging
    cluster:
      kind: gke
      architectures: [amd64]   # the node architectures, Go/OCI names
```

A recipe declares every platform it can build; the environment picks which of
them to build, and a platform the cell needs that the recipe cannot build is an
error. A local cluster (`k3d`, `kind`, `minikube`) runs on the machine building
the images, so when it declares no architectures its nodes have the container
engine's (an amd64 CI runner builds amd64, an Apple Silicon laptop arm64). A
remote cluster that declares none, or an environment with no `cluster`, is
refused before anything is built. A pushed `codefly build service` for an
environment with no `cluster` builds a portable artifact instead: every platform
its recipe declares.

`render` is a function of the workspace and needs no cluster: by default no
service's manifests are sent to a Kubernetes API. Pass `--validate-cluster` to
also dry-run each rendered service server-side (`kubectl apply --server-side
--dry-run=server`) against the environment's declared `cluster.context` — the
environment must then declare one, and the namespace the manifests bind to must
already exist in that cluster: the rendered Argo Application does not create it
(`CreateNamespace=false`), so when it is missing the dry-run is skipped for that
service with a message naming the namespace and context rather than failing the
render. A cluster that cannot be reached is an error.

The destination namespace is provisioned outside the render, by the cell rather
than by the module: every Application the CLI stamps carries
`CreateNamespace=false`. A *service* unit therefore never ships a `Namespace`
object for the namespace it deploys into — it does not own a namespace its
sibling services share — and render drops one that a service agent still emits,
naming the manifest it dropped so that agent gets fixed. This is what lets a
governed cell work: an AppProject with an empty `clusterResourceWhitelist`
refuses `Namespace`, and the render no longer needs it whitelisted. A `Namespace`
naming anything other than the render's own destination is still a cluster-level
claim and is still held to the selected AppProject's authority. A *solution*
renders into a namespace nothing else shares and keeps owning it.

The workspace declares the destination repository, owned path, and Argo target
branch:

```yaml
gitops:
  repo-url: git@github.com:example/platform-manifests.git
  path: environments
  branch: main
```

Render first writes to a temporary sibling, rejects unsafe or non-promotable
manifests, invokes module agents for transport-neutral topology bundles, and
installs the selected environment resources and exact service graph under
`deployments/modules/<module>`. The installed
`.codefly-render.json` contains the sorted file inventory and aggregate digest.
Publish clones the selected GitOps repository, commits and advertises the
immutable service/module snapshot, derives exact AppProject authority from that
snapshot, and adds CLI-owned Applications pinned to its commit and paths. It
then creates the signed publication commit and opens or updates a pull request.
Planning does not advertise the snapshot or mutate the remote. Observe requires an
approved, merged pull request and verifies the publication digest, the
snapshot revision bound into every Application, exact service paths, project
authority, cluster identity, sync, operation, and Healthy status before writing
evidence under `.codefly/gitops/evidence/`.
Publishing requires configured Git commit signing and an authenticated `gh`
session; observation uses the active authenticated `argocd` context. Rollback
refuses a target revision unless a prior Healthy reviewed evidence receipt
links that revision.

**Contract admission.** `.codefly-render.json` records, per unit, the API
contracts it exposes (from the module's `contracts/api/catalog.codefly.json`)
and consumes (from its libraries' generated-client provenance), plus the
module's own package identity when it has a `module.package.codefly.yaml`.
At `plan` and `publish`, every consumed contract is checked against the
exposing module's own `.codefly-render.json` in the same GitOps path: the
module must be deployed there, still expose the endpoint, and its package
version must satisfy the consumer's pinned version or declared semver
constraint, with a matching contract digest (a compatible newer host with a
different digest is reported as drift, not a violation). `plan` prints the
resulting checks as a table; `publish` refuses when any check is a violation.
Pass `--allow-unresolved-contracts` to downgrade a violation caused by the
exposing module not being deployed yet to a skipped check, for bootstrap
ordering — every other violation still blocks publication.

**The deployed security posture.** A deployed render — any restricted profile:
`deploy gitops render`, `deploy gitops snapshot`, `deploy module`/`deploy service
--render-only`, `deploy solution`, and the service build behind [`deploy
dev`](#codefly-deploy-devmoduleservice--the-dev-escape-hatch) — refuses a
rendered workload that breaks one of three rules. Each refusal names the
service, the rule and the field that triggered it. A local or ephemeral render
is not a cell and is not held to them. The posture itself is the platform
owner's decision, recorded in obin-ai/handbook `decisions/security-posture.md`;
the render is where it cannot be forgotten, because it is the last door before a
cell and the one every module, solution and agent passes through.

| Rule | Refused when |
| --- | --- |
| `peer-transport-material` | A container is configured with certificate, key or CA material (`…TLS_CERT_FILE`, `--ca-cert=…`, a value naming `tls.crt`/`tls.key`/`ca.crt`), or a volume delivers that material as files, **while the environment asserts `internal-transport/mesh-protected`**. Transport between workloads is the mesh's; TLS at the edge is the ingress's. |
| `non-scratch-mount` | A workload volume is anything but the standard scratch volume (`emptyDir`) or a durable data claim (`persistentVolumeClaim`; a StatefulSet's `volumeClaimTemplates` are not pod volumes and are never read). Configuration and credentials reach a service as values and secrets in the environment variables the render already projects, so a ConfigMap, Secret, projected or `hostPath` mount means something reads a file the platform never delivers. |
| `in-memory-state-store` | A container is started in a development or in-memory mode: a `-dev`/`--dev…` switch on its command line, an environment variable whose name carries both a development and a credential word (`…_DEV_ROOT_TOKEN_ID`), one naming in-memory storage (`…_INMEM…`), or a storage/backend variable set to `memory`, `inmem`, `ephemeral`, `tmpfs` or `none`. A deployed product's keys and state live in a durable store; a development mode loses them on the next restart or node replacement. |

The rules read the manifests the cell would apply — the Kustomize-built overlay
output, so a volume an environment overlay patches into a conforming base is
refused too — and they read structured fields only: a container's volumes, its
`env` names and values, and its `command`/`args` tokens. A shell script an agent
hands its container as a single argument is that agent's program, not a field the
render holds to a rule.

```
Error: deployed render refuses service shop/frontend: non-scratch-mount — volume
"settings" takes its contents from a configMap source, and a deployed workload
mounts only the standard scratch volume (emptyDir) or a durable data claim
(persistentVolumeClaim); configuration and credentials reach a service as values
and secrets in the environment variables the render already projects, so a mount
means something reads a file the platform never delivers
(services/frontend/base/deployment.yaml: Deployment/frontend
spec.template.spec.volumes[1].configMap). Declare a deliberate exception as an
environment-level posture allowance (posture.allowances: rule non-scratch-mount,
service shop/frontend, and the reason), which the render then prints on every
run. The rule is obin-ai/handbook decisions/security-posture.md
```

**The environment declares the posture**, including the exceptions:

```yaml
environments:
  - name: staging
    posture:
      # What the environment states about the platform it deploys onto. An
      # assertion the render does not know is an error, never a no-op.
      asserts:
        internal-transport/mesh-protected: true
      # The deliberate exceptions. Each names one rule and one service
      # ("<module>/<service>", or a bare "<service>" for that service in
      # whichever module renders it) and says why; the reason is required.
      allowances:
        - rule: non-scratch-mount
          service: saas/frontend
          reason: the theme files are a build artifact of the host, reviewed in handbook#215
```

An allowance is never a silent skip: **every declared allowance is printed on
every deployed render**, whether or not that run needed it, and a render whose
exception has become unnecessary says so until the declaration is removed.

```
Warning: security posture: service saas/frontend is allowed to break rule
non-scratch-mount — the theme files are a build artifact of the host, reviewed in
handbook#215
```

An environment with no `posture` block asserts nothing and allows nothing:
`non-scratch-mount` and `in-memory-state-store` apply to every deployed render,
and `peer-transport-material` applies only where the mesh is asserted — a
service's own TLS may be the only transport protection an unmeshed environment
has. A client that must pin an *external* peer's CA is exactly what an allowance
is for.

#### Service secrets

When the environment declares `service-secrets`, render projects each service's
`secret-<service>` as an `ExternalSecret` that materializes exactly the keys the
service's manifests reference — no secret value ever enters git. A key resolves
to its remote location in one of three ways:

```yaml
service-secrets:
  secret-store:
    name: cell-secrets
    kind: ClusterSecretStore
  services:
    accounts:
      remote-keys:
        # scalar: the remote key, for a store of bare scalars
        SOME_KEY: some-remote-key
        # key + property: address a field inside a structured remote document
        # (an Azure Key Vault JSON secret, a Vault KV path)
        CODEFLY__…__IDENTITY_CLIENT_SECRET:
          key: lodestar-identity
          property: client_secret
      # defaults template: applies to every key not listed above; "{service}"
      # and "{key}" are substituted
      defaults:
        key: lodestar-{service}
        property: "{key}"
```

A key with no matching `remote-keys` entry and no `defaults` falls back to the
`<service>/<key>` store path. The rendered `ExternalSecret` is the single
source; the store is seeded from it with `codefly deploy secrets`, never by hand,
and nobody hand-authors ExternalSecrets.

#### `codefly deploy secrets` — seed the store from the render

```bash
codefly deploy secrets --env staging --dry-run --metadata-only  # which keys exist; reads no value
codefly deploy secrets --env staging --dry-run                  # full plan; reads the store in memory
codefly deploy secrets --env staging                            # write it (confirms; --yes to skip)
codefly deploy secrets --env staging --module runtime --dry-run  # one module's keys, plus its federation counterpart
```

It reads every `ExternalSecret` the render projected for `--env` under
`deployments/modules` (refusing one edited after the render), resolves the store
behind the `SecretStore`/`ClusterSecretStore` it names from the environment's
cluster (today: `gcpsm`, written through the operator's own `gcloud` login), and
resolves every remote property to a source, never printing a value:

| action | source |
| --- | --- |
| `keep` | the store already holds it; nothing stored is ever rotated |
| `derive` / `update` | a federation credential or the registrar's digests of them: a solution's registration secret, a consuming backend's `prefix:secret` map, a consumed module's identity secret. A credential the store already holds anywhere is reused; the registrar's digests are recomputed from the plaintexts, so both ends always agree |
| `propagate` | a configuration value (`CODEFLY__WORKSPACE_SECRET_CONFIGURATION__…`, `CODEFLY__SERVICE_SECRET_CONFIGURATION__…`) is one value however many services read it, so it is copied from the remote key that holds it |
| `generate` | declared random by `service-secrets.generate` (below) |
| `require` | nothing produces it: the operator supplies it — an external credential, or a value its producing agent derives |

**Each property is resolved by the secret key read out of it, not by the
property's name.** An `ExternalSecret` entry has both: `remoteRef.property` is
where the value is filed in the store, and `secretKey` is the
`CODEFLY__…` name core gives the configuration value — what the value *is*. They
are the same string only when an environment files a key under its own name; one
that maps keys to its store's own property names
(`service-secrets.services.<svc>.remote-keys` with `property: postgres_user`)
makes them differ for every key it maps. Every source above is named by the key —
a federation derivation, one configuration value shared across remote keys, a
`service-secrets.generate` declaration — so resolving by the property matched
none of them there, and each of those values reported as `require`: a secret to
type by hand for something the CLI derives itself. The plan reports both names,
and `Must be supplied` lists `remote-key#property (key)`.

A property the render recorded no `secretKey` for is refused rather than read as
its own key: nothing could say what that value is, so it would fall to `require`
whatever it actually was. A property read as two keys that
`service-secrets.generate` declares differently is refused too — whichever won
would be an accident of ordering.

Properties connected through configuration-key aliases resolve as one value,
including properties in the same remote document. Every existing holder must
agree. Federation derivation uses the service/key bindings in each rendered
entry, so separate services can store independent credentials in one document.
A mapping that mixes federation and configured sources is refused.

Existing empty required properties and malformed federation encodings are
errors: they never authorize minting a replacement. Missing externally supplied
values fail apply even when there is nothing to write. `--dry-run` still reports
requirements without writing, and `--allow-missing` permits only the remaining
valid writes. Neither flag authorizes invalid stored values or conflicting
sources. See [deployment secret safety](deployment-secret-safety.md) for migration,
output handling, and the limits of validation.

Apply writes plaintexts before the registrar digests that admit them — ordered by
the credentials, so a key carrying both is still written in the right place —
refuses while any property is `require` (`--allow-missing` writes the rest), and
refuses a `--metadata-only` plan, which never saw what existing keys hold. Two
keys holding one credential or one configuration value with different values are
refused rather than reconciled, whether or not a third key needs it propagated.

The federation is derived from the workspace and the keys from the render, so the
two can disagree, and where they do the store is left alone rather than rewritten:

- A credential no rendered `ExternalSecret` reads in plaintext is never minted —
  its module renders for another environment, or has not been rendered yet.
  Minting it would hand the registrar the digest of a secret no service will ever
  hold and, because the digest list is recomputed whole, drop the digest that
  module's running services are admitted by. It is reported as `require`: render
  its module for this environment, then seed again.
- A stored digest list that admits an identity this workspace no longer derives —
  a consumed prefix renamed after the render — is `require` too, naming what the
  rewrite would drop. Re-render the environment, or drop those identities from the
  store deliberately.

A managed service's remote keys (the environment's `managed-services.<svc>.secret-references`)
are planned like any other: its projection is its bundle's base rather than an
environment overlay, and both are read.

`--module <m>[,<m>]` limits the plan to the remote keys the named modules'
services read, plus their **federation counterpart**: the registrar's digest
properties that encode a credential those keys hold (scoping to a solution also
plans the registrar's digest of its registration secret, and nothing else of the
registrar's key). The plan names each counterpart key and property as such.
Every other remote key is still read, so a scoped key keeps agreeing with what
the store already holds, but nothing outside the scope is planned or written. A
scope that would mint a credential and store only its digest, its holder being
outside the scope, is refused.

A consumed API that the registrar's own module serves (a solution calling the
host's accounts service) is not federated: the host routes it itself, so
neither a registration secret for its prefix nor an identity credential is
derived — the same rule a run and a render follow.

Which configuration keys are random is declared by the environment:

```yaml
service-secrets:
  generate:
    - scope: workspace            # a workspace configuration group
      configuration: internal-auth
      keys: [CODEFLY_INTERNAL_TOKEN, CODEFLY_GATEWAY_TOKEN]
    - scope: service              # a service configuration, of every service
      configuration: postgres     # or of the `services` listed
      keys: [POSTGRES_PASSWORD, POSTGRES_READ_ONLY_PASSWORD, POSTGRES_READ_WRITE_PASSWORD]
    - scope: service
      configuration: postgres
      services: [runtime/store]
      keys: [POSTGRES_USER]
      format: identifier          # hex (default), base64, identifier; `bytes` sizes it
```

A key its producer declares as a template (core's `ConfigurationValue.template`
— a Postgres agent's `READ_WRITE_CONNECTION`, say: role, address and database
around a reference to its read-write password) is never read from the store.
The consumer's ExternalSecret reads the producer's referenced keys from the
**producer's** remote location — where the producer's own ExternalSecret reads
them — and assembles the value in the cluster with a `target.template`
(`engineVersion: v2`, `mergePolicy: Replace`). The store holds only the
primitives. An environment `template` may not also assemble such a key.

The producer's location is whichever surface the producer itself resolves
through: a managed service's keys come from its `secret-references`
(`remote-key`/`property`), a regular service's from `service-secrets` under the
producer's own scope. A producer resolving through a different `secret-store`
than the consumer is refused — one ExternalSecret reads through one store. A
template may only reference keys the producer's own deployment reads as
secrets; a reference to one of its plain values is refused at render.

`mergePolicy: Replace` is what keeps the primitives out of the consumer's
Secret. They are still listed under `data`, because the template has to read
them, but an ExternalSecret under `Merge` emits everything it fetches — which
would put the producer's raw password in `secret-<consumer>` beside the value
it assembles. Under `Replace` the template is the whole Secret, so every key
the consumer references gets an entry: its assembly, the environment's
expression, or a `{{ .KEY }}` pass-through.

Locally there is no reachable Git host for Argo to fetch from, so the CLI owns a
reproducible read-only fetch remote on the private k3d network:

```bash
codefly deploy gitops remote plan payments --env local
codefly deploy gitops remote up payments --env local
codefly deploy gitops remote status --env local
codefly deploy gitops remote down --env local
```

It serves an exact reviewed revision from a read-only mirror over TLS, binds any
host verification port to IPv4 loopback only, pins its image by digest, stamps
exact ownership labels, and refuses teardown when ownership or network identity
drifts. See [gitops-fetch-remote.md](gitops-fetch-remote.md) for the developer
and recovery workflow. `codefly doctor` audits any remote it finds.

Maintainers can run the disposable local qualifications (k3d, an in-network Git
remote, and pinned Argo CD) with:

```bash
CODEFLY_GITOPS_K3D_QUALIFY=1 \
  go test ./pkg/gitops -run TestLocalK3dDisposableGitQualification -v -count=1
CODEFLY_GITOPS_K3D_QUALIFY=1 \
  go test ./pkg/gitops -run TestLocalFetchRemoteLifecycle -v -count=1
```

### `codefly deploy dev <module>/<service>` — the dev escape hatch

> **Dev escape hatch, not a release path.** After it runs, the environment runs
> code that no module release and no full render describes, until the next full
> `codefly deploy gitops render` of that module.

Push ONE service's current code — uncommitted or unreleased — into a hosted
environment that a full render has already produced, in about the time one
image build takes, without tagging a module or re-rendering everything. Run it
from the deployment workspace the render writes into:

```bash
codefly deploy dev payments/api --env staging --path ../payments/services/api
codefly deploy dev payments/api --env staging            # uses the service override
codefly deploy dev payments/api --env staging --commit   # commit it
codefly deploy dev payments/api --env staging --commit --push
```

1. **Source.** `--path <dir>` if given; else the machine-local service override
   (`codefly override service <module>/<service>`, i.e.
   `resolve.<module>.services.<service>` in `codefly.local.yaml`); else it refuses
   with "nothing to deploy". A `--path` passes the same contract check an
   override does (same name, agent and endpoints).
2. **Build.** The service is built and pushed through exactly the path
   `deploy gitops render` takes for it: the environment's `registry` (and its
   login), the snapshot flow driving the service agent's Build and Deploy with
   push, and the same `@sha256:` digest capture. `DOCKER_HOST` is honoured the
   way every build honours it. That render is held to [the deployed security
   posture](#codefly-deploy-service-name) exactly as a full one is: a service
   whose current code renders a non-scratch mount, its own peer TLS on a
   mesh-protected environment, or a development-mode store is refused before its
   image reaches the cell, naming the service, the rule and the field.
3. **Patch.** Only that service's digest pin changes, inside its own rendered
   unit (`deployments/modules/<module>/services/<service>/`); every other byte of
   the tree is left as the render wrote it, and the render inventory is
   re-derived so the tree still matches it. It refuses when the module was never
   rendered, was rendered for another environment (or another `--app-project`,
   when given), was edited since its render, or does not pin the image the build
   produced — in every case: run the full render first.
4. **Record.** `.codefly-render.json` gains a `dev` entry for the service:
   source origin and path, the source's git commit and whether it was dirty, the
   image and digest, and the time. `codefly doctor workspace` reports each one as
   a `gitops_dev_deployment_active` warning.
5. **Git.** The changed files are staged and the exact commit and push commands
   printed. `--commit` commits them as
   `dev: <module>/<service> from <path>@<sha>[-dirty]`; `--push` (which needs
   `--commit`) pushes the current branch. Nothing is pushed otherwise. Argo CD
   then syncs the new digest.

**Leaving dev mode** is a full render: `codefly deploy gitops render <module>
--env <env>` re-derives every image from the workspace, drops the `dev` entries
and says which dev deployments it cleared.

### `codefly deploy solution [name]`

Drive a `codefly:solution` executor: package a solution source into an OCI
artifact and render its manifests into the owned gitops tree, in the solution's
own namespace.

```bash
codefly deploy solution lastlogin --env production \
  --agent codefly.dev:solution-generic:0.0.1 \
  --source ./solutions/lastlogin --reference ghcr.io/example/lastlogin:0.0.1
```

No `codefly:solution` executor is published today, so this command cannot
obtain one for any `--agent`, and it says so: when the executor cannot be
resolved or loaded it fails naming the step and the cause, and points at the
render path a composed solution actually takes. A solution composed into a
workspace by `source` + `version` is a module — its services run on service
agents — and renders like one:

```bash
codefly deploy gitops render lastlogin --env production
```

### `codefly deploy init`

Initialize deployment configuration for a service.

---

## Management

### `codefly add`

Add resources to the workspace.

```bash
codefly add module backend                                     # Add a module
codefly add module saas --agent=saas-starter                   # Scaffold and pin a module template
codefly add module host --source=../saas-host/modules/host     # Compose an out-of-repo module (location → codefly.local.yaml)
codefly add module documents --worktree=obin-ai/module-document-store@main  # Compose by identity; the resolver finds your local worktree
codefly add service api --agent=go-grpc                        # Add a service with an agent
codefly add service-dependency api --dependency=backend/db     # Add a service dependency
codefly add library utils                                      # Add a library
codefly add library-dependency utils --dependency=core/models  # Add a library dependency
codefly add job db-migration --agent=go-grpc                   # Add a job
codefly add application web                                    # Add an application
codefly add application-dependency web --dependency=backend    # Add an application dependency
```

#### Product-Owned Selections and Approval

`codefly composition` selects nested released components, inspects differences and
stages exact selected executors. See [the complete command contract](composition-selections.md)
for required configuration identity flags and deployment guards.

`composition stage-build` stages Core-selected source builds using
`--build-requests` plus `--render-requests`, an explicit staging parent and
sandbox/principal choices. Build and render configuration share one identity.
It produces verified local build evidence, not a published artifact, signed
derived output or qualification. Keep both request files for subsequent commands.

`composition acquire` supports HTTPS and digest-addressed OCI objects using the
configured Docker registry credentials over TLS. It does not recursively acquire
layers or dependency source. `composition publish-build BUILD.json INPUTS.json
SIGNERS.json --expected-selection DIGEST --expected-build DIGEST --repository REGISTRY/REPOSITORY --output
ABSOLUTE_INPUTS.json` separately publishes verified build outputs by digest and
signs Core derived statements with the package owner's configured build authority.
It re-reads remote bytes, retains them with a digest-named OCI manifest reference,
and preserves existing output records. The output file can be passed directly to
`stage-render`; publication is not qualification or deployment. Registry policy
must preserve the retention roots against explicit deletion/expiration.

`codefly composition --workspace . inspect-local-target local` reads the explicit
local k3d environment's live target binding without applying resources. It needs
no selection/configuration flags. `--expected-identity DIGEST` rechecks an
independently retained binding and rejects namespace recreation or other identity
drift. The binding contains cluster routing/CA identity and actual namespace UIDs,
not credentials. A successful read is not qualification, fencing or deploy authority.

`configure-approval-authority CONFIG.json` explicitly installs a product-scoped
host policy/key/audience/target binding outside the workspace; replacing it needs
`--expected-digest` from `inspect-approval-authority`. This is trusted-local
administration, not a candidate or remote caller's policy choice.

After reviewing a Core admission record, `approve-admission` signs it only after
fresh admission under installed host policy:

```sh
codefly composition approve-admission inputs.json admission.json "$APPROVAL_FILE" \
  --expected-identity "$ADMISSION_ID" --expected-authority "$AUTHORITY_DIGEST" \
  --signing-key "$APPROVER_KEY_FILE" \
  --expires "$APPROVAL_EXPIRY" --render-requests requests.json --identity-key identity.key
codefly composition check-approval inputs.json "$APPROVAL_FILE" \
  --render-requests requests.json --identity-key identity.key
```

These commands neither execute qualification tests nor consume authorization for
deployment. Existing effect guards remain; approval is not observed running state.
`inspect-approval-use ID` reads a protected historical consumption record by the
`useIdentity` returned during approval inspection. A record establishes only
single-use consumption on this host, not deployment or health. There is no CLI
reserve/reset/retry command; the effect-owner consumption API is not yet wired
into a deployment adapter.

#### Module composition

`add module --source <path>` and `add module --worktree <owner/repo>@<ref>`
compose an out-of-repo module — the composition mode for multi-repo solutions
(a solution repo booting the host and runtime modules it does not own).

Composition splits **identity** (what to compose, portable) from **location**
(where it lives on this machine):

- The machine-specific location goes into a gitignored `codefly.local.yaml`
  overlay — a `resolve:` directive per module (`path:` for `--source`,
  `worktree: <owner/repo>@<ref>` for `--worktree`). It never lands in committed
  config, so a local source choice keeps `git status` clean.
- When the module is not yet composed, a portable identity (`source` +
  `version`, never a path) is added to `workspace.codefly.yaml`. `--source`
  derives the identity from the directory's `origin` remote when it is a git
  checkout.

At boot the [resolver](#codefly-run-solution) walks the precedence overlay
`path` → overlay `worktree` → overlay `pinned` → committed `path` → committed
identity, so the same committed config resolves on every worktree and in CI.
`codefly doctor workspace` flags an unresolved reference with the
`module_reference_unresolved` diagnostic.

To move a *single service* of a composed module rather than the whole module,
see [Overriding one service of a composed module](#overriding-one-service-of-a-composed-module).

A composition that **vendors** its sources — a git submodule per module, which
is how it gets a reviewable, reproducible pin — states both on one entry:
`source` + `version` records which module this is, and `path` points it at the
checkout already in the tree. No overlay is involved, so the same committed
config resolves for everyone.

**This route is unverified.** A committed `path` wins over `source`, so the
module resolves as a local checkout: nothing pulls the signed artifact, nothing
checks a signature or digest against `module-trust`, and the `version` beside it
is dropped rather than enforced against what the submodule is parked on. It
resolves even in a workspace with no `module-trust` block, where a bare
`source` + `version` reference would be refused. Use it when the submodule
pointer is what your review actually gates on; see #731.

```yaml
modules:
    - name: saas
      source: obin-ai/lodestar
      version: "0.0.62"
      path: platform/lodestar/modules/saas
```

A composition that vendors its sources — a submodule per producer repository —
can state the pin and the checkout satisfying it on the same committed entry,
with no `codefly.local.yaml` in play:

```yaml
modules:
    - name: saas
      source: obin-ai/lodestar
      version: "0.0.62"
      path: platform/lodestar/modules/saas
```

The resolver prefers the `path` and drops the `version` with it, so a submodule
parked past the tag the entry names runs as if it were that tag. `codefly
doctor workspace` compares the two and reports the divergence with the
`module_checkout_version_drift` diagnostic: it names the pin and what `git
describe --tags` says the checkout actually is.

The same comparison covers a pin satisfied by a machine-local checkout — a
committed `source` + `version` with the location in a `resolve.<name>.path`
overlay entry rather than a committed `path:`. Doctor asks the resolver where
each pin lands rather than re-deriving the precedence, so both spellings of
"this pin is satisfied by this checkout" are checked. Two resolutions are
deliberately excluded: a path a [resolution receipt](#resolution-receipts)
names is a materialization the CLI wrote, reported by `module_resolution_stale`
instead; and a `worktree:` directive names its own git ref, which — not the
pin's version — is what the user asked to run. It is a warning, not a failure
— vendoring a checkout deliberately ahead of its tag is normal while developing
the module, and only a problem unnoticed. Only a checkout that is its own
repository is compared (a `path:` inside the workspace's own working tree is
described by the workspace's tags), and a checkout with no reachable version
tag — a shallow CI submodule clone — is left alone rather than reported as a
version nothing established.

Only the two tag namespaces a module package is published under are consulted:
`v<version>` and `module-package/v<version>`. A vendored monorepo routinely
carries per-component and nightly tags as well, and `git describe` does not
prefer a version tag among tags on one commit — unrestricted, it would report a
checkout sitting exactly on its pin as drifted. A producer tagging outside both
conventions is therefore not checked rather than checked against the wrong tag.

A `pinned` (committed `source` + `version`) reference resolves through the
producer's verified module package rather than a git clone: `run` fetches the
signed release from GitHub, verifies its signature and artifact digest against
the workspace's `module-trust` policy, and extracts it into
`.codefly/cache/modules/<digest>/` — a moved or unsigned tag is rejected, not
silently trusted. Declare which repositories and signers are trusted in
`workspace.codefly.yaml`:

```yaml
module-trust:
  repositories:
    codefly/saas-starter: https://github.com/codefly-dev/module-saas-starter
  signers:
    codefly/saas-starter:
      <signature identity written into provenance.json>: <base64 ed25519 public key>
```

Signing authority is package-scoped. A key authorized for one package cannot
authorize another package's release, even when signer names match. Flat signer
maps are rejected; migrate each key under only the package IDs its owner authorizes.

A repository is looked up by its `source`; when `module-trust.repositories`
maps more than one package ID to the same repository, add `package: <id>` to
that module's entry in `workspace.codefly.yaml` to disambiguate.

A workspace with no `module-trust` block cannot verify a pinned module at all,
so `run` errors for every `source@version` reference instead of falling back
to an unverified clone; `codefly doctor workspace` flags this ahead of time
with the `module_trust_missing` diagnostic. The escape hatch is per module:
`resolve.<name>.git: true` in `codefly.local.yaml` keeps that one module on
the unverified git clone (`run` prints `unverified git clone for <name>` once
per run when it does). An overlay entry selects exactly one of
`path`/`worktree`/`pinned`/`git`, so `run` replaces that entry with the
clone's `path:` once it materializes the module, and says so when it does.
Setting `resolve.<name>.pinned: true` revokes the opt-out and returns the
module to verified resolution.

##### Committed git resolution

The overlay is machine-local, so it cannot answer the case where a module has
no signed package *for anyone*: every checkout of the workspace would need the
same opt-out written by hand, which is the per-machine step committed identity
exists to remove. A top-level `module-resolution` block declares it instead,
keyed by module name:

```yaml
module-resolution:
    saas: git

modules:
    - name: saas
      source: codefly-dev/module-saas-starter
      version: "0.0.68"
```

`git` — the only value the key takes today — means: resolve that module's
`source` at its `version` by cloning its git tag, **unverified by declaration**.
Nothing is signature- or digest-checked; the workspace is stating in committed,
reviewable config that it accepts that for this module, rather than each
developer accepting it again on their own machine. Every [command that
materializes](#which-commands-materialize) prints `unverified git clone for
<name> (declared in workspace.codefly.yaml)` when it materializes one, and
`codefly doctor workspace` reports it with the informational
`module_resolution_git` diagnostic beside the standing `module_unverified`
warning — writing the opt-out down does not make the clone checked.

Three things are errors naming the module, never ignored keys: any value other
than `git`; a name no composed module answers to; and writing `resolution: git`
on the module entry itself. The last is refused because the entry is the one
place the declaration cannot survive — a workspace carries unknown *top-level*
keys through a save (that is also how `module-trust` survives one), but a module
entry does not, so `codefly add module` would silently delete a per-entry
declaration from every other module in the file and you would commit that
deletion inside an unrelated change.

Precedence is unchanged: an overlay `resolve.<name>` entry still wins on the
machine that has one, so `path`/`worktree` keep pointing at a checkout you are
editing and `pinned: true` still tests verified resolution locally. Because
`run` must replace the directive with the path it produced, an overlay choice is
carried forward on its [receipt](#resolution-receipts) — `git` for the opt-out,
`overlay-verified` for the opt-in — and outranks the declaration until you
revoke it with the opposite directive or drop the module from the record. With
no overlay entry at all, the declaration behaves exactly as `git: true` would:
the same cache, the same receipt (recorded as mode `declared-git`), and the same
rewrite of the overlay to the materialized `path:`.

Leaving it is one edit. Once the producer publishes a signed module package and
`module-trust` names its repository and signer, **drop the module from
`module-resolution`**: it returns to verified resolution on the next run, with
nothing else about the entry changed and no stale record to clean up.

#### Overriding one service of a composed module

Sometimes the module is right and one service inside it is not: you want the
downstream solution to boot exactly as composed, except that `saas/accounts`
comes from the checkout you are editing, or from another published version.

`resolve.<module>.services.<service>` in `codefly.local.yaml` says so. Like
every overlay directive it is gitignored and machine-local — committed config
keeps naming the module as a whole, and only your machine runs the service from
somewhere else:

```yaml
resolve:
    saas:
        services:
            accounts:
                path: /Users/me/module-saas-starter/module/services/accounts
            auth-gateway:
                worktree: codefly-dev/module-saas-starter@feat/gateway-x
            telemetry:
                version: "0.0.66"
```

Each entry selects exactly one of:

| Key | Resolves to |
|-----|-------------|
| `path` | that directory (absolute, or relative to the overlay file) |
| `worktree` | `<owner/repo>@<ref>`, matched against your local checkouts exactly as a module `worktree:` is, then `<checkout>/<module>/services/<service>` |
| `version` | the module package at that version, pulled under `module-trust` like any pinned module, then its `services/<service>` |

The entry is orthogonal to the module directive. An entry carrying **only**
`services` is valid, and the module itself keeps resolving from committed
config exactly as if the entry were absent — so overriding one service never
implies pinning or relocating the module. Per service the precedence is overlay
`services.<svc>` → the module's own committed `services[].path` → the
`services/<name>` default.

`codefly override service` writes these entries for you, but the YAML is the
interface: hand-editing `codefly.local.yaml` is equally supported and is what
the resolver actually reads.

```bash
codefly override service saas/accounts --path ~/module-saas-starter/module/services/accounts
codefly override service saas/auth-gateway --worktree codefly-dev/module-saas-starter@feat/gateway-x
codefly override service saas/telemetry --version 0.0.66
codefly override service saas/accounts --clear   # back to the module's own copy
```

**The override must be the same service the module composed.** The module's
other services and its `interface` bind to the composed service's name, agent
and endpoints, so the directory is refused at load unless its
`service.codefly.yaml` declares the same `name`, the same `agent.name`, and at
least the endpoints the module declares (more is fine). The agent *version* may
differ — running a service at a different agent version is a reason to override
it — and is reported rather than refused. `run` announces each override once:

```
service saas/accounts resolves to /Users/me/… (overlay services.accounts.path, agent go-grpc 0.1.44 vs module 0.1.43)
```

A `version:` override is materialized by `run` exactly as a pinned module is —
same `module-trust` requirement, and the same escapes from it: `resolve.<module>.git:
true`, or the module's committed [`module-resolution`
entry](#committed-git-resolution).
An override never resolves differently from the module it belongs to, so
overriding one service of a declared-git module pulls that service's version
from the clone too, and leaves the rest of the module on the declaration. The
directive is then rewritten to the `path:` it produced, with a
[receipt](#resolution-receipts) keyed `<module>/<service>` recording the request
it answered.

A per-service override the CLI materialized for a module you later remove from
`workspace.codefly.yaml` is collected with that module; one you wrote yourself
is left in place, and `doctor` lists it again the moment the module is composed
back.

`codefly doctor workspace` reports `service_override_active` for each override
in effect (they are invisible in committed config, so the healthy ones are
listed too), `service_override_unresolved` when the directory is missing or the
module declares no such service, and `service_override_contract_drift` when the
directory is not the same service. It also reports
`gitops_dev_deployment_active`, a warning, for every
[dev deployment](#codefly-deploy-dev-moduleservice--the-dev-escape-hatch) a
rendered module tree carries. `codefly ci plan` and `codefly ci run`
**refuse** to run while any service override is in effect, because a CI plan is
a claim about the committed workspace; pass `--allow-service-overrides` to plan
against them deliberately, in which case each override's tree is part of the
cache identity like any other service directory.

There is deliberately no *committed* per-service version. The module is the unit
of identity and trust, and a committed per-service pin would fragment it and
bypass `module-trust`; the only committed spelling stays the module's own
`services[].path`.

This works on **every workspace layout**. On a flat (single-module) workspace the
module name is the workspace's own name:

```yaml
# workspace.codefly.yaml declares `name: solution`, `layout: flat`
resolve:
    solution:
        services:
            api:
                path: /Users/me/api-checkout
```

[`codefly run service --service-path`](#codefly-run-service-name) remains the
per-run spelling for the one service you are launching; an overlay entry is
durable and applies to every service, on any layout.
#### Moving the agent version of composed services

A composed module pins the agent each of its services runs on. When an agent
ships a fix (say `go-grpc` 0.1.46 → 0.1.47), a workspace that composes modules
by tagged version would otherwise have to wait for every module using that
agent to be re-tagged. A top-level `agent-overrides` block in
`workspace.codefly.yaml` moves the agent version in **committed** config
instead:

```yaml
agent-overrides:
    codefly.dev/go-grpc: 0.1.47   # <publisher>/<name>: exact version
```

Every composed service whose agent is that publisher and name runs against that
version — in `codefly run`, `codefly deploy gitops render` and every `codefly
ci` verb alike, because core applies it at the one place a composed module
loads a service. Only the version moves; the agent's publisher, name and kind
never do, and the module's own files are untouched. Each run or render states
it once per overridden agent:

```text
agent codefly.dev/go-grpc overridden to 0.1.47 by workspace.codefly.yaml (3 services; module pins: 0.1.46)
```

The command refuses, naming the key, a key that is not `<publisher>/<name>`, a
version that is not an exact semantic version (`0.1.47`, not `^0.1` or
`v0.1.47`), and a key no composed service's agent answers to — the typo surface
of a top-level map, as with `module-resolution`. The block is top level, not a
key on a module or service entry, for the same round-trip reason: core
preserves a top-level key it does not own across a load-and-save.
`codefly doctor workspace` lists each override in force as
`agent_override_active` (informational) and reports the refusals as
`agent_override_invalid`.

This is distinct from [overriding one service of a composed
module](#overriding-one-service-of-a-composed-module), which stays machine-local
because the module is the unit of trust and a committed per-service pin would
bypass `module-trust`. An agent is not module content: it is published and
versioned on its own, so pinning its version in committed config changes which
released agent interprets the module, not what the module contains, and the
module is still resolved and verified exactly as before.

Leaving it is one edit: once the modules pin that version themselves, drop the
key.

#### Which commands materialize

Materialization — pulling a composed pinned module into the module cache,
recording its [receipt](#resolution-receipts), pointing the overlay's
`resolve.<name>.path` at the result and gitignoring both files — is not a
`run`-only step. Every command that loads composed modules in order to act on
them materializes first, through one entry point, so a fresh checkout of a
workspace composed by identity works for each of them with no prior run:

- `run service`, `run job`, `run command`, `run solution` (which alone
  re-resolves a floating reference forward), and the dependency stacks the SDK
  spawns;
- `test service`, `test solution`, `test composition`;
- `deploy gitops render`, `snapshot`, `plan`, `publish`, `observe`, `rollback`,
  `deploy module`, `deploy service` and `deploy dev` — a CI render always starts from a fresh
  checkout, so it materializes exactly as `run` does;
- every `ci` verb (`plan`, `build`, `test`, `run`, `validate`, `push`,
  `deploy`);
- `generate client`, `sync solution-sdk` and the fixture listing, which read a
  composed module's tree.

All of them share the fast path: once a receipt answers the request the
workspace makes, a later command compares and pulls nothing, takes no lock and
rewrites no file, so a render after a run, or a second render, costs nothing.
All of them print the same `unverified git clone for <name>` warning when the
clone is unverified, and write the same overlay, receipt and `.gitignore`
entries.

The one command that reads composed modules and **never writes** is `codefly
doctor workspace`. When a declared module is not materialized on the machine it
reports `module_not_materialized` — "module X is declared but not materialized
yet" — with the commands that materialize it as the remediation, and skips the
service-scoped checks it cannot perform, rather than relaying core's refusal to
load the module as if the manifests were broken.

#### The module cache

`CODEFLY_MODULE_CACHE` names the directory composed modules are cached in. It
must be an absolute path; a leading `~` is expanded. Both caches follow it, so a
workspace repository that composes everything by identity holds no module bytes
at all:

| | default | under `CODEFLY_MODULE_CACHE=<root>` |
| --- | --- | --- |
| git clone | `<CODEFLY_HOME>/modules/<owner>/<repo>/<tag>/` | `<root>/<owner>/<repo>/<tag>/` |
| verified package | `<workspace>/.codefly/cache/modules/<digest>/` | `<root>/.packages/<digest>/` |

A module's optional `module:` subpath is joined after that, so with
`CODEFLY_MODULE_CACHE=~/development/vendors` a module composed as
`codefly-dev/module-saas-starter` at `0.0.68` is browsable at
`~/development/vendors/codefly-dev/module-saas-starter/v0.0.68/`. A verified
package stays addressed by digest under either root — that addressing is what
makes every cache hit checkable against what was verified — so it keeps its own
reserved subdirectory rather than sharing the browsable tree. A value that is
not usable as a root is an error rather than an ignored setting: falling back
silently would put the modules somewhere other than where you said, and you
would go looking where you asked. Resolution receipts record the path the module
actually landed at and every materializing command checks it against the root in
force now, so moving the root — or deleting the cache — re-materializes on the
next materializing command of any shape rather than leaving a module pointed at
where it used to be. A root you configure is yours, not the CLI's: a checkout of
your own inside it keeps its `resolve.<name>.path`, because there the CLI treats
only the path on a receipt as its own output.

Two names under the root are reserved for the CLI: `.packages/` holds the
digest-addressed verified packages, and `.staging/` holds in-flight clones being
promoted. Everything else is the browsable `<owner>/<repo>/<tag>/` layout. If you
point the root inside a git repository, `run` says so once — cached modules are
regenerable machine output and will otherwise sit there as untracked files. It
does not write a `.gitignore` for you: a root you configure may also hold module
checkouts of your own, and the blanket rule that would cover the cache would hide
those too. Ignore the two reserved names, or point the root outside the
repository.

#### Resolution receipts

Everything the CLI materializes — by any of the [commands that
do](#which-commands-materialize) — is recorded as a receipt in
`codefly.local.resolved.yaml` beside the overlay (gitignored like it). A
receipt binds the request it answered — canonical source, module subpath,
requested version or constraint — to what that request resolved to: the
materialization mode (`verified`, `git` for the overlay opt-out, or
`declared-git` for the committed `module-resolution` entry, and
`overlay-verified` for a `pinned: true` the CLI has consumed), the exact
resolved version,
the path, and for a verified package its artifact digest and commit.

```yaml
resolved:
    saas:
        source: codefly-dev/module-saas-starter
        requested: "0.1.0"
        mode: verified
        version: 0.1.0
        path: /path/to/workspace/.codefly/cache/modules/<digest>
        digest: sha256:…
        commit: …
```

That record is the boundary between a checkout you manage and output the CLI
manages. An overlay `path:` matching a receipt is one `run` wrote and may
refresh; one that does not is you editing the module in place, and `run` never
touches it. It is also what keeps a module on its git opt-out after the
directive has been replaced by a path, and what lets `codefly doctor
workspace` report the module as unverified. A committed `module-resolution`
entry is the exception: it is recorded as `declared-git` but never read back to
decide the mode, because the workspace manifest still says it — and stops saying
it the moment the key is dropped.

Because the receipt names the request, a materialization can be checked
against the request being made *now*. When a module's requested version (or
source) changes and the new request cannot be resolved, `run` does **not** keep
running the previous version: it drops the materialized path from the overlay
and fails with both versions named —

```
module <saas>: cannot resolve requested version v9.9.9: …;
its previously resolved version v0.0.1 at … no longer answers that request
and has been dropped from codefly.local.yaml
```

The cached files may remain on disk, but nothing routes the composed reference
to them any more, so a failed upgrade cannot silently keep shipping the old
module. A module the CLI has *never* materialized is unaffected: it is left
unresolved with a warning, and only a target whose dependency closure needs it
fails. `codefly doctor workspace` reports a materialization that answers a
different request than the workspace now makes with the
`module_resolution_stale` diagnostic, before a run is attempted. A record
written before receipts existed is read for its git opt-out and its path, but
records no request, so it cannot vouch for its checkout under a request that
fails.

An unchanged request still resolves offline: the git clone cache is
version-keyed and the verified package cache is digest-checked on every reuse,
so a warmed-up workspace boots with the producer unreachable.

**`add runnable`:**

```sh
codefly add runnable word-count --agent=python:0.0.1 --handler=handler.py --json
```

`--agent` and `--handler` are required. `--module` selects the owner, defaulting
to the current module. Creation uses the agent over gRPC; edit the generated
contract and handler before building. See [Runnables](runnable.md).

**`add service` flags:**

| Flag | Description |
|------|-------------|
| `--agent` | Agent type (required). Examples: `go-grpc`, `python-grpc`, `nextjs`, `krakend` |

### `codefly override service <module>/<service>`

Run one service of a composed module from somewhere else on this machine,
leaving the rest of the module — and every committed file — untouched. It edits
the gitignored `codefly.local.yaml` overlay and nothing else.

```bash
codefly override service saas/accounts --path ~/module-saas-starter/module/services/accounts
codefly override service saas/auth-gateway --worktree codefly-dev/module-saas-starter@feat/gateway-x
codefly override service saas/telemetry --version 0.0.66
codefly override service saas/accounts --clear
```

| Flag | Description |
|------|-------------|
| `--path` | Directory holding the service |
| `--worktree` | `<owner/repo>@<ref>` of a local checkout to take the service from |
| `--version` | Module package version to take the service from |
| `--clear` | Remove the override and go back to the module's own copy |

Exactly one is required. The command prints what the override resolved to, so a
`--worktree` matching no local checkout fails here rather than at the next run.
See [Overriding one service of a composed module](#overriding-one-service-of-a-composed-module)
for the overlay schema, the contract the override must satisfy, and the
`doctor`/`ci` behaviour.

### `codefly delete`

Remove resources from the workspace.

```bash
codefly delete module backend
codefly delete service api
```

### `codefly update`

Move the agents services run on, or a repository's dependencies.

```bash
codefly update workspace                                              # every service to its latest compatible release
codefly update workspace --agent-override codefly.dev/go-grpc=0.1.47-dev.abc123def456
codefly update service api                                            # one service to its latest compatible release
codefly update service api --agent-version 0.1.47-dev.abc123def456    # pin one service exactly
codefly update deps                                                   # Go dependencies under the current directory
```

Without flags, `update workspace` and `update service` move to the agent's
**latest release** — never a prerelease. Pinning an exact version, a
[dev build](#codefly-publish-dev) included, takes one of two flags, one per
place a pin can live:

- `update workspace --agent-override <publisher>/<name>=<version>` (repeatable)
  writes the top-level [`agent-overrides`](#moving-the-agent-version-of-composed-services)
  block of `workspace.codefly.yaml`, and moves nothing else. It is the pin for
  services of **composed** modules: the modules' own files stay untouched.
  Entries not named are kept; the file is written through core's workspace
  saver, which re-emits the file in canonical form (comments are not kept, as
  with `codefly add module`).
- `update service [<service>] --agent-version <version>` rewrites the
  `agent.version` of the service's own `service.codefly.yaml`, and only that
  token — comments, formatting and every other key are kept byte-for-byte. It is
  the pin for a service the workspace **authors**; a service of a module the
  workspace composes by `source` is refused, pointing at `--agent-override`.

Both refuse, before writing anything, a version that is not an exact semantic
version (`0.1.47` or `0.1.47-dev.abc123def456`; not `latest`, `^0.1`, `0.1` or
`v0.1.47`), and a version that is not published: the candidate agent is
downloaded and started to check its protocol, exactly as a latest-release
update admits one. `--agent-override` also refuses a key no composed service's
agent answers to, the same check every run applies to the block.

### `codefly sync`

Synchronize service configurations with dependencies.

```bash
codefly sync service api                # Sync a service with its dependencies
codefly sync library-dependencies       # Sync library dependencies
codefly sync solution-sdk --language python --apply-dependencies  # Aggregate api.consumes into one solution SDK
```

#### sync solution-sdk

```bash
codefly sync solution-sdk --language python                      # Generate/refresh the solution's library
codefly sync solution-sdk --language go,python --apply-dependencies  # Also wire up service-dependencies
codefly sync solution-sdk --language python --check               # CI drift gate
```

Run from a solution workspace (`workspace.codefly.yaml` plus a module whose
`service-entry` is the solution backend, as in `core-solutions/solutions/*`).
Reads the single declaration `api.consumes` in `solution.codefly.yaml` (the
solution manifest core's `solution/manifest` package defines) and aggregates
every module-bound entry — `{module, service, endpoint, version, services, as}`
— into **one** library, `libraries/<solution module>-sdk/`, generated from the
composed module packages' API contract catalogs (the same generator
`generate client` uses, aggregating N contract entries into one library
instead of one). This is the **one declaration, three outputs** rule: the same
`api.consumes` entries drive the generated SDK, the runtime
`service-dependencies` (below), and the `SolutionBundle` `platform.consumes`
lodestar derives from the manifest.

| Flag | Description |
|------|-------------|
| `--language` | Required, repeatable or comma-separated: `go`, `typescript`, `python` |
| `--check` | Do not write; exit 1 if the library or `service-dependencies` are out of date |
| `--apply-dependencies` | Add any `service-dependencies` entry missing for a bound consume entry to the entry service's `service.codefly.yaml`; without it, missing entries are printed as a warning. Existing entries are never removed. |

Each bound entry is resolved against the workspace's own composition: the
composed module named by `module` (workspace `add module` first), its
`module.package.codefly.yaml` and `contracts/api/catalog.codefly.json` (from
`generate contracts`), `version` checked as a semver constraint against the
composed package version, and `services` validated against the endpoint's
declared protobuf services. Two entries whose contracts share a proto package
at different digests (a diamond) are rejected.

If a `solution-sdk.yaml` (the pre-`api.consumes` declaration this command
replaces) sits next to the manifest, its dependencies must be migrated into
`api.consumes` by hand — `source.repo`/`ref` there has no equivalent, since the
workspace's own composition pin replaces it — and the file deleted; the
vendored `_sdk/` directory it produced is replaced by `libraries/<name>-sdk/`.

Use `codefly sync library-dependencies --service <entry service>` (printed as
the next step) to link the generated library into the entry service locally.

### `codefly environment`

Declare and inspect the deploy environments in `workspace.codefly.yaml`.

```bash
codefly environment import <env> --coordinate-contract <file|-> [--namespace <ns>] [--dry-run]
codefly environment show <env> [--json]
```

#### `codefly environment import`

Import a producer-independent `codefly/coordinate/v1` descriptor whose `environment`
contains Codefly's environment fields. Producers supply resolved endpoints,
ports, secret references, runtime identities and delivery paths. The CLI does
not interpret a provider's infrastructure inventory or infer service aliases.

```bash
codefly environment import production --coordinate-contract coordinate.json --dry-run
codefly environment import production --coordinate-contract coordinate.json
```

The requested environment and namespace must match the descriptor. The namespace
comes from the producer declaration; `--namespace` asserts that declared target.
Re-import refuses to change an existing namespace while retaining its identity,
secrets and delivery declarations. This flag does not rewrite the
contract's delivery paths or secret references. The superseded `codefly/cell/v1`
and `codefly/cell/v2` descriptors are rejected; their producers must emit explicit
`codefly/coordinate/v1` declarations. `--cell-contract` remains accepted as the
former spelling of `--coordinate-contract` for one release.

Declared fields replace their exact named values. Maps merge by explicit key;
omitted fields and unrelated entries remain intact. Explicit empty maps, empty
sequences and nulls clear the corresponding field where its schema permits.
There is no special `store` alias, single-database limit, default secret path or
ignored producer extension. Unknown fields and capabilities fail validation.

An import re-serializes only the selected environment item. Surrounding workspace
bytes remain unchanged; a provenance comment records the coordinate and
import time. `--dry-run` prints the diff without writing. After a write, workspace
readiness validation runs for the selected environment; when it reports the
workspace is not ready, the command prints the diagnostics and exits non-zero.
The merged file is still written, so the diagnostics can be read against it.

#### `codefly environment show`

Print the resolved `resources.Environment` as YAML (default) or `--json`.

```bash
codefly environment show azure
codefly environment show azure --json
```

### `codefly list`

List workspace resources.

```bash
codefly list project      # List projects in workspace
codefly list module       # List modules (alias: application)
codefly list libraries    # List workspace libraries (--remote for published versions, --json for machine output)
codefly list jobs         # List jobs
codefly list runnables    # List runnables (--module to scope, --json for machine output)
```

See [Runnables](runnable.md) for what a runnable is and which parts of its
lifecycle the CLI implements today.

### `codefly show runnable <name>`

Show one runnable's contract, execution bounds and dependency resolution.

```bash
codefly show runnable word-count
codefly show runnable backend/word-count --json
codefly show runnable word-count --version=0.2.0
```

Takes `module/name`, or a bare name when it is unambiguous across modules and
across versions; when a name stands for several releases the command lists them
and `--version` selects one. It loads through core's strict loader, so an
invalid declaration is reported as a load error rather than rendered partially.
Each declared service dependency is reported as resolved or unresolved against
what the workspace declares, using core's own binding rules for runnables
(endpoints match by name, because that is all a runnable's wire form carries)
— reachability and credential resolution belong to whoever installs and
launches a binding, not to this command. An unresolved
dependency is reported, not fatal: the command exits 0, and unattended callers
gate on `--json` and each dependency's `resolved` field.

### `codefly show network`

Show every service's endpoints with the deterministic native address each binds
to on a local run, and the dependency endpoints each consumes. Nothing is started.

```bash
codefly show network
codefly show network --naming-scope ci-42
codefly show network --json --env staging
codefly show network --json --env staging --rendered
```

`--json` adds each endpoint's `deployed_port`: its in-cluster port in the
environment `--env` names (default `local`, as `deploy gitops render`), the same
allocation the render emits. It depends on the environment: an external endpoint
that resolves to a public host gets no cluster port, and a service the
environment replaces with a managed one (`"managed": true`) has none at all, so
`deployed_port` is absent there. `native` is absent for an external endpoint.
Module generators read ports from here rather than declaring them.

```json
{
  "workspace": "acme",
  "naming_scope": "",
  "environment": "staging",
  "services": [
    {
      "service": "saas/accounts",
      "module": "saas",
      "name": "accounts",
      "endpoints": [
        {"name": "authority", "api": "grpc", "visibility": "private",
         "native": "localhost:28113", "deployed_port": 52893},
        {"name": "grpc", "api": "grpc", "visibility": "private",
         "native": "localhost:18883", "deployed_port": 9090}
      ],
      "dependencies": [{"service": "saas/store", "endpoints": ["tcp"]}]
    }
  ]
}
```

`module` is the name the workspace composes the module under, which the named
ports are hashed on: the same `authority` endpoint composed as `saas-starter`
deploys on 6003. The text output, without `--json`, is unchanged and takes no
environment.

`--rendered` (with `--json`) adds each endpoint's `container_port`: the port its
pods listen on. The agent decides it when it renders, so it is not known before
a render and is read from the render committed at `deployments/modules/<module>`
(recorded per service as `render`, relative to the workspace), exactly as
`deploy gitops render` reads it to check `spec.deployment.endpoint-ports`: the
`targetPort` of the rendered Service port that publishes the endpoint's
in-cluster port. A service whose module has no render, or whose render has no
unit for it (a managed service), carries no `render` and no `container_port`; an
endpoint no rendered Service publishes carries none either. A render for an
environment other than `--env` is an error. The render output itself is not
extended with a ports file: the manifests are the record, and this is its one
reader.

```json
{"name": "tcp", "api": "tcp", "visibility": "external",
 "native": "localhost:21422", "deployed_port": 80, "container_port": 5432}
```

### `codefly show fixtures`

List the fixtures the workspace's composed packages declare, with the principals
each one seeds.

```bash
codefly show fixtures
codefly show fixtures --json
```

A fixture names the state a composed host boots with under `CODEFLY__FIXTURE`,
so these are exactly the names [`codefly run solution --fixture`](#codefly-run-solution)
accepts. Each principal is reported by id, email and role — `role` is the lookup
key a solution test resolves an identity by, instead of hardcoding a seeded
login. Seed tokens are declared in the manifest but are not printed.

Fixtures are collected across every composed package and sorted by name. A module
that composes no package declares none and is skipped.

A package that cannot be read, and a name two packages both declare, are reported
as problems *after* the listing, and the command exits non-zero. The listing is
not suppressed: a collision is what you run this command to diagnose, so it names
what each package declares rather than withholding the data needed to act on it.
This command resolves each composed module in order to read its manifest, and
[materializes](#which-commands-materialize) a pinned module exactly as the run
path does when it is not materialized yet — into the module cache, with the
receipt and overlay entry written and gitignored — so this is not a purely
offline command the first time a pinned module is seen, and it writes
`codefly.local.yaml` on that first sight like every other materializing command.
A module that cannot be resolved is reported as a problem, not silently dropped
from the listing.

A name two packages declare *differently* is a collision. Identical declarations
of one name — what a package composed under two module references produces — name
one seed and are listed once.

---

## Setup

### `codefly init workspace [name]`

Create a new workspace in the current directory.

```bash
codefly init workspace my-project
codefly init workspace my-project --with-default  # Use default configuration
```

### `codefly login`

Authenticate with the codefly platform.

```bash
codefly login
```

### `codefly publish [patch|minor|major|beta]`

Release the repository in the current directory: bump its manifest version, land
the bump on `main` through a release pull request, then tag the commit `main`
ends up carrying and push the tag. One flow for every codefly-dev repository; the
mode is detected from the manifest present (`agent.codefly.yaml` → agent,
`version/info.codefly.yaml` → core, `pkg/cli/info.yaml` → cli,
`info.codefly.yaml` → standalone module).

```bash
codefly publish              # patch bump
codefly publish minor
codefly publish --dry-run    # show the plan, change nothing
```

Pre-flight is strict and aborts with no side effects: clean tree, on `main`, in
sync with `origin/main`, target tag free locally and remotely. Nothing is ever
pushed with `--force`. A service-agent repository additionally runs release-grade
agent CI against the bumped version, then creates the GitHub release, uploads the
loader archives and SBOMs, and verifies each resolves through the install URL.
That final read uses the publisher's authenticated GitHub client through Core's
agent-download path, so private release archives are verified without requiring
public access. Asset-CDN requests do not receive the GitHub API credential.
Installing private agents requires `GITHUB_TOKEN` or `GH_TOKEN` in the install
process; public agent downloads continue to work without either.
A module-agent repository publishes only the immutable Git tag. If the release
pull request merged but the tag push failed, re-run: the untagged release commit
is recognised and finished rather than bumped again.

Releasing does not update what the repository *pins*. Move a dependency first —
`codefly agent deps --dir <agent> --pin vX.Y.Z` for an agent's Core pin,
`go get github.com/codefly-dev/core@vX.Y.Z && go mod tidy` in the CLI — commit
that on `main`, and then publish. See
[docs/runbooks/release-the-fleet.md](runbooks/release-the-fleet.md) for the order.

#### Releasing from GitHub instead of a laptop

A release does not need a laptop that stays open. An agent repository adds one
caller workflow, `.github/workflows/publish.yml`, that runs the reusable
[`publish-agent.yml`](../.github/workflows/publish-agent.yml) from this
repository:

```yaml
on: {workflow_dispatch: {inputs: {bump: {type: choice, options: [patch, minor, major], default: patch}}}}
jobs:
  publish:
    uses: codefly-dev/cli/.github/workflows/publish-agent.yml@vX.Y.Z
    with: {bump: "${{ inputs.bump }}", codefly-version: vX.Y.Z}
    secrets: {release-token: "${{ secrets.GH_PAT }}"}
```

The job refuses anything but `main`, installs the pinned CLI release on an
`ubuntu-latest` runner after verifying `checksums.txt` against its signature and
the pinned release certificate, and runs `codefly publish <bump> --ci` on the
runner's Docker. Pin `codefly-version` (and the `@vX.Y.Z` ref) to a release
that carries `--ci`.

`release-token` is required: GitHub starts no workflow for a push, pull request
or tag made with a workflow's own `GITHUB_TOKEN`, so the release pull request
would never get the CI `publish` waits on, and the tag would never start the
repository's own release workflow. Pass a GitHub App installation token or a
PAT with contents and pull-requests write on the repository.

Fire it with one click in the Actions tab, with
`gh workflow run publish.yml -R <owner>/<repo> -f bump=patch`, or from the
repository checkout:

```bash
codefly publish --remote              # dispatch on main, print the run URL, return
codefly publish minor --remote --wait # follow the run to its conclusion
codefly publish --remote --workflow release-agent.yml   # a differently named caller
```

`--remote` accepts `patch`, `minor` or `major`; a beta is cut locally.

`--ci` (on by default when `CI=true`) is what makes the flow runner-safe, and
works on any CI system:

- the release commit and tag are made as `github-actions[bot]` when git has no
  identity, with commit and tag signing off. `main` still receives the commit
  GitHub creates and signs when it squash-merges the release pull request; the
  annotated tag naming it is unsigned. A release that must sign its tag refuses
  CI mode rather than drop the signature.
- when no CI at all registers on the release pull request, the merged commit,
  or — for `release.owner: workflow` — the tag, within 10 minutes, the release
  fails and says why, instead of waiting out its whole budget. This check is on
  outside CI mode too.

Independently of `--ci`, a service agent whose `release.owner` is `workflow`
no longer needs a darwin/arm64 host: its own release workflow builds and
uploads every loader platform, and `publish` verifies those archives against
the checksums that workflow published. Release-grade agent CI still runs on the
publishing host, so on a linux/amd64 runner that qualification is the
linux/amd64 build only. An agent with `release.owner: cli` uploads the archives
itself and still requires a host that builds every platform.

### `codefly publish all [patch|minor|major]`

Discover every git repository under the workspace that carries a codefly
manifest and run the same flow on each, in dependency order: core → cli →
standalone modules → agents.

```bash
codefly publish all              # patch-bump every repository
codefly publish all --dry-run    # print the full plan, change nothing
codefly publish all --root DIR   # workspace root (default: nearest go.work, else cwd)
codefly publish all --remote     # release each repository on GitHub, in order
```

With `--remote`, nothing is built or pushed from this machine. Every
repository must carry the caller workflow above (`--workflow` names it;
default `publish.yml`), which is checked for all of them before the first
dispatch. Each release is then dispatched in dependency order and must conclude
successfully before the next repository's is dispatched, so a consumer is never
released against a dependency that did not ship. The machine running it must
stay up for the whole sequence; a single `codefly publish --remote` does not.

The run is atomic at the pre-flight boundary: every repository is validated
first and any failure aborts before a single tag is pushed. Publication then
proceeds sequentially and stops at the first real failure, reporting what already
shipped. It sweeps *every* manifest-bearing repository under the root — use
per-repository `codefly publish` when only some of the fleet should move.

### `codefly publish re-tag`

Move the current manifest tag to `HEAD` without rewriting `main`. For a release
whose tag landed on the wrong commit; it never force-pushes `main`.

### `codefly publish dev`

Publish an agent build **for iteration**, without a release. Run it in an agent
repository on any branch:

```bash
codefly publish dev                 # build HEAD and publish it as a dev build
codefly publish dev --dry-run       # print the dev version, build and publish nothing
codefly publish dev --allow-dirty   # publish the working tree with uncommitted changes
```

A dev build is built exactly as `codefly publish` builds a release
(release-grade `codefly agent ci`, the same loader archives and SBOMs) but is
published under a **non-semantic dev version** derived from the commit:

```text
<current-version>-dev.<12-char-sha>     e.g. 0.1.47-dev.abc123def456
```

It is a semver prerelease, so every resolver parses it, and semver ranks it
*below* the release it was built from (`0.1.47-dev.… < 0.1.47`): a dev build
can never collide with or outrank a real release. Nothing is bumped —
`agent.codefly.yaml` keeps its version (the build sees the dev version only
while it runs), no release pull request is opened, `main` is never touched.
The only ref created is the tag `v<dev-version>` at `HEAD`, and the assets go
where release assets go, as a GitHub **prerelease that is never marked Latest**
— so `version: latest` never resolves to a dev build.

- A dirty tree is refused unless `--allow-dirty`. Published assets are
  immutable, so publishing different bytes again under the same commit is
  refused; commit to get a new dev version.
- An agent whose releases a workflow publishes (`release.owner: workflow`) is
  built by that workflow from the pushed tag. `--allow-dirty` is refused for
  it, and its `.goreleaser.yaml` must set `release: {prerelease: auto}` so the
  dev tag is published as a prerelease — the command refuses before pushing
  anything otherwise, and checks the published release is a prerelease after.

The command prints the exact spelling to consume the build with. Either override
the agent for the whole workspace (`workspace.codefly.yaml`):

```yaml
agent-overrides:
  codefly.dev/go-grpc: 0.1.47-dev.abc123def456
```

or pin one service's `agent.version` in its `service.codefly.yaml`. Both are
written by a command rather than by hand — `codefly update workspace
--agent-override <publisher>/<name>=<version>` and `codefly update service
<service> --agent-version <version>` (see [`codefly update`](#codefly-update)).

**Which of the two survives a merge.** `codefly ci prerelease` refuses a dev version in a
service's `agent.version` on the default branch, because a tag cut from that branch would ship a
module pinning an unreleased agent — which is exactly what `module-saas-starter` v0.0.85 and
`module-runtime` v0.1.5 did. Use a service pin on a branch, for as long as the branch lives. The
committed route is the workspace-wide `agent-overrides` entry **with a label**: a comment naming
the issue it stands in for. Even that is refused by `codefly ci prerelease --release`, so a dev
build cannot survive into a tag. See [prerelease-gate.md](prerelease-gate.md).
`codefly agent list` reports such a pin as `dev build of <release>`, not as
behind that release: it was built on top of it.
`codefly doctor workspace` warns (`agent_dev_build`) about every service running
a dev build. Dev builds are for iteration only: `codefly publish patch` remains
the release path, and a workspace should pin the released version before it
ships.

### `codefly publish library <name>`

Publish a workspace library's language exports (`codefly add library`) to the durable stores configured under the workspace's `libraries.publish` block — a GitHub repository tagged at the version for `go`/`python`, an npm-compatible registry for `typescript`. Published versions are immutable: an identical retry adopts the existing version, while different bytes require a version bump.
A publish never creates a repository on its own. Pass `--create-missing-repository` to let it, and the repository is **private** unless you also pass `--public-repository` (which requires `--create-missing-repository`: an existing repository's visibility is never changed by a publish). Both are deliberate: a generated client's bindings carry every message in its contract, not only the services its facade exposes, so a public repository discloses a module's whole surface. Where a library may be published, and whether that destination is public, is an infrastructure fact — prefer taking it from the platform's coordinate contract over hardcoding it.

`--create-missing-repository` needs a GitHub credential with repository-creation scope (`GITHUB_TOKEN`, or `gh auth login`); without one the publish fails naming what is missing rather than silently skipping the creation. When the repository it publishes into is private, the reported install hint carries `GOPRIVATE` for the owner's namespace, because a bare `go get` resolves through the public module proxy and cannot see a private repository. If the repository already exists and is public while a private one was requested, the publish proceeds and warns: its visibility is not a publish's to rewrite.

```bash
codefly publish library authkit --dry-run           # show what would be published, touch nothing
codefly publish library authkit --version 1.2.0
codefly publish library authkit --language go,python
```

Configure `workspace.codefly.yaml`:

```yaml
libraries:
  publish:
    go: {owner: codefly-dev}                                              # github.com/<owner>/<name>-go
    typescript: {registry: https://npm.pkg.github.com, scope: "@codefly-dev"}
    python: {owner: codefly-dev}                                          # github.com/<owner>/<name>-python
```

Publish credentials (`GITHUB_TOKEN`/`GH_TOKEN` or `gh auth token`; `NPM_TOKEN`/`NODE_AUTH_TOKEN` or, for `npm.pkg.github.com`, `gh auth token`) belong in release CI, never in a runtime.

If a language export publishes and a later one in the same run fails, the command stops and reports which languages already published — those versions are immutable and are never rolled back.

### `codefly publish clients [module]`

Generate and publish a client library for every API contract the module's `interface:` block exports (`codefly generate contracts`), in every language the contract kind supports — `go`, `typescript` and `python` for protobuf, `go` and `typescript` for OpenAPI. Each endpoint becomes one codefly library named `<module>-<service>-<endpoint>-client`, so two contract endpoints on one service never contend for the same immutable package version. Libraries are generated from the committed package contract and proto sources and published at the **module package version** through the workspace's `libraries.publish` stores. This is how a module's consumers — other modules, solutions — get its client: an immutable published handle, never a vendored copy of generated code.

```bash
codefly publish clients saas-starter --dry-run          # plan + identities; no toolchain, no network
codefly publish clients saas-starter --check            # CI gate: every client of this version is recorded as published
codefly publish clients saas-starter                    # generate (Docker) and publish what is still missing
codefly publish clients saas-starter --language go      # one language only
codefly publish clients saas-starter --output ./libraries   # keep the generated libraries instead of a temp dir
```

Like `publish library`, this creates no repository unless `--create-missing-repository` is passed, and creates a private one unless `--public-repository` is passed as well. Publishing is not required to consume a module's clients: a Codefly workspace carries the proto companion the CLI pin resolves, so `generate client` reproduces them from the module package's contract, and a per-language SDK repository that carries the generated tree distributes them under its own tag.

An endpoint shapes its clients in the optional, publish-owned `clients.codefly.yaml`. Keeping this policy outside `module.codefly.yaml` prevents synchronization of the generated `interface:` block from replacing it. The schema and endpoint identity are required; every policy field is optional, and unknown fields are rejected:

```yaml
schema: codefly/module-clients-config/v1
endpoints:
  - service: accounts
    endpoint: connect
    languages: [go, typescript]                 # default: every language the contract kind supports
    services: [AuditService, WebhookService]    # facade subset (protobuf only); default: the whole contract
    publish: false                              # opt this endpoint out
```

`services:` decides what a consumer can reach through the facade; the bindings still carry the full contract, and a TypeScript `_pb` module is one proto file, so keep a service that must not ship in its own `.proto`.

`contracts/clients.codefly.json` records, for the current package version, every published export (library, language, import path, ref/digest, install hint). It is written immediately after each language publishes — published versions are immutable, so a partial failure is checkpointed before the next language starts — and it must be committed. Concurrent publishing commands for one module are serialized. If a process exits after a GitHub tag lands but before the checkpoint, the retry adopts the tag only when its content digest is identical. Its rules:

- Same package version, same endpoint, contract digest, facade selection, store identity and complete publication evidence → skipped.
- Same package version with a different endpoint, contract digest, or facade selection → refused: generation inputs moved without a release; bump `version:` in `module.package.codefly.yaml` first (a bump starts the manifest over; the stores keep the history).
- `--check` validates the complete recorded identities and evidence offline and is the gate release CI should run after publishing.

The exported package is the published unit: before generating, the command requires `module.package.codefly.yaml`, the catalog, and every committed contract artifact to agree on package identity, version, path, and digest. Run `codefly generate contracts` and commit first when that validation fails. Go and Python publish into `github.com/<owner>/<name>-go|-python`; TypeScript publishes `<scope>/<name>` to the configured npm registry.

### `codefly install library <name>@<constraint>`

Resolve the highest published version of a library export satisfying a semantic version constraint, via the store `codefly publish library` published it to, and print the durable install handle (import path, install command, ref, digest). With `--destination`, also runs the language's native install command there (`go get`, `npm install`, or `pip install`). It never vendors source — the registry decision is that consumers pin an immutable handle, not a local copy.

```bash
codefly install library authkit@^1.0.0 --language go
codefly install library authkit@^1.0.0 --language go --destination ./services/api
```

---

## Development

### `codefly agent`

Manage service agents (the gRPC plugin processes that implement service operations).

```bash
codefly agent info service --agent=<name>:<version>   # Show a service agent's reported capabilities
codefly agent info runnable --agent=<name>:<version>  # Show a runnable agent's reported capabilities
codefly agent install <name>[:<version>]              # Download a released agent (--kind=runnable for a language agent)
codefly agent generate [agent-name]  # Generate agent scaffolding
codefly agent build [agent-name]     # Build an agent binary
codefly agent ci                     # Run source, release, generated-service, and drift gates
codefly agent deps --pin vX.Y.Z      # Pin Core across root, base and generated factory locks
codefly agent deps --pin vX.Y.Z --dependency github.com/codefly-dev/sdk-go@vA.B.C
```

`agent deps --dependency` is repeatable and requires `--pin`. It updates only
modules that already require the selected library, then verifies their standalone
builds and regenerates factory locks. After pinning, every owned module (root
and base fixtures; factory templates are byte copies of their base lock) must pass
`go mod tidy -diff` with `GOWORK=off` and no `GOFLAGS` — the tidy gate agent CI
runs — or the verb fails. Unknown selections, a failed build or an untidy lock
leave the original locks intact. Re-running `agent deps --pin` after a manual
`go get` is how to bring the locks back to tidy. See [the release runbook](runbooks/release-the-fleet.md).

`--kind` on `codefly agent install` selects the registered agent kind
(`service`, the default, or `runnable`). A runnable language agent is named by
its language and resolves to the `runnable-<language>` repository and executable
through the same machinery service agents use — the CLI has no per-language
branch.

```bash
codefly agent install python:0.0.1 --kind=runnable
codefly agent info runnable --agent=python:0.0.1
```

`codefly agent ci` is the provider-neutral agent-repository gate. It uses an
isolated Codefly home, builds the local agent, records binary and CycloneDX
hashes, runs the complete workspace CI gate against a conformance workspace, and
verifies that validation did not change the agent repository. The default
report/artifact directory is `.codefly/agent-ci`.

A polyglot source-tag repository whose root is not itself a language project
declares the exact in-repository source root and source agent instead of relying
on recursive extension detection:

```yaml
source:
  directory: modules/example/services/api/code
  agent: codefly.dev/go:0.0.49
```

The directory must remain inside the repository after symlink resolution, and
an omitted agent version resolves `latest`. Source tests, packaging, and audit
all use that selection, checking compatibility from the running agent rather
than its release. Self-hosted packagers declare `source.agent: self` and their
own bootstrap command; see [runtime agent compatibility](agent-compatibility.md).

Conformance defaults to scaffolding a fresh service through `Builder.Create`.
Attach-only generic agents whose `Builder.Create` intentionally declines to
generate a project template (for example `codefly.dev/python`), or agents whose
conformance needs a complete dependency graph, declare an
attach-existing-source conformance mode in `agent.codefly.yaml` and ship a
fixture workspace instead:

```yaml
conformance:
  mode: attach-existing-source
  fixture: ./conformance/fixture   # a Codefly workspace whose service pins the agent at version: latest
```

CI copies that fixture out of the repository and runs the Code/Runtime/Tooling
gate against it. A malformed declaration or a fixture missing
`workspace.codefly.yaml` fails the conformance stage rather than skipping it.
The candidate uses `latest` or its exact version in that fixture. Other service
agents require exact canonical release versions and are installed through
`codefly agent install` into the isolated conformance home before the gate.
An operator's installed agents cannot satisfy or override those fixture pins.

Runnable agents must declare `conformance.mode: runnable-create` or
`runnable-package`, plus `conformance.handler` (a relative handler filename).
Both modes create a fresh Runnable through the exact built candidate in the
isolated home. Package mode additionally runs `build runnable`, including its
archive and descriptor verification. No language name selects a mode. Publishing
a Runnable never passes `--skip-conformance`; source-only generation is not
evidence of native packaging or invocation support.

Toolbox agents declare `conformance.mode: toolbox-session` and a fixture naming
the operations the host must serve and the one it must refuse:

```yaml
conformance:
  mode: toolbox-session
  fixture: ./conformance/operations.yaml
```

```yaml
# conformance/operations.yaml
operations:
  - name: describe-identity
    tool: git.status            # a tool toolbox.codefly.yaml declares
    arguments:
      path: .
  - name: refused-write
    tool: git.commit
    denied: true                # the host policy must refuse this one
```

CI launches the installed artifact through Core's toolbox session under the
sandbox and permission ceiling its own `toolbox.codefly.yaml` declares. The host
owns the principal, the policy decision point and the session scope; the owner
owns the operations. Because production admission requires a non-empty sandbox
declaration, and Core refuses to launch a sandbox-declaring plugin with no
enforcing backend, a toolbox release must be qualified on a host that has one —
`bwrap` on Linux, `sandbox-exec` (built in) on macOS. On a host without it the
stage fails rather than qualifying the artifact unconfined. A declared permission the running binary does not serve, an
operation naming a tool it does not advertise, a refusal the host served, a
refusal that reached the plugin, or a session that does not release its process
fails the stage. A declared permission is read with Core's own matcher, so a
wildcard ceiling such as `git.*` is qualified the way catalog admission reads
it.

Every fixture must declare at least one operation the host serves **and** at
least one it refuses: a release qualified only on its success path has not
shown that its permission boundary does anything. One tool cannot be both, since
the policy decision point is keyed by tool rather than by operation name.

Provider agents declare `conformance.mode: provider-requests` and a fixture
naming the requests their `provider.codefly.yaml` packages:

```yaml
conformance:
  mode: provider-requests
  fixture: ./conformance/operations.yaml
```

```yaml
# conformance/operations.yaml
operations:
  - name: create-account
    request: account.create     # a request descriptor the manifest packages
    body:
      name: conformance
  - name: observe-account
    request: account.observe
    path_parameters:
      account_id: acct_0001
```

CI first starts the built provider through the agent loader and holds the
runtime catalog it advertises to the reviewed manifest: a release that does not
start, does not serve the provider protocol, or implements requests and resource
actions its manifest does not package fails before any operation runs.

CI then composes each declared operation into the request the host would plan for it
and runs it through the real provider broker, delivering from a sealed cassette
rather than the network. Every admission check a live call makes therefore runs:
descriptor packaging and digest, the read-only rule, the budget, origin
admission, request binding (method, remote-id path parameters, query and body
allowlists, ownership binding), checkpoint ordering, credential injection and
the byte budget — with no request to the provider's upstream API. A declared
operation carrying a field its descriptor does not allow fails here, as it would
at runtime. Each operation is additionally probed with a tampered descriptor
digest, and a mutating one in a read-only context; both must be refused. The
idempotency key and response-policy digest are host-owned constants: the
protocol requires them to be present and stable, not to match a value only a
live coordinator can compute.

Both declarations are required even when a run passes `--skip-conformance`: a
release with no suite to run has nothing to waive.

```bash
codefly agent ci
codefly agent ci --format json --output .artifacts/codefly-agent
codefly agent ci --native-only --skip-audit       # Explicit local audit waiver
codefly agent ci --skip-conformance               # Source/build/drift debugging only
```

### `codefly generate`

Generate client code from service APIs.

```bash
codefly generate client --from billing/api/grpc --language go --output libraries/billing-api-client  # A codefly library from a live local service
codefly generate client --from package:codefly/saas-starter@0.1.0 --language go,typescript --services AuditService  # From a composed module package's contract
codefly generate client --from contracts:module-saas-starter/contracts/api --language python --no-facade  # From a `generate contracts` export, bindings only
codefly generate proto --proto ../proto --output ./generated                             # Generate code from local proto files, in the proto companion
codefly generate contracts saas-starter                                                  # Export a module's interface endpoints as API contracts
codefly generate contracts saas-starter --check                                          # CI drift gate: fail if the on-disk catalog is stale
codefly generate runnables documents                                                     # Derive a Runnable package per method carrying the operation option
codefly generate runnables documents --check                                             # CI drift gate: fail if the derived packages are stale
codefly generate runnable-bindings --env staging                                         # Bind every derived operation to its owner endpoint as the environment resolves it
```

**`generate proto` flags:**

| Flag | Description |
|------|-------------|
| `--proto` | Path to proto directory |
| `--output` | Output directory for generated code |
| `--path` | Limit generation to a proto-relative path (repeatable) |
| `--template` | Generation template, relative to `--output` or absolute (default: `--proto`/`buf.gen.yaml`) |
| `--local` | Select `--output`/`buf.gen.local.yaml`; plugins still run inside the pinned companion |

buf resolves each `out` in the template against the template's directory, inside
the companion. The companion therefore mounts the nearest common ancestor of
`--proto`, `--output`, the template's directory and **every** `out` the template
declares, so a template whose outputs sit beside the proto directory
(`out: ../code/pkg/gen`, `out: ../openapi` — the go-grpc service layout)
regenerates on the host. An `out` that escapes the workspace owning `--proto`
(outside a workspace: the directory `--proto`, `--output` and the template share)
is refused, as is an absolute `out`, which names a path the companion cannot see.
That escape test resolves symlinks on both sides: a lexical one is bypassed by a
symlinked component in the output's own path — `out: ../link/gen` where `link`
points outside the workspace reads as inside it — and everything anchored on
that conclusion, publication and `clean: true` included, would then act outside
the boundary the caller was promised. Only existing components can be symlinks,
so an output buf has yet to create is resolved as far as it exists.

**The output validation contract: generation is staged.** buf *syncs* an output
tree rather than rewriting it — it compares the bytes it generated against what
is already under each `out` and writes only the files that are new or
different, and with `clean: true` it additionally prunes what it no longer
generates while still leaving byte-identical files untouched. So nothing
observable about the output tree after a run separates "the generator emitted
exactly what was already there" from "the generator emitted nothing" or "the
generation never reached the host", and a file that was already on disk is not
evidence that the run produced it.

So buf generates into a staging tree, through a derived template that redirects
every `out` into it, and **the CLI publishes to the declared outputs**. The
caller's template is otherwise unchanged — same plugins, options, managed-mode
block and version — and buf's own `--output` cannot be used for this: it is
prepended to each `out`, so `out: ../code/pkg/gen` under `-o /stage` resolves
straight back out of the staging directory.

**The companion runs as the invoking host UID/GID**, the way contract
generation already does, so everything it writes under a bind mount — the
staging tree it generates into, and the formatted Go it hands back — belongs to
the caller. This is load-bearing rather than cosmetic: buf creates its output
directories `0700`, and on Linux a bind mount preserves the identity that
created a file, so a companion running as root emits a tree the invoking user
can neither inspect nor remove. Publication then fails on the generator's own
output (`cannot inspect what was generated ... permission denied`), and the
staging cleanup fails after it. Desktop Docker maps container ownership onto
the host user, which is why this surfaces only on Linux, typically in CI.
Consumers need no `sudo`, `chmod` or other permission-repair step; the proto
companion sets `HOME=/tmp` precisely so it runs as an arbitrary UID.

The staging tree is created fresh for each run under the codefly home
(`~/.codefly/generate-proto/`, or `$CODEFLY_HOME`) and bind-mounted separately
from the generation mount, for two reasons. It must be **empty**, or a leftover
could supply files the run never generated — the whole point of staging — so it
is created exclusively rather than reused. And it must sit where no `out` can
name it: an `out` can resolve to the generation mount's own root (`out: .` in a
template that is its own output directory), and `clean: true` over that would
delete the staging tree along with the output, destroying the evidence
publication reads. (A `clean` that is nonetheless handed a staging tree inside
an output leaves it alone rather than deleting its own evidence, and an output
that *is* the staging tree is refused.)

The staged tree is what the run emitted, which makes each question separately
answerable:

- **A generation that emitted no file fails**, and the refusal is decided from
  staging before anything is published, so the declared outputs are left
  exactly as they were found — including under `clean: true`, which never gets
  to empty a tree on behalf of a generator that produced nothing.
- **An unchanged replay publishes byte-identical content** and succeeds. A file
  whose bytes are already on disk is left untouched, so content *and*
  modification times are unchanged, which is what a CI drift gate
  (`git diff --exit-code` over the generated tree) reads.
- **Generation into a tree the host cannot see is no longer possible**, because
  buf no longer writes the output. A staging directory the companion does not
  share with the host is caught before generation by a probe the host writes
  and the companion deletes — which also catches a mount that reads but
  discards writes, where the command exits 0 and the probe survives.
- **Publication adds and overwrites; it does not prune.** An `out` may be a
  broad source root holding handwritten files beside generated ones, and a
  generator that stops emitting a file leaves the old one in place, exactly as
  buf does. `clean: true` is the caller asking for the opposite and replaces
  the output with what the run emitted. One plugin with nothing to emit for a
  given input — openapiv2 over a contract carrying no REST annotations — is not
  a failed generation and does not disturb that output.
- **Under `clean: true`, every destination is cleaned before anything is
  published.** Outputs nest: a plugin writing `nested/x.ts` into the tree for
  `out: gen` publishes it to the path `out: gen/nested` owns, so cleaning each
  output just before copying it would delete what a sibling had already
  published.
- **Publication cannot be redirected out of the boundary.** Every host
  mutation — the clean, the output directories, the files — goes through one
  `os.Root` anchored at the boundary above, so a directory in the published
  tree that is a symlink leaving it, or a symlinked component in an output's
  own path, is an error rather than a write to a path no `out` names.
  Resolving the outputs up front establishes that they are inside the
  boundary; anchoring the writes there keeps it true, which a check alone
  cannot — a symlink swapped in between the check and the write would escape
  it, and `clean: true` deletes before it writes. A symlink that stays inside
  the boundary is still followed: that is the tree's own business.

The Go lane was never an exception to buf's sync; it only looked like one
because `goimports` runs after generation and leaves a shape buf never emits,
so the next run finds every Go file different. Publication is content-based, so
that churn no longer decides anything.

**Teardown is housekeeping, not generation.** The container `generate proto`
builds is created, driven once and thrown away: it is marked ephemeral and the
command projects the container-recovery ownership a later run's sweep matches,
so a container it cannot remove is left in exactly the state an interrupted
`generate` leaves, eligible for scoped recovery. A removal that fails is therefore
a **warning naming the container's immutable ID**, not a failed command: folding it in would
report a correct generation as a failed one, and a drift gate
(`codefly generate proto && git diff --exit-code`) cannot tell those apart. The
warning supplies a removal command using that ID. Recovery across naming scopes
requires a durable host identity; without one, only a matching scope can collect
the leftover. If the command could *not* project ownership — outside a workspace, an
unwritable home, which it already warns about — nothing will collect the
leftover, and the failure is then reported as the command's. A generation that
actually failed still fails, either way.

The removal deadlines themselves are Core's, fixed per Docker call in its own
fresh contexts, so nothing the CLI passes down shortens them. Measured idle on
the companion image with both of this command's mounts, stopping one of these
containers takes about the full SIGTERM grace — its paused PID 1 does not
handle the signal — and force-removing it is near-instant. Removal timeouts were
intermittent in local qualification; those idle timings do not establish why the
daemon sometimes exceeds its deadline. Raising that deadline is Core's call.

#### generate client

`codefly generate client` produces a **codefly library** — generated bindings plus
a generated facade — under `libraries/<name>/<language>/`, with a
`library.codefly.yaml` recording the contract it was generated from. It replaces
`generate grpc`/`generate openapi` (kept as hidden, deprecated aliases that print a
warning and delegate to `generate client --no-facade`), which produced loose files
with no caller and no way to detect drift.

| Flag | Description |
|------|-------------|
| `--from` | Required. Contract source: `module/service[/endpoint]` (a local workspace service), `package:<id>@<version>` (a composed module package), or `contracts:<dir>` (a `generate contracts` export) |
| `--language` | Required, repeatable or comma-separated: `go`, `typescript`, `python` |
| `--services` | Restrict the facade to these protobuf services (protobuf contracts only; not supported when `--from` resolves through a descriptor set — see below) |
| `--endpoint` | Select an endpoint when `--from package:`/`contracts:` resolves to more than one: `service/endpoint` |
| `--name` | Library name (default `<module>-<service>-client`) |
| `--module-name` | Facade entry-point name (default: derived from the contract's proto package) |
| `--no-facade` | Bindings only |
| `--output` | Output directory (default `<workspace>/libraries/<name>`) |
| `--force` | Overwrite an existing library whose recorded contract digest differs |
| `--go-module` | Go module path for `go/` (default `github.com/codefly-dev/<name>-go`) |
| `--npm-scope` | npm package name for `typescript/` (default `@codefly-dev/<name>`) |

Re-running `generate client` for the same library regenerates in place when the
contract is unchanged (idempotent); it refuses to overwrite a library whose
recorded `sources[].contract-digest` differs, unless `--force` is given — the
digest is the drift signal between a library and the contract it was generated
from. `library.codefly.yaml` always carries a `sources:` list (one element for
`generate client`, one per aggregated contract for `sync solution-sdk` below) so
both commands share one schema.

`--from module/service[/endpoint]` loads the service's Builder to build a
one-entry contract in memory (the same way `generate contracts` does) and
generates from the service's own proto sources. `--from package:`/`contracts:`
read an already-persisted contract (a `contract.binpb`/`openapi.json` plus
catalog entry) and generate from its descriptor bytes — a known upstream
limitation in the facade plugin currently means `--services` filtering does not
work over a multi-file descriptor set from these two sources; use the local
source, or omit `--services` to generate the full contract's facade.

##### Library layout

```
libraries/saas-starter-accounts-client/
  library.codefly.yaml
  contract/
    catalog.codefly.json         # the endpoint's catalog entry (subset if --services)
    contract.binpb | openapi.json
  go/        go.mod, gen/…, accounts_facade.pb.go
  typescript/ package.json, src/gen/…, src/accounts_facade.ts
  python/    pyproject.toml, <pkg>/_gen/…, <pkg>/accounts.py
```

Use `codefly sync library-dependencies` to link a generated library into a
service locally, and `codefly publish library <name>` to distribute it.

Generation runs buf inside the `proto` companion image; Docker must be running.

#### generate contracts

`codefly generate contracts [module]` exports the API contract of every endpoint a
module's `interface` declares (`module.codefly.yaml`'s `interface:` block) into
`contracts/api` and writes `contracts/api/catalog.codefly.json`. gRPC and connect
endpoints get a serialized `FileDescriptorSet` (`contract.binpb`) plus a copy of
the service's proto sources; REST endpoints get their OpenAPI document
(`openapi.json`). HTTP and TCP endpoints have no machine-readable contract and are
skipped. When the module has a `module.package.codefly.yaml`, its
`services[*].api-contracts` are updated to match.

An exported endpoint that carries no machine-readable contract is skipped with a
notice, never a failure: `http` and `tcp` endpoints, a `connect` endpoint on a
service with no proto, and a `rest` endpoint with no OpenAPI document. An
interface may export such an endpoint so composed modules can reach it (a
gateway's REST surface, say); that export is reachability, not a contract.

Run it before `module-package build`; the package carries the result. `--check`
is the CI drift gate.

| Flag | Description |
|------|-------------|
| `--output` | Output directory (default: `<module dir>/contracts/api`) |
| `--check` | Do not write; exit 1 if the on-disk catalog differs from what would be generated |
| `--format` | `text` (default) or `json`; `json` prints the catalog to stdout |

#### generate runnables

`codefly generate runnables [module]` derives a **SERVICE-facility Runnable
package** for every gRPC method a module's published contracts mark with the
`codefly.runnable.v0.operation` method option. A unary, idempotent method an
owner service already publishes becomes a Runnable by derivation, not by
authoring: the option says which methods are operations and under what
execution policy, and the message descriptors say what the contract is.

The input is what `generate contracts` already wrote — each gRPC/connect
endpoint's `contract.binpb` — so run that first. No flag names a method; the
option is the only selector.

```
contracts/runnables/
  index.json                                           one row per operation
  <service>/<endpoint>/<Method>/runnable-package.json  the canonical package
  <service>/<endpoint>/<Method>/operation.json         its policy and authority
```

`runnable-package.json` is the canonical proto3 JSON core takes the package
digest over, so what a composition reads and what was hashed are the same
bytes. `operation.json` is kept beside it rather than inside it: policy and
authority are installed with a binding, and two installations of one contract
may differ in both. `index.json` is what a composition reads to prepare
bindings — identity, digest, full method, input and output message names and
endpoint coordinates per row.

The tree is fully owned: a method that no longer carries the option loses its
directory. Only the three file names a generation produces are removed, and
only the directories that empties, so pointing `--output` at a directory this
command shares — `--output=contracts`, one word short of `contracts/runnables`
— never destroys what is beside it. A module with no marked method, and a
module that declares no `interface:` and so exports no endpoint at all, both
write an empty index rather than an error; a module that *does* declare an
interface but has no catalog is told to run `generate contracts` first.

A streaming method, a payload outside the bounded schema profile, or an option
core refuses is named with its field path; the walk continues so an owner
fixing a contract sees the whole list, and the command then exits non-zero
**having written nothing**. A failing run leaves the tree exactly as it found
it: writing the methods that did derive would delete the refused one's
committed package, asserting the module no longer publishes an operation whose
payload the owner merely broke, and that deletion outlives the non-zero exit.

`--check` reports a refusal and any drift from the same run, rather than
returning the refusal and discarding the diff it already computed.

**Versioning is derived, never authored.** A module that declares a
`module.package.codefly.yaml` carries its release version; one that does not is
not packageable, so the contract's own digest stands in as
`0.0.0-contract-<digest12>`. The `contract-` prefix is load-bearing: a
prerelease identifier made only of digits may not carry a leading zero, so a
bare digest would spell an invalid semantic version for roughly one contract in
1400.
Either way the version's only job is to be a valid identity — a binding pins
the package **digest**, which is what a consumer actually agreed to.

Derived operations appear in `codefly list runnables`, `codefly show runnable`
and the MCP `list_runnables` tool, with facility `service` and a `source` of
`<service>/<endpoint>/<Method>`.

| Flag | Description |
|------|-------------|
| `--output` | Output directory (default: `<module dir>/contracts/runnables`) |
| `--check` | Do not write; exit 1 with a unified diff if the on-disk tree differs from what would be generated |

---

#### generate runnable-bindings

`codefly generate runnable-bindings [--env <env>] [--check]` prepares, for one
environment, the binding of every operation the workspace's modules derived
(`contracts/runnables/index.json`). Each becomes one key of the workspace
configuration group `runnable-bindings`, in
`configurations/<profile>/runnable-bindings.env`, whose value is a JSON document:
the canonical `package`, the `binding` core prepared and verified against it
(SERVICE facility, targeting the owner endpoint at the address the environment
resolves: the native address locally, the in-cluster Service in a Kubernetes
environment), the method's `operation` policy and authority, and a
`descriptor_set` reference: the digest of the owner endpoint's descriptor set.
The key is `<MODULE>__<OPERATION>`, upper-cased. Each referenced set is written
once beside the values, under `DESCRIPTOR_SET__<digest>`: the endpoint's
published `contract.binpb` without source info, base64. Every operation on one
endpoint shares it, the installer refuses a set whose digest is not the one
referenced, and a contract whose bytes the catalog did not record is refused
here (run `generate contracts`). The value schema is
`codefly.runnable-prepared/v2`; an installer refuses the older embedded form, so
regenerate the group after upgrading. A set is larger than a process
environment should carry, so Codefly delivers it by file (core
`docs/runnable-binding-delivery.md`).

A service that installs derived operations declares `runnable-bindings` as a
workspace configuration dependency and receives them like any other group, so an
installer never names an owner module. `--check` exits non-zero when the file is
stale. Run it after `generate runnables` in every module it binds, and again
whenever an owner endpoint moves.

## Infrastructure

### `codefly daemon`

Manage the background daemon process.

```bash
codefly daemon start                          # Start services as background daemon
codefly daemon start -- --runtime-context nix # Forward flags to run service
codefly daemon start --gateway                # Start Mind Gateway gRPC server
codefly daemon stop                           # Stop the daemon
codefly daemon restart                        # Stop and restart
codefly daemon status                         # Check if daemon is running
codefly daemon logs                           # Show daemon output
codefly daemon logs -f                        # Follow log output
codefly daemon logs -n 50                     # Show last 50 lines
codefly daemon monitor                        # One-shot process check
codefly daemon monitor -w                     # Continuous monitoring (every 30s)
codefly daemon monitor --kill-orphans         # Kill orphaned agent processes
```

### `codefly service`

Install and operate a long-running foreground process through the current
user's native supervisor. macOS uses a LaunchAgent in
`~/Library/LaunchAgents` and modern `launchctl bootstrap`, `bootout`,
`kickstart`, and `print` operations. Linux uses a user unit in
`$XDG_CONFIG_HOME/systemd/user` (or `~/.config/systemd/user`) and
`systemctl --user`.

```bash
codefly service install dev.codefly.mind \
  --version 2026.07.28 \
  --executable /absolute/path/to/mind-server \
  --public-arg serve \
  --public-arg=--foreground \
  --health-http http://127.0.0.1:17400/healthz
codefly service start dev.codefly.mind
codefly service status dev.codefly.mind
codefly service restart dev.codefly.mind
codefly service stop dev.codefly.mind
codefly service uninstall dev.codefly.mind --version 2026.07.28
```

The installation version is the identity of the complete materialized
contract. Changing the executable, arguments, environment, working directory,
probe, restart policy, login policy, or logs requires a new version; Codefly
then atomically replaces the single definition. Reusing a version for different
content is rejected so a stable label cannot be silently rebound.

`--public-arg VALUE` and `--public-env NAME=VALUE` explicitly classify literals
as safe to materialize. The typed control plane rejects sensitive or
unclassified values; credentials and provider secrets must be resolved by the
service at runtime. The default restart policy
is `on-failure`, so a crash is restarted while an explicit stop remains
stopped. `--start-at-login=true` enables future login startup. macOS defaults
to owner-only files under `~/.codefly/services/logs`; Linux defaults to the user
journal. Uninstall removes only supervisor configuration and preserves product
data, credentials, and logs.

Status combines native state, PID, exit information, restart count, recent log
diagnostics, and the configured readiness probe. Its stable states are
`not-installed`, `installed-stopped`, `starting`, `running-healthy`,
`running-unhealthy`, `crash-looping`, `failed`, and `stale-corrupt`.
Use `--json` on any lifecycle command for the typed result.

### `codefly server`

Serve the local dashboard for the current workspace. Has two modes:

- **Attach**: if `codefly run service <name> --cli-server` is already serving
  this workspace's dashboard, `codefly server` prints its URL and exits
  instead of starting a second server (which would fail with "address
  already in use").
- **Inventory-only**: otherwise, it starts a dashboard showing declared
  workspace inventory; the Services, Logs and Config tabs have no live
  runtime state until a `--cli-server` run is attached.

```bash
codefly server              # Attach to a running dashboard, or serve inventory only
codefly server --open       # Same, and open the dashboard in the browser
```

### `codefly expose service [name]`

Expose a service for local Kubernetes development (port-forwarding).

```bash
codefly expose service api
```

---

## Integration

### `codefly mcp`

Model Context Protocol server for AI integration (Claude Desktop, Claude Code, etc.).

```bash
codefly mcp serve   # Start MCP server in stdio mode
codefly mcp tools   # List available MCP tools
```

---

## CI/CD

### `codefly ci`

CI pipeline commands for automated environments.

```bash
codefly ci plan --base <revision> --format json  # Inspect changed/affected services
codefly ci run --base <revision>                 # Run the complete Codefly-owned gate
codefly ci run --base <revision> --phase sync-drift,audit,sbom
codefly ci run --base <revision> --suite unit --suite integration
codefly ci run --all --jobs 4 --fail-fast=false  # Bounded graph-aware scheduling
codefly ci run --all --format json --output .artifacts/codefly
codefly ci lint --changed-file <path>             # Run agent-owned lint for affected services
codefly ci compile --changed-file <path>          # Run native compile/typecheck for affected services
codefly ci test --base <revision>                 # Run tests for affected services
codefly ci test --all --suite integration         # Use an advertised named suite
codefly ci build --base <revision>                # Build deployable artifacts for affected services
codefly ci prerelease                            # Refuse a prerelease version pin (see below)
```

All selection flags are provider-neutral. Use `--all` for an explicit full
workspace run. CI providers should invoke `codefly ci run`; language commands
and service matrices belong to Codefly agents, not provider configuration.

Tools, JSON, contracts, and configuration retain normal dependency-aware
selection.

Affected-service phase commands accept `--jobs` (`0` selects an automatic value
capped at four) and `--fail-fast`. Selected dependency prerequisites remain
ordered in every phase, including image builds: standalone agent execution
does not establish that another service's image is absent from build inputs.
With `--fail-fast=false`, independent tasks continue after failures while tasks
whose prerequisites failed are skipped. Test flows also lock shared runtime
dependency closures. Executable CI commands atomically write a
schema-versioned `report.json` to `.codefly/ci` by default. `--output` selects a
different workspace-relative report/artifact directory; `--format json`
suppresses normal narration and emits the same report payload on stdout. Every
task includes Codefly's content-addressed cache key and input digests. The
default gate is `verify`, `sync-drift`, `lint`, `compile`, `test`, `audit`,
`sbom`, and `build`; reports retain typed integrity, drift, audit, and artifact
evidence.

`codefly ci build` and the build phase of `codefly ci run` accept
`--image-sbom`, which requires a digest-bound CycloneDX inventory for every
image the build produces. Each document is written under `sbom/image/` and named
by the digest and platform actually scanned, so multi-image and multi-platform
outputs never collide while one digest shared by several services is stored
once; the report entry carries that digest, the platform, and every service
association. Incomplete coverage — a failed scan, a missing image, a stale
digest, or an omitted platform — fails the build instead of being reported as
covered. What is owed follows what the build actually made: a build that pushes
writes every platform it builds (the target environment's, see above) into one manifest list and owes evidence for all
of them, while a build that does not push loads a single platform per recipe and
owes evidence for that one alone. The report names the platform each document
covers, so a local build is never a claim about the platforms it did not build.
It is opt-in because collecting evidence runs a container scanner and
needs an agent that serves image-scope SBOMs. The `sbom` phase is unchanged and
remains source-scoped evidence, which never counts as image coverage.

### `codefly ci prerelease`

Refuse a prerelease version pin, so one never reaches the default branch and therefore never
reaches a released tag. Full reference: [prerelease-gate.md](prerelease-gate.md).

```bash
codefly ci prerelease                      # on every pull request
codefly ci prerelease --release            # before cutting a tag: no exception at all
codefly ci prerelease --go-modules         # also refuse first-party Go pseudo-versions
codefly ci prerelease --dir ../module-runtime --format json
```

Detection is the *shape* of the version, never the literal `dev`: a semver prerelease component
(`0.1.48-dev.e87db5e08865`, `-rc.1`, `-alpha`) and a Go pseudo-version
(`v0.0.0-20260930123456-abcdef123456`) are the same defect spelled differently. A range
constraint (`^0.0.1`) and the `latest` sentinel name no build and are left alone.

It reads every `version:` key in every tracked `*.codefly.yaml` at any depth — a service's
`agent.version`, a workspace's `modules[].version` and `solutions[].version`, a module's or
library's own version — plus `workspace.codefly.yaml`'s `agent-overrides` block and first-party
requires in `go.mod`. Only files git tracks, which is also why `codefly.local.yaml` is outside
the gate by construction; `testdata/` trees are skipped unless `--include-testdata`.

`agent-overrides` is the one sanctioned carrier, because `codefly publish dev` and `codefly
update workspace --agent-override` are a documented loop that has to reach a shared environment.
A prerelease is permitted there on the default branch when the entry carries a label — a comment
naming the issue it stands in for — and refused under `--release`, which is the scope a tag is
cut in. First-party `go.mod` pseudo-versions are reported rather than refused unless
`--go-modules`: they are routine in trees whose agent pins are clean, and an `// indirect` one
moves only when its dependency releases.

Needs no workspace, no agent and no network. Failure names the file, the line, the key and the
version, and says what to do instead; `--format json` emits the same report as a
schema-versioned payload. Agents reach it over MCP as `check_prerelease_versions`.

### Verified result reuse

`codefly ci run --reuse-results` stands a task on a previously verified
successful execution of the same inputs instead of running it again. It is
opt-in and needs an explicit scope: `--reuse-store` (or
`$CODEFLY_CI_RESULT_STORE`) for the record and artifact directory,
`--reuse-environment` (or `$CODEFLY_CI_REUSE_ENVIRONMENT`) to name the execution
environment — typically the runner image digest — and at least one
`--reuse-trusted-reference`. `--reuse-reference` names the reference this run
publishes under, `--reuse-run` its provenance, and `--reuse-max-age` /
`--reuse-audit-max-age` bound how old a reusable result may be. Dependency
audits default to never being reused, because advisory data changes
independently of source.

```sh
codefly ci run --base <revision> --reuse-results \
  --reuse-store /cache/codefly-results \
  --reuse-environment ghcr.io/example/runner@sha256:... \
  --reuse-reference "$GITHUB_REF" --reuse-trusted-reference refs/heads/main \
  --reuse-run "$GITHUB_RUN_ID/$GITHUB_RUN_ATTEMPT"
```

A run publishing under a trusted reference needs `--reuse-run` (or
`CODEFLY_CI_RUN`) so every success has traceable run provenance. The value must
identify a run: one made only of separators — what
`"$GITHUB_RUN_ID/$GITHUB_RUN_ATTEMPT"` collapses to wherever those variables are
unset — does not count. Without it the run still executes and reports
everything, and publishes nothing; `cache.status_reason` says so. A cached
record must identify the requested service, phase and suite, and its success
time must not be older than the configured maximum age, nor lead this run's
clock by more than five minutes — enough for ordinary skew between a publisher
and a consumer, not enough for a badly wrong clock to extend the window.
Incomplete, mismatched or implausibly dated evidence causes execution.

Records are authenticated with an HMAC keyed by `CODEFLY_CI_RESULT_KEY`, so
authenticity does not depend on the storage backend. **Expose that key only to
runs on a protected reference**: a run that can write to the store but does not
hold the key cannot publish a record any verifier accepts, and Codefly also
refuses to publish from a run whose own reference is not declared trusted.
Missing, failed, malformed, expired, untrusted or unrestorable records all fall
back to executing the task; a storage failure is never a success. The `build`
phase is never reused — its container images are not recorded in the report, so
Codefly cannot restore or verify them — and the workspace `verify` phase and the
release commands likewise keep full execution.

Each task's `cache.status` reports this run's reuse outcome — `identity_only`
when reuse is off, `ineligible`, `miss` or `hit` — with `cache.status_reason`
explaining anything but a hit, and `cache.stored` saying whether this run
published its own result. A reused task reports status `reused` rather than
`passed`, and carries `cache.reuse` identifying the producing reference, run and
revision, the matched identity, the original success time, and every restored
artifact digest. Providers must not invent keys or infer a hit.

---

## Utilities

### `codefly doctor`

Run host-level health checks (Docker, codefly home, installed agents, disk,
process limits, daemon state, stray agents, stale sockets) and print actionable
fixes. Exits non-zero if any hard check fails.

### `codefly doctor workspace`

Read-only workspace readiness check, designed to run right after creating a
fresh git worktree — before `codefly run`/`codefly test`. It validates that the
selected environment and its required configuration can be discovered and
resolved, without starting agents, containers, or services, and without ever
printing or writing secret values.

```bash
codefly doctor workspace                       # validate the local environment
codefly doctor workspace --env staging         # validate a declared environment
codefly doctor workspace --module payments     # restrict to one module's services
codefly doctor workspace --service api         # restrict to one service's declared dependencies
codefly doctor workspace --json                # machine-readable report (for worktree managers)
codefly doctor workspace --timeout 10s         # bound secret-provider resolution
```

What it checks, in order:

1. Workspace discovery and manifest validity (never migrates or rewrites files).
2. Every composed pinned module is materialized on this machine. The doctor
   never writes, so it cannot pull one; a module no materializing command has
   pulled yet fails with `module_not_materialized` naming the module and the
   commands that materialize it (see [which commands
   materialize](#which-commands-materialize)), and the service-scoped checks
   below are skipped rather than reported as core's refusal to load it.
3. The requested environment resolves through the workspace declaration
   (`local` is implicit when undeclared).
4. Declared secret backends are supported and their executables are on PATH
   (`op` for 1Password).
5. Every workspace configuration group services declare under
   `workspace-configuration-dependencies` is provided and defines values.
   The doctor reads exactly what a run provisions: the workspace's own
   `configurations/<profile>/*` composed with the groups each composed module
   ships in its own tree, so a module-shipped group counts as satisfied and the
   check names the module providing it. The workspace's own file wins over a
   module's; a group two modules define differently is reported as ambiguous
   (`configuration_duplicate`, naming both providers) until the workspace
   declares it. A missing `configurations/<profile>` directory fails only for
   the groups no composed module provides. The directory is never created.
6. Per-service `configurations/<env>` files parse; duplicates are flagged.
7. Every `${endpoint:<module>/<service>/<endpoint>}` reference in a workspace
   configuration a service in scope declares names a service of the workspace
   and an endpoint that service declares (`configuration_reference_unresolved`,
   one per reference, naming the consumer, the key and the producer). This is
   the same check, over the same service graph, that `codefly run`, `codefly ci
   run` (for its test, lint and compile phases), `codefly deploy gitops render`
   and `codefly deploy dev` run before they build or start anything; an
   unresolved reference is never silently omitted. A run that excludes the
   producer — `--exclude-dependency`, or a run profile's
   `exclude-dependencies` — must exclude the group that references it as well
   (`exclude-workspace-configurations`), or the reference has no producer left
   to resolve against and the run is refused naming both.
8. Secret provider references (`op://…`) resolve in memory through the
   configured backend; resolved values are discarded immediately. Plaintext
   values shaped like unsupported reference schemes are flagged.

With `--service`, only that service's declared workspace/service configuration
requirements are validated and resolved; unrelated configurations are not
touched. `--module` narrows the same way to one module's services, so a sibling
module's missing configuration does not fail the check — this is the scope the
GitOps delivery verbs evaluate for the module they act on. `--service` is the
narrower of the two and wins when both are given.

**Exit codes:** `0` — ready (warnings allowed); `1` — at least one check
failed (or the command itself failed).

**JSON contract** (`--json`, stdout): `{schema_version: 1, workspace,
workspace_dir, environment, environment_declared, module?, service?, status:
"ready"|"not_ready", checks: [{code, name, status: "ok"|"warn"|"fail",
message, remediation?}]}`. Output never contains configuration values, raw
`op://` references, provider output, or environment dumps.

**Stable diagnostic codes:** `workspace_not_found`, `workspace_invalid`,
`environment_not_found`, `module_not_found`, `service_not_found`,
`configuration_directory_missing`, `configuration_missing`,
`configuration_invalid`, `configuration_duplicate`,
`configuration_reference_unresolved`, `provider_not_configured`,
`provider_executable_missing`, `provider_authentication_required`,
`provider_resolution_failed`, `plaintext_not_allowed`,
`reference_scheme_unknown`, `module_not_materialized`,
`agent_override_active`, `agent_override_invalid`, `timeout`. Automation
should match on codes, never
on message prose; renaming or removing a code bumps `schema_version`.

### `codefly version`

Print the CLI version. `--json` also reports the release commit and build date.

### `codefly self check-update`

Check immutable Codefly GitHub releases without changing the installation.

```bash
codefly self check-update
codefly self check-update --channel beta
codefly self check-update --json
```

The stable channel ignores prereleases. JSON output is schema-versioned and
includes the detected install kind, selected asset, cache state, and the
installation-owner action.

### `codefly self update`

Install the selected authenticated release over a directly installed Codefly
binary.

```bash
codefly self update
codefly self update --yes
codefly self update --channel beta --yes
codefly self update --allow-downgrade --yes
```

Prereleases require `--channel beta`; an older selected release requires
`--allow-downgrade`. Homebrew, development, symlinked, and managed
installations are reported with their owner-specific upgrade command and are
never overwritten.

### `codefly open`

Open resources in your editor.

```bash
codefly open project [name]
codefly open application [name]
codefly open service [name]
```

### `codefly import application`

Import an existing application into the workspace.

### `codefly replay`

Replay recorded operations.

### `codefly clear`

Clear cached state and temporary files. Also reaps leaked frontend dev servers
(`next dev` / `npm run dev` / `vite`) that codefly spawned — proven by their
process-group authentication — and that lost their supervisor: the leaks
`codefly ps` reports as `orphaned`. Dev servers the user launched by hand (shown
as `external`) are never touched.

### `codefly ps`

List the processes this workspace's run is still holding: frontend dev servers
(`next dev` / `npm run dev` / `vite`) and native-mode services — the compiled
service binaries and the stateful stores a run started. STATUS is `orphaned`
(codefly's, escaped its supervisor — reaped by `codefly clear`), `tracked`
(codefly's, still supervised), or `external` (not codefly's — shown for
visibility, never reaped). Add `--json` for machine-readable output.

A process belongs to the run that launched it, not to the directory it runs in.
That is the difference that makes a composed run listable at all: a composed
module is checked out outside the workspace that composes it, so its services
run from that checkout, and a store runs from a data directory under
`~/.codefly/data` that is in no workspace at all. The run is recorded in the
environment of everything a `codefly` invocation starts, so `ps` lists a
composed run whole — which matters because it is the set `codefly stop` acts on.

Scoped to the current workspace. `--all` lists every workspace's processes on
this machine, which is how to find a leak belonging to a checkout you are not
standing in; outside a workspace the listing is machine-wide, since there is
nothing to scope to.

---

## Available Agents

Agents are gRPC plugin processes that implement service operations (run, build, test, deploy).
The set of agents evolves, so it isn't reproduced here: run `codefly agent list` to see every
agent known to your machine, `codefly agent versions <publisher/name>` (e.g. `go-grpc`, `nextjs`,
`postgres`) for its available versions, or call the MCP `list_agents` tool for the same
information from an AI assistant.

`codefly agent list` (and `agent versions`) report how far each pin trails its
repository as **releases** behind: prereleases never count, since no update
moves a pin to one. A dev build `<X>-dev.<sha>` is shown as `dev build of <X>`
and counted only against the releases after `X` — semver ranks it below `X`,
but it was built on top of `X`, so it is not behind it. Any other prerelease
(`1.0.0-rc.1`) keeps semver's meaning and is behind `1.0.0`.

### Keeping an image whose inputs did not change

A build reuses the image this workspace already built from the same inputs
instead of building it again. A configuration-only change — an edit to a value
under `configurations/`, a workspace or module manifest, anything a service reads
at runtime rather than at build time — re-renders the configuration and leaves
every image digest where it was, so it costs the render and not a rebuild of
every image in the module.

Reuse is keyed on a digest over the bytes that go into the image: every file of
the build context Docker would send, the verified recipe tree the agent emitted,
the exact `docker buildx` invocation (platforms, target, every build argument and
its value, push versus load, the registry cache policy), the `go.mod`/`go.sum` of
every Go module root the recipe declares a download for, and the resolved
manifest digest of every base image the Dockerfile builds from. It is never
keyed on a dependency manifest, a lock file or any other declaration *about*
those bytes — that is how a cache serves a stale binary for a source edit in a
language whose manifest did not move. A source file that changes by one byte
changes the key whether or not any manifest changed.

The last two are bound separately because a build reads them without reading the
context. The Go module prefetch reads a declared module root's manifests off the
filesystem, where no ignore policy applies, so they are hashed by path — hashing
them only through the context would let an ignore rule drop a live input. And a
base image is resolved rather than read as text: `FROM golang:1.25` is built from
whatever manifest that tag points at, and the tag is re-pushed whenever the base
is patched, so a reference is resolved to its manifest digest and the digest goes
in the key. A base reference the build cannot resolve — an unreachable registry,
no container engine, a value the recipe parameterizes — makes the recipe decline
reuse and build.

**What no digest over inputs can bind:** whatever a build step fetches from the
network itself. A `RUN` that installs from a mutable package index reads bytes no
input names, so reuse means "the image built from these inputs", not "the image a
build today would produce". Docker's own layer cache has the same property.

Two exclusions, both because the CLI writes the file from inputs the key already
binds: anything under a `.codefly/` directory (CLI-owned scratch, which is where
the reuse records themselves live), and the `build-recipes/` archive, whose
contents are a copy of the recipe tree keyed by agent version. Everything the
recipe's ignore policy excludes is excluded too, matched with the same matcher
Docker uses — a file Docker does not send is not an input. A recipe-declared
ignore file takes precedence over the context root's, exactly as for Docker.

Reuse never stands in for a build on the strength of the record alone. A pushed
image is reused only if the registry still serves the recorded manifest digest,
and a locally loaded image only if the daemon still holds the recorded image ID;
anything else — including an unreachable registry — builds. A plan with no
verified recipe digest, a context that cannot be walked and an ignore file that
cannot be parsed all build as well.

A build whose context changes while it runs records nothing: the identity was
taken before the build and the image was produced after, so an entry would claim
inputs the image does not match. The build itself is unaffected; only the record
is skipped, and the next build runs.

`--rebuild` builds every image even when no input changed. It is on
`codefly build service`, `codefly build module`, `codefly ci build`,
`codefly ci run`, `codefly deploy service`, `codefly deploy module`,
`codefly deploy dev`, `codefly deploy gitops render`, and
`codefly deploy gitops snapshot`. Reach for it when you suspect the reuse rather
than the code; a rebuild also replaces the record.

`--rebuild` bypasses **this** cache — the workspace's record of images it already
built — and nothing else. A registry layer cache (`--cache-from`/`--cache-to`), a
BuildKit layer cache, and any cache inside a build agent's own recipe emission or
inside the image build itself are untouched by it and still have to be cleared
their own way. Records live one small JSON file per input set under
`<workspace>/.codefly/build-cache/`, are never pruned, and can be removed
wholesale with `rm -rf <workspace>/.codefly/build-cache`.

```sh
codefly deploy gitops render saas --env staging            # keeps unchanged images
codefly deploy gitops render saas --env staging --rebuild  # builds every image
```

### Registry build cache

`codefly build service`, `codefly build module`, `codefly ci build`, and the build
phase of `codefly ci run` accept `--cache-from`, `--cache-to`, `--cache-scope`,
`--cache-mode`, and `--cache-backend`. Repositories are fully qualified and omit
tags. The backend currently supports `registry`; mode defaults to `max` so
intermediate dependency installs survive ordinary source edits.

```sh
codefly build service frontend --cache-from ghcr.io/example/build-cache --cache-to ghcr.io/example/build-cache --cache-scope workspace/protected
codefly ci run --phase build --cache-from ghcr.io/example/build-cache --cache-scope workspace/protected
```

Omitting `--cache-to` makes the request read-only. Authenticate Docker separately
with repository-scoped credentials. Untrusted PRs must never receive credentials
that can write a cache consumed by protected builds. Use a separate repository
for untrusted writes; scope names are not registry ACLs. `max` exports intermediate
artifacts, so cache readers must be trusted to read those artifacts too.

The CLI adds workspace, service and recipe identity to the stable caller scope. Core adds
the actual target platform. Cached builds provision a container-driver BuildKit instance before the agent
Build RPC, unless `build service --builder` selects a caller-owned builder.
Legacy in-agent builds receive and must acknowledge that selection. Recipe builds
retain agent Dockerfile/build-argument semantics, and leave
context traversal and ignore matching to Docker. A recipe-declared ignore file
takes precedence over the context root ignore file, just like a Dockerfile-specific
ignore file; declared build definitions are staged separately without changing
source files. Publication policy stays with the caller. Agents returning verified recipes leave cache execution to the CLI. Legacy
in-agent image builds must acknowledge cache execution or fail explicitly. Pushed image digests still come from Buildx metadata, never cache tags.
BuildKit progress reports cache hits, transfer sizes and import/export durations
when available; image-build wall time is logged and CI reporting retains the
operation's total duration. Provider workflows continue invoking Codefly.

### Private Go modules in image builds

A Go service that imports a module the public proxy does not serve — a private
repository — needs that module at `go mod download`. The CLI fetches it **on the
host, before any image build**, and hands the build a module proxy. No
credential is mounted into BuildKit, so none has to stay valid until a late
build runs: a render that takes hours builds its last Go service from modules
fetched at its start.

- A recipe declares the module graphs it downloads
  (`DockerBuildRecipe.go_module_downloads`: the directory of each `go.mod`,
  relative to the build context, and the named build context it reads them
  from). Such a recipe carries the v5 recipe contract, which older CLIs refuse
  rather than build without the modules.
- Every image build the CLI runs (`codefly build service`, `codefly build
  module`, `codefly ci build`, the build phase of `codefly ci run`, and a module
  render such as `codefly deploy gitops render`) runs `go mod download` and `go
  list -m all` for each declared module with the **host's** go toolchain, into a
  module cache of its own. A render does this for every service in its plan
  phase, before it builds the first image; a single-service build does it at
  its start.
- The fetch reaches a module exactly when the host's `go` does: `GOPRIVATE`,
  `GOPROXY`, `GONOSUMDB`, `NETRC` and git's configuration (credential helpers,
  `insteadOf` rewrites) are the host's. `go` must be on `PATH` when a recipe
  declares downloads.
- One git setting is the CLI's, not the host's: automatic repository maintenance
  is off in that cache (`maintenance.auto=false`, `gc.auto=0`, passed to git in
  the environment after whatever the host already configures there). A `git
  fetch` otherwise detaches `git maintenance run --auto`, which repacks the
  clone it just fetched into — and the go tool deepens that clone in the next
  fetch when it validates a pseudo-version (`--depth=1`, then every ref, then
  `--unshallow`). The maintenance rewrites `.git/shallow` while the unshallow is
  working from it, git refuses with `fatal: shallow file has changed since we
  read it`, and the go tool reports `invalid pseudo-version` — failing a render
  that was only downloading modules. The cache is deleted with the flow and each
  clone is pruned as soon as its modules are in the proxy tree, so maintaining
  it buys nothing and costs the fetch.
- A failed fetch is logged where it happens, with the go tool's own output (the
  git error, the missing credential, the incomplete `go.sum`), so the plain log
  names the cause without a `--debug` re-run.
- The build receives each fetched graph as the named build context the recipe
  declares (`--build-context gomodproxy=<cache>/cache/download`), which its
  Dockerfile reads as `GOPROXY=file://`. `GOPRIVATE`, which names module paths
  and is not a credential, is still passed as the `GOPRIVATE` build argument to
  a recipe that declares `ARG GOPRIVATE`, unless the recipe declares its own.

```sh
# A developer whose `go build` already reaches the private module.
GOPRIVATE=github.com/example-org/* codefly build service api

# A CI job: let the host's git reach the private repositories with the job's
# token; the prefetch uses it at the start of the render, and no build sees it.
git config --global url."https://x-access-token:${TOKEN}@github.com/example-org/".insteadOf "https://github.com/example-org/"
export GOPRIVATE=github.com/example-org/*
codefly deploy gitops render api --env staging
```

A recipe that declares no downloads (a `go-grpc` agent that predates the
declaration) fetches inside the build as it always did, now with no credential:
a private module there needs an authenticated `GOPROXY` or a newer agent.

### Startup container cleanup ownership

`run service` resolves its workspace and final naming scope before sweeping
containers. The regular cleanup pass is restricted to the exact canonical
`CODEFLY_HOME`, workspace path and resolved scope, including an explicitly empty scope. Agent processes
inherit this identity before they create containers. Containers from a different
home or workspace are never swept, even when their creator PID is absent.

Containers also carry a durable `codefly.recovery-namespace` label for their
stable caller host identity and canonical home/workspace. A later run can recover
explicitly ephemeral containers from a previous invocation even when its own generated scope differs,
as happens with SDK/test invocations. Live owners, stateful containers and
ledger-owned containers are preserved. This does not change which containers
agents mark ephemeral or make stateful containers disposable. Failed removals
leave the ownership labels available for retry.

Agents using the scoped-recovery Core API add `codefly.recovery-scope` at creation.
The agent acknowledges its inherited identity over gRPC. Docker initialization
fails before provisioning if that acknowledgement is absent or mismatched: rebuild
the agent against this CLI's pinned Core. Updating only the CLI does not update
installed agent binaries. Containers from older agents without both labels remain
untouched and require explicit recovery by container ID; startup never relabels them.
In the same scope, stopped orphans and running ephemeral orphans remain eligible,
while live owners, running stateful containers and session-ledger-owned containers are preserved. Startup
removal does not request deletion of volumes.
