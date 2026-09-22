# CLI Command Reference

Codefly formalizes development operations as typed gRPC APIs. The CLI is the user-facing entry point that orchestrates agents (gRPC plugin processes) to execute these operations.

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
checkouts) and against your local worktrees. It errors clearly when no module —
or more than one — declares a `service-entry`.

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
docker buildx builder with `--builder <name>`:

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
source; the platform side seeds the store and does not hand-author
ExternalSecrets.

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
same `module-trust` requirement, same `resolve.<module>.git: true` escape — and
the directive is then rewritten to the `path:` it produced, with a
[receipt](#resolution-receipts) keyed `<module>/<service>` recording the request
it answered.

`codefly doctor workspace` reports `service_override_active` for each override
in effect (they are invisible in committed config, so the healthy ones are
listed too), `service_override_unresolved` when the directory is missing or the
module declares no such service, and `service_override_contract_drift` when the
directory is not the same service. `codefly ci plan` and `codefly ci run`
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

#### Resolution receipts

Everything `run` materializes is recorded as a receipt in
`codefly.local.resolved.yaml` beside the overlay (gitignored like it). A
receipt binds the request it answered — canonical source, module subpath,
requested version or constraint — to what that request resolved to: the
materialization mode (`verified` or `git`), the exact resolved version, the
path, and for a verified package its artifact digest and commit.

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
workspace` report the module as unverified.

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

Update resources.

```bash
codefly update service api       # Update a service
codefly update workspace         # Update workspace configuration
codefly update --interactive     # Interactive update mode
```

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
Unlike the run path, this command reads the workspace without materializing
pinned modules into the overlay, so it never writes `codefly.local.yaml` or
`.gitignore`. It does resolve each composed module in order to read its manifest,
which materializes a pinned module into the content-addressed cache and fetches
it when absent — so this is not a purely offline command the first time a pinned
package is seen. A module that cannot be resolved is reported as a problem, not
silently dropped from the listing.

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
A module-agent repository publishes only the immutable Git tag. If the release
pull request merged but the tag push failed, re-run: the untagged release commit
is recognised and finished rather than bumped again.

Releasing does not update what the repository *pins*. Move a dependency first —
`codefly agent deps --dir <agent> --pin vX.Y.Z` for an agent's Core pin,
`go get github.com/codefly-dev/core@vX.Y.Z && go mod tidy` in the CLI — commit
that on `main`, and then publish. See
[docs/runbooks/release-the-fleet.md](runbooks/release-the-fleet.md) for the order.

### `codefly publish all [patch|minor|major]`

Discover every git repository under the workspace that carries a codefly
manifest and run the same flow on each, in dependency order: core → cli →
standalone modules → agents.

```bash
codefly publish all              # patch-bump every repository
codefly publish all --dry-run    # print the full plan, change nothing
codefly publish all --root DIR   # workspace root (default: nearest go.work, else cwd)
```

The run is atomic at the pre-flight boundary: every repository is validated
first and any failure aborts before a single tag is pushed. Publication then
proceeds sequentially and stops at the first real failure, reporting what already
shipped. It sweeps *every* manifest-bearing repository under the root — use
per-repository `codefly publish` when only some of the fleet should move.

### `codefly publish re-tag`

Move the current manifest tag to `HEAD` without rewriting `main`. For a release
whose tag landed on the wrong commit; it never force-pushes `main`.

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
builds and regenerates factory locks. Unknown selections or a failed build leave
the original locks intact. See [the release runbook](runbooks/release-the-fleet.md).

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
```

**`generate proto` flags:**

| Flag | Description |
|------|-------------|
| `--proto` | Path to proto directory |
| `--output` | Output directory for generated code |

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
writes every declared platform into one manifest list and owes evidence for all
of them, while a build that does not push loads a single platform per recipe and
owes evidence for that one alone. The report names the platform each document
covers, so a local build is never a claim about the platforms it did not build.
It is opt-in because collecting evidence runs a container scanner and
needs an agent that serves image-scope SBOMs. The `sbom` phase is unchanged and
remains source-scoped evidence, which never counts as image coverage.

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
codefly doctor workspace --service api         # restrict to one service's declared dependencies
codefly doctor workspace --json                # machine-readable report (for worktree managers)
codefly doctor workspace --timeout 10s         # bound secret-provider resolution
```

What it checks, in order:

1. Workspace discovery and manifest validity (never migrates or rewrites files).
2. The requested environment resolves through the workspace declaration
   (`local` is implicit when undeclared).
3. Declared secret backends are supported and their executables are on PATH
   (`op` for 1Password).
4. `configurations/<env>` exists when services declare
   `workspace-configuration-dependencies`, and required configurations exist
   and define values. The directory is never created.
5. Per-service `configurations/<env>` files parse; duplicates are flagged.
6. Secret provider references (`op://…`) resolve in memory through the
   configured backend; resolved values are discarded immediately. Plaintext
   values shaped like unsupported reference schemes are flagged.

With `--service`, only that service's declared workspace/service configuration
requirements are validated and resolved; unrelated configurations are not
touched.

**Exit codes:** `0` — ready (warnings allowed); `1` — at least one check
failed (or the command itself failed).

**JSON contract** (`--json`, stdout): `{schema_version: 1, workspace,
workspace_dir, environment, environment_declared, service?, status:
"ready"|"not_ready", checks: [{code, name, status: "ok"|"warn"|"fail",
message, remediation?}]}`. Output never contains configuration values, raw
`op://` references, provider output, or environment dumps.

**Stable diagnostic codes:** `workspace_not_found`, `workspace_invalid`,
`environment_not_found`, `service_not_found`,
`configuration_directory_missing`, `configuration_missing`,
`configuration_invalid`, `configuration_duplicate`, `provider_not_configured`,
`provider_executable_missing`, `provider_authentication_required`,
`provider_resolution_failed`, `plaintext_not_allowed`,
`reference_scheme_unknown`, `timeout`. Automation should match on codes, never
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

List frontend dev servers running inside a codefly workspace, machine-wide and
independent of the current directory (unlike `list jobs`, which needs a
workspace). STATUS is `orphaned` (codefly's, escaped its supervisor — reaped by
`codefly clear`), `tracked` (codefly's, still supervised), or `external` (not
codefly's — shown for visibility, never reaped). Add `--json` for machine-readable
output.

---

## Available Agents

Agents are gRPC plugin processes that implement service operations (run, build, test, deploy).
The set of agents evolves, so it isn't reproduced here: run `codefly agent list` to see every
agent known to your machine, `codefly agent versions <publisher/name>` (e.g. `go-grpc`, `nextjs`,
`postgres`) for its available versions, or call the MCP `list_agents` tool for the same
information from an AI assistant.

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
