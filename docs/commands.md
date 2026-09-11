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

### `codefly run service [name]`

Run a service locally with its dependency graph.

```bash
codefly run service api
codefly run service api --standalone              # Run without dependencies
codefly run service api --runtime-context nix     # Use nix runtime context
codefly run service api --service-path ./my-svc   # Override service path
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
| `--service-path` | Override the path to the service directory |
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

Each run also mints one registration secret per consumed facade prefix and
provisions all three ends of the federation exchange. Provisioning writes nothing
to disk, so a secret lives in the environment of the processes that spend it and
does not outlive the run — unless you ask for it with `--output-env`, which
exports a service's whole runtime environment, overrides included:

| End | Carrier | Value |
|-----|---------|-------|
| Solution entry service | `CODEFLY__MODULE_REGISTRATION_SECRETS` | `prefix:secret,…` — presented to register each consumed module's routes |
| Consumed module's services | `CODEFLY__MODULE_REGISTRATION_SECRET` | that module's own secret, presented to mint its service-principal work context |
| Registrar | `MODULE_REGISTRATION_SECRETS` in the `federation` workspace configuration group | `prefix:sha256hex` — the digests both plaintexts are checked against |

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

Module-agent scaffolds record their immutable template repository, tag, and
commit in `tools/base-source.json`. Scaffolds that include a base manifest must
match the source's service code or add fails without leaving a partial module
behind. Inventory-only scaffolds may omit the base manifest and service code;
their first `sync module` treats the missing manifest as an empty base and
populates the pinned source without rerunning the agent.

#### Module composition

`add module --source <path>` and `add module --worktree <owner/repo>@<ref>`
compose an out-of-repo module **without vendoring a copy** — the composition
mode for multi-repo solutions (a solution repo booting the host and runtime
modules it does not own). It is distinct from `sync module`, which vendors a
hash-pinned base.

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
`path` → overlay `worktree` → overlay `pinned` → committed identity, so the same
committed config resolves on every worktree and in CI. `codefly doctor
workspace` flags an unresolved reference with the `module_reference_unresolved`
diagnostic.

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
    <signature identity written into provenance.json>: <base64 ed25519 public key>
```

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

**`add service` flags:**

| Flag | Description |
|------|-------------|
| `--agent` | Agent type (required). Examples: `go-grpc`, `python-grpc`, `nextjs`, `krakend` |

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
codefly sync module saas                # Preview the first or next pinned base update
codefly sync module saas --apply        # Apply the pinned base update
codefly sync module saas --restore-code # Restore missing module-owned service code
codefly sync solution-sdk --language python --apply-dependencies  # Aggregate api.consumes into one solution SDK
```

For agent-backed modules, run `codefly add module --agent ...` before the first
sync so the agent can generate consumer-owned module and service inventory.
`sync module --create` initializes and populates only the manifest-owned base;
it does not run a module agent or generate that consumer inventory.

An `--apply` also refreshes each composed service's generated
`service.codefly.yaml` from the pinned source. These per-service manifests are
generated overlays (`# Code generated ... DO NOT EDIT`) that the base manifest
does not track, so without this their agent pins would drift stale against the
synced module version; the dry-run lists the manifests it would rewrite. Only
manifests still carrying the generated marker are refreshed — a service manifest
you have taken over as hand-authored product content (no marker) is left
untouched, the same ownership boundary `codefly update` honors.

The same apply refreshes the generated `interface` block of the module's own
`module.codefly.yaml` from the pinned source. That file is generated from
`deployment/topology.bindings.codefly.yaml`, a base-owned file the sync updates,
so a base release that adds an interface endpoint would otherwise leave the
module declaring a contract its own bindings contradict — and the base's
composition gate then fails in the consumer. Unlike a service manifest it is not
copied wholesale: only the `interface` block is rewritten, so the consumer's own
`name`, `description`, added `services`, comments, and formatting are kept
byte-for-byte. It is refreshed only while both sides still carry the generated
marker, and the dry-run says when it would be rewritten.

An `--apply` that leaves a service's lockfile out of sync with the pinned base's
dependencies regenerates it (`npm install --package-lock-only`), so the synced
workspace stays installable with `npm ci` (for example in a render's frontend
Dockerfile) instead of failing on a lockfile that still names the old
dependencies. Regeneration is driven by on-disk drift, not by whether this run
rewrote the `package.json`: a base sync commits its manifest last, so if an
earlier run applied the `package.json` but its lockfile regeneration was
interrupted, the next `sync module --apply` still heals the lockfile. It runs
last, after the deterministic base update and manifest refresh, so a network
failure never robs those. Only a directory that already carries a
`package-lock.json` (or `npm-shrinkwrap.json`) is regenerated — a service
without one is not an `npm ci` workflow — and the dry-run lists the lockfiles it
would rewrite. When a lockfile is genuinely adrift and `npm` is not installed,
the apply fails with the exact `npm install --package-lock-only` commands to run.

`sync module <name> --restore-code` restores only absent service files listed
by the pinned base manifest. Existing base files and consumer-owned overlays
are not changed. A legacy scaffold with neither a source lock nor a recorded
agent can bootstrap the lock during repair by providing its original immutable
source explicitly:

```bash
codefly sync module saas --restore-code \
  --source https://github.com/codefly-dev/module-saas-starter.git \
  --to v0.0.36 --subdir module
```

The source must match the service-code hashes already owned by the target base
manifest; a newer or locally modified source is rejected.

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
codefly environment import <env> --cell-contract <file|-> [--namespace <ns>] [--dry-run]
codefly environment show <env> [--json]
```

#### `codefly environment import`

Point an environment at a *cell* by consuming its `codefly/cell/v1` contract,
so cell facts are sourced from the platform instead of hand-typed. Hand-typing
an egress CIDR wrong silently drops all database traffic — the exact bug the
contract prevents.

The descriptor is produced on the platform side. For obin cells, infra-base's
`obinctl` emits it:

```bash
obinctl cell-contract <coordinate> > cell.json
codefly environment import azure --cell-contract cell.json

