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
codefly run service api --with-server             # Run with web companion UI
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
| `--with-server` | Start the web companion server |

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
```

`--render-only` writes a validated, inventoried service-owned tree without
calling Kubernetes. For a complete module promotion, use the GitOps lifecycle:

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
codefly add module host --source=../saas-host/modules/host     # Reference an out-of-repo module (no vendored copy)
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

`add module --source <path>` declares a module **by reference** rather than
vendoring a copy: the workspace entry records a `path:` to an out-of-repo module
directory, and `codefly run` boots it alongside local modules. This is the
composition mode for multi-repo solutions (a solution repo referencing the host
and runtime modules it does not own); it is distinct from `sync module`, which
vendors a hash-pinned base. `codefly doctor workspace` reports each referenced
module and flags an unresolved reference with the `module_reference_unresolved`
diagnostic.

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
```

For agent-backed modules, run `codefly add module --agent ...` before the first
sync so the agent can generate consumer-owned module and service inventory.
`sync module --create` initializes and populates only the manifest-owned base;
it does not run a module agent or generate that consumer inventory.

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

### `codefly list`

List workspace resources.

```bash
codefly list project      # List projects in workspace
codefly list module       # List modules (alias: application)
codefly list libraries    # List libraries
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

### `codefly install library [name]`

Install a library.

```bash
codefly install library auth-utils
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
codefly generate grpc                                        # Generate gRPC client code
codefly generate openapi                                     # Generate OpenAPI/Swagger client code
codefly generate proto --proto ../proto --output ./generated  # Generate code from local proto files (Docker)
```

**`generate proto` flags:**

| Flag | Description |
|------|-------------|
| `--proto` | Path to proto directory |
| `--output` | Output directory for generated code |

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

Start the codefly web companion server (for workspace visualization).

```bash
codefly server
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
Affected-service phase commands accept `--jobs` (`0` selects an automatic value
capped at four) and `--fail-fast`. Executable CI commands atomically write a
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

Clear cached state and temporary files. Also reaps orphaned frontend dev
servers (`next dev` / `npm run dev` / `vite`) whose working directory is inside
a codefly workspace but that were reparented away from codefly — the leaks that
`codefly ps` reports as `orphaned`.

### `codefly ps`

List frontend dev servers running inside a codefly workspace, machine-wide and
independent of the current directory (unlike `list jobs`, which needs a
workspace). Servers marked `orphaned` escaped codefly's tracking and can be
reaped with `codefly clear`. Add `--json` for machine-readable output.

---

## Available Agents

Agents are gRPC plugin processes that implement service operations (run, build, test, deploy).

| Agent | Language | Protocols | Description |
|-------|----------|-----------|-------------|
| `go-grpc` | Go | gRPC, REST | Go gRPC service |
| `python-grpc` | Python | gRPC | Python gRPC service |
| `python-fastapi` | Python | REST | Python FastAPI service |
| `nextjs` | TypeScript | HTTP | Next.js frontend |
| `rails` | Ruby | REST | Ruby on Rails application |
| `krakend` | - | REST, gRPC | KrakenD API Gateway |
| `postgres` | - | TCP | PostgreSQL database |
| `external-mysql` | - | TCP | MySQL database |
| `redis` | - | TCP | Redis cache/database |
| `minio` | - | HTTP | MinIO object storage |
