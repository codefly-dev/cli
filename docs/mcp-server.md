# MCP Server Integration

Codefly implements a Model Context Protocol (MCP) server that exposes workspace operations as tools and resources for AI assistants.

## What is MCP?

MCP (Model Context Protocol) is a standard for connecting AI models to external tools and data sources. It uses JSON-RPC 2.0 over stdio, allowing AI assistants like Claude to invoke codefly operations directly.

```
┌──────────────────┐     JSON-RPC 2.0      ┌──────────────────┐
│   AI Assistant   │ ◄────── stdio ──────► │  codefly mcp     │
│  (Claude, etc.)  │                       │     server       │
└──────────────────┘                       └────────┬─────────┘
                                                    │
                                              ┌─────┴─────┐
                                              │ Workspace  │
                                              │ + Agents   │
                                              │ (gRPC)     │
                                              └───────────┘
```

## Quick Start

### Start the MCP server

```bash
codefly mcp serve    # Starts in stdio mode
codefly mcp tools    # List available tools
```

### Configure Claude Desktop

Add to your Claude Desktop config file:

- macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`
- Windows: `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "codefly": {
      "command": "codefly",
      "args": ["mcp", "serve"]
    }
  }
}
```

### Configure Claude Code

Register the server with the CLI:

```bash
claude mcp add codefly -- codefly mcp serve
```

Or add to your project's `.mcp.json` or global MCP config:

```json
{
  "mcpServers": {
    "codefly": {
      "command": "codefly",
      "args": ["mcp", "serve"]
    }
  }
}
```

---

## Available Tools

`codefly mcp tools` prints the authoritative list for the installed version.

### Workspace Tools

| Tool | Description | Required Args | Optional Args |
|------|-------------|---------------|----------------|
| `workspace_info` | Get workspace name, description, modules, and services | -- | -- |
| `list_modules` | List all modules with descriptions | -- | -- |
| `list_services` | List services (optionally filtered by module) | -- | `module` |
| `service_info` | Detailed service info: agent, endpoints, dependencies | `module`, `service` | -- |
| `service_dependencies` | Get service dependencies with endpoints | `module`, `service` | -- |
| `list_agents` | List agents known to this machine: agents pinned by workspace services plus agents installed in the local cache | -- | `kind` |
| `agent_info` | Get an agent's real manifest: capabilities, protocols, languages, backends, toolchains, validation contract, configuration docs, techniques and README | `agent` | `kind` (default `service`), `include_prompts` |
| `list_jobs` | List jobs (optionally filtered by module) | -- | `module` |
| `list_runnables` | List runnables with their module/name@version identity, pinned agent and execution bounds (optionally filtered by module) | -- | `module` |

### Mutation Tools

| Tool | Description | Required Args |
|------|-------------|---------------|
| `add_service` | Create a service via the agent's Create flow | `module`, `name`, `agent`; optional `description` |

### Per-Service Tools

These tools operate on a specific service within a module (all take `module`, `service`).

| Tool | Description | Required Args | Optional Args |
|------|-------------|---------------|----------------|
| `describe` | Service metadata: name, agent, file list | `module`, `service` | -- |
| `read_file` | Read a file from the service directory | `module`, `service`, `path` | -- |
| `write_file` | Write content to a file in the service directory | `module`, `service`, `path`, `content` | -- |
| `build` | Build the service via the agent's builder | `module`, `service` | -- |
| `run_checks` | Run a command in the service directory | `module`, `service` | `command` (default: `go test ./...`) |
| `stop` | Stop the service runtime | `module`, `service` | -- |
| `list_service_commands` | List commands available on a service agent | `module`, `service` | -- |
| `run_service_command` | Run a command on a service agent | `module`, `service`, `command` | `args` |

### Mutation Tools

| Tool | Description | Required Args | Optional Args |
|------|-------------|---------------|----------------|
| `add_service` | Create a new service from a codefly agent template | `module`, `name`, `agent` | -- |
| `add_dependency` | Add a service dependency to a service's service.codefly.yaml | `module`, `service`, `dependency` | -- |
| `generate_proto` | Regenerate code from proto files | `module`, `service` | -- |
| `run_service` | Run a service with all its dependencies | `module`, `service` | `debug` |
| `test_service` | Run tests for a service with all dependencies started | `module`, `service` | -- |
| `install_agent` | Install or update a codefly agent | `name` | `version` |

### Terminal Tools

| Tool | Description | Required Args | Optional Args |
|------|-------------|---------------|----------------|
| `open_terminal` | Open a new terminal session scoped to a module/service directory | -- | `module`, `service`, `shell` |
| `send_terminal_input` | Send input to a terminal session and return output | `session_id`, `input` | -- |
| `read_terminal_output` | Read latest output from a terminal session | `session_id` | -- |
| `close_terminal` | Close a terminal session | `session_id` | -- |
| `list_terminals` | List active terminal sessions | -- | -- |

### Run & Test Tools

`run_service` and `test_service` drive the same in-process orchestration as `codefly run`/`codefly
test`, through the control plane rather than a subprocess. Runs started over MCP live as long as
the MCP server process; they are stopped when the client disconnects. Use `stop_flow` to stop
earlier.

`run_service` can be called more than once for different services, so more than one run can be
active at a time. Its response includes `flow_id`; pass that same value as `flow_id` to
`flow_status`/`stop_flow` to target that specific run. Without `flow_id`, both tools fall back to
the most recently started run — fine when only one run is active, but they will report on or stop
the wrong run if another is active and no `flow_id` is given.

| Tool | Description | Required Args | Optional Args |
|------|-------------|----------------|----------------|
| `run_service` | Run a service with its dependency graph in-process. Returns once the flow is running unless `wait=false`. | `module`, `service` | `runtime_context`, `profile`, `wait` (default `true`), `timeout_seconds` (default `300`) |
| `test_service` | Run tests for a service with all dependencies started | `module`, `service` | `suite`, `filter`, `runtime_context` |
| `flow_status` | Report the state of a run (`idle`, `starting`, `running`, `stopped`, `failed`), its services, and — when failed — the error | -- | `flow_id` (defaults to the most recently started run) |
| `stop_flow` | Stop a run | -- | `flow_id` (defaults to the most recently started run), `destroy` (also removes stateful containers, e.g. databases — default `false`) |

---

## Available Resources

Resources provide read-only access to workspace configurations.

| Resource URI | Description | MIME Type |
|-------------|-------------|-----------|
| `codefly://workspace` | Workspace configuration (workspace.codefly.yaml) | `application/x-yaml` |
| `codefly://module/{name}` | Module configuration | `application/x-yaml` |
| `codefly://service/{module}/{service}` | Service configuration | `application/x-yaml` |
| `codefly://endpoints/{module}/{service}` | Service endpoint definitions | `application/json` |