# Or stream it straight in and preview the change:
obinctl cell-contract hosted-eastus2 | codefly environment import azure --cell-contract - --dry-run
```

The namespace defaults to the environment's existing namespace, or the
workspace name when the environment is new; override it with `--namespace`.
`--dry-run` prints the unified diff and writes nothing. After a write, the same
readiness validation as `codefly doctor workspace --env <env>` runs and its
result is printed.

**Ownership.** An import replaces only the fields the contract owns and
preserves everything else byte-for-byte, comments included: it re-serializes
only the single environment item being imported and splices it back into the
original file, so other environments, top-level keys, blank lines, and comments
outside that item are never reflowed. (A whole-file round-trip through the YAML
library would strip blank lines and normalize indentation across the whole
document, burying the one line that changed.)

- *Contract-owned* (replaced on every import): `cluster.kind` and
  `cluster.context`, `registry`, `namespace` (set to the resolved namespace —
  `--namespace` if given, else the existing one, else the workspace name — so
  it can never disagree with the derived `gitops.path`), `gitops.repo-url` and
  `gitops.path` (path = `<workloads_path_prefix>/<namespace>`), each managed
  database's `managed-services.<name>` `kind` / `external-name` /
  `egress-cidrs`, `service-secrets.secret-store`, and `dns`.
- *Operator-owned* (never touched): `description`, `fixture`, `ingress`,
  `resource-quota`, `secrets`, `configuration-profile`, `gitops.branch`,
  `cluster.kubeconfig` (a local path, not a cell fact), a managed service's
  `secret-references`, `service-secrets.services` mappings, and any other
  declared field.

A provenance comment is stamped above the environment item and replaced (not
stacked) on re-import:

```yaml
environments:
    # imported from cell contract hosted-eastus2 (hosted-eastus2) on 2026-09-06T12:00:00Z; re-run: codefly environment import azure --cell-contract …
    - name: azure
      ...
```

The consumer maps a single cluster, registry, database and secret store per
cell (core's `ParseCellContract` rejects more). core maps the managed database
under the `store` key by default, but an existing environment that already
declares the database of the same kind under a different service name (the name
the deploy path matches) is updated in place under that name — the import never
adds a second `store` entry beside it. A sole existing managed service of a
different kind (a cache, a queue) is left untouched and the database is inserted
beside it. An environment with two or more managed services and no exact `store`
match is ambiguous and refused rather than silently leaving a stale one. Object
stores in the descriptor are ignored until core models them.

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
```

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

### `codefly publish library <name>`

Publish a workspace library's language exports (`codefly add library`) to the durable stores configured under the workspace's `libraries.publish` block — a GitHub repository tagged at the version for `go`/`python`, an npm-compatible registry for `typescript`. Published versions are immutable: publishing the same version twice fails.

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
codefly agent info [agent-name]      # Show agent information
codefly agent generate [agent-name]  # Generate agent scaffolding
codefly agent build [agent-name]     # Build an agent binary
codefly agent ci                     # Run source, release, generated-service, and drift gates
```

`codefly agent ci` is the provider-neutral agent-repository gate. It uses an
isolated Codefly home, builds the local agent, records binary and CycloneDX
hashes, runs the complete workspace CI gate against a conformance workspace, and
verifies that validation did not change the agent repository. The default
report/artifact directory is `.codefly/agent-ci`.

Conformance defaults to scaffolding a fresh service through `Builder.Create`.
Attach-only generic agents whose `Builder.Create` intentionally declines to
generate a project template (for example `codefly.dev/python`) declare an
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
codefly generate proto --proto ../proto --output ./generated                             # Generate code from local proto files (Docker)
codefly generate proto --proto ../proto --output ./generated --local                     # Same, with locally installed pinned plugins
codefly generate contracts saas-starter                                                  # Export a module's interface endpoints as API contracts
codefly generate contracts saas-starter --check                                          # CI drift gate: fail if the on-disk catalog is stale
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

Plans report a module's exact `tools/base-manifest.json` path in
`integrity_inputs`, with its owner, required `verify` phase, and reason. This
hash and ownership index does not select service test/build tasks; the underlying
source changes still do. `ci run` always includes verification for these inputs,
even with zero affected services or an explicit `--phase` list, and a changed
manifest that has been removed fails verification. `--all` preserves these
integrity obligations. Unknown change bounds or failed Git discovery block the
integrity gate even when all services are selected; supply valid change bounds
rather than relying on full service selection to replace integrity evidence.

Changed manifests are also compared with `--base` (local working-tree runs
default to `HEAD`; CI requires an explicit base). The baseline must be available
in the Git checkout. Dropping an entry while its file remains fails: base sync
would otherwise lose ownership of that file. Remove retired files with their
entries, or retain their recorded base hashes and declare intentional local
divergences in `tools/base-integrity-allow.json`. Other tools, JSON, contracts,
and configuration retain normal dependency-aware selection.

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
evidence. Cache
status is currently `identity_only`; providers must not invent keys or infer a
hit until Codefly adds restore/store outcomes.

---

## Utilities

### `codefly doctor`

Run host-level health checks (Docker, codefly home, installed agents, disk,
manifest-owned module service code, process limits, daemon state, stray agents,
stale sockets) and print actionable fixes. Exits non-zero if any hard check
fails. Missing module service code names the corresponding
`codefly sync module <name> --restore-code` repair command.

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
