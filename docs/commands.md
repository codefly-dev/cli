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
codefly run service api --output-env .env         # Write endpoint env vars to file
codefly run service api --exclude-root            # Only run dependencies, not the service itself
codefly run service api --exclude-dependency infra/temporal  # Omit optional dependency
codefly run service api --silent backend/db       # Suppress log output for a dependency
codefly run service api --with-server             # Run with web companion UI
```

**Key flags:**

| Flag | Description |
|------|-------------|
| `--standalone` | Don't start dependency services |
| `--exclude-root` | Start dependencies only, skip the target service |
| `--exclude-dependency` | Exclude optional dependency services from this run. Repeatable; accepts `module/service` or an unambiguous service name. |
| `--service-path` | Override the path to the service directory |
| `--runtime-context` | Runtime context (e.g., `nix`, `docker`) |
| `--fixture` | Named fixture for test data |
| `--remote` | Use a remote service instead of local (format: `module/service:environment`) |
| `--silent` | Suppress output for named services |
| `--output-env` | Write runtime environment variables to a file |
| `--load-only` | Stop after Load phase |
| `--init-only` | Stop after Init phase |
| `--with-server` | Start the web companion server |

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
```

### `codefly deploy init`

Initialize deployment configuration for a service.

---

## Management

### `codefly add`

Add resources to the workspace.

```bash
codefly add module backend                                     # Add a module
codefly add service api --agent=go-grpc                        # Add a service with an agent
codefly add service-dependency api --dependency=backend/db     # Add a service dependency
codefly add library utils                                      # Add a library
codefly add library-dependency utils --dependency=core/models  # Add a library dependency
codefly add job db-migration --agent=go-grpc                   # Add a job
codefly add application web                                    # Add an application
codefly add application-dependency web --dependency=backend    # Add an application dependency
```

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
```

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

### `codefly version`

Print the CLI version.

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

Clear cached state and temporary files.

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