`resources/list` enumerates the concrete module, service and endpoint resources of the loaded workspace; `resources/templates/list` returns the URI templates.

---

## Protocol Details

### Transport

The MCP server communicates via **stdio** (stdin/stdout). Each message is a single line of JSON.

### JSON-RPC 2.0

All messages follow JSON-RPC 2.0:

```json
// Request
{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "workspace_info", "arguments": {}}}

// Response
{"jsonrpc": "2.0", "id": 1, "result": {"content": [{"type": "text", "text": "..."}]}}
```

### Supported Methods

| Method | Description |
|--------|-------------|
| `initialize` | Protocol handshake (returns server capabilities) |
| `initialized` | Client acknowledgment (no response) |
| `tools/list` | List available tools |
| `tools/call` | Invoke a tool |
| `resources/list` | List available resources |
| `resources/templates/list` | List resource URI templates |
| `resources/read` | Read a resource |
| `ping` | Health check |

### Protocol Version

The server implements MCP protocol version `2024-11-05`.

### Capabilities

The server advertises:
- **Tools** -- tool invocation support
- **Resources** -- resource read support

### Error Codes

| Code | Meaning |
|------|---------|
| -32700 | Parse error |
| -32600 | Invalid request |
| -32601 | Method not found |
| -32602 | Invalid params |
| -32603 | Internal error |
| -32002 | Resource not found |

### Tool Errors

Tool errors are returned as successful responses with `isError: true`:

```json
{
  "content": [{"type": "text", "text": "Error: module not found: unknown"}],
  "isError": true
}
```

---

## Security

### File Access

- `read_file` and `write_file` operations are sandboxed to the service directory
- Path traversal is blocked: paths that escape the service directory (via `..`) are rejected
- The check uses `filepath.Abs` and `strings.HasPrefix` to enforce containment

### Workspace Scope

- All tools operate within the loaded workspace context
- The server loads the workspace from the current directory at startup
- If no workspace is found, the server runs in limited mode (workspace tools return "No workspace loaded")

---

## Example Interactions

### AI discovers workspace structure

```
AI → tools/call: workspace_info
Server → {"name": "my-project", "modules": ["backend", "frontend"], "services": [...]}

AI → tools/call: list_services {module: "backend"}
Server → [{"name": "api", "agent": "go-grpc", "endpoints": [...]}, ...]
```

### AI reads and modifies code

```
AI → tools/call: describe {module: "backend", service: "api"}
Server → {"name": "api", "agent": {"name": "go-grpc", "publisher": "codefly.dev", "version": "0.0.16"}, "files": ["main.go", "handler.go", ...]}

AI → tools/call: read_file {module: "backend", service: "api", path: "handler.go"}
Server → <file contents>

AI → tools/call: write_file {module: "backend", service: "api", path: "handler.go", content: "..."}
Server → "ok"
```

### AI runs tests

```
AI → tools/call: run_checks {module: "backend", service: "api"}
Server → <test output>
```

---

## Testing

The MCP server supports a VFS (Virtual File System) option for testing:

```go
server, err := mcp.NewServer(ctx, version, mcp.WithVFS(testVFS))
```

The `ServeIO()` method accepts custom `io.Reader`/`io.Writer` for testing without stdio:

```go
err := server.ServeIO(ctx, inputReader, outputWriter)
```
