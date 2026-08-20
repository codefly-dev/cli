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

Add to `~/.claude/claude_desktop_config.json`:

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

Add to your project's `.mcp.json` or global MCP config:

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

### Workspace Tools

| Tool | Description | Required Args |
|------|-------------|---------------|
| `workspace_info` | Get workspace name, description, modules, and services | -- |
| `list_modules` | List all modules with descriptions | -- |
| `list_services` | List services (optionally filtered by module) | `module` (optional) |
| `service_info` | Detailed service info: agent, endpoints, dependencies | `module`, `service` |
| `service_dependencies` | Get service dependencies with endpoints | `module`, `service` |
| `list_agents` | List available agent types | -- |
| `list_jobs` | List jobs (optionally filtered by module) | `module` (optional) |

### Help Tools

| Tool | Description | Required Args |
|------|-------------|---------------|
| `how_to` | Get codefly how-to runbooks (bump Go, cut a release, add a command, rebuild agents). Omit `topic` to list topics; pass it to fetch the full runbook. | `topic` (optional) |

The runbooks are the embedded [`docs/runbooks/`](runbooks/) files, so `how_to` works offline
and without repository access. The same content is available at the shell as
`codefly help <topic>`.

### Per-Service Tools

These tools operate on a specific service within a module.

| Tool | Description | Required Args |
|------|-------------|---------------|
| `describe` | Service metadata: name, type, agent, file list | `module`, `service` |
| `read_file` | Read a file from the service directory | `module`, `service`, `path` |
| `write_file` | Write content to a file in the service directory | `module`, `service`, `path`, `content` |
| `build` | Build the service via the agent's builder | `module`, `service` |
| `run_checks` | Run a command in the service directory | `module`, `service`, `command` (optional, default: `go test ./...`) |
| `stop` | Stop the service runtime | `module`, `service` |

---

## Available Resources

Resources provide read-only access to workspace configurations.

| Resource URI | Description | MIME Type |
|-------------|-------------|-----------|
| `codefly://workspace` | Workspace configuration (workspace.codefly.yaml) | `application/x-yaml` |
| `codefly://module/{name}` | Module configuration | `application/x-yaml` |
| `codefly://service/{module}/{service}` | Service configuration | `application/x-yaml` |
| `codefly://endpoints/{module}/{service}` | Service endpoint definitions | `application/json` |

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
Server → {"name": "api", "agent": "go-grpc", "files": ["main.go", "handler.go", ...]}

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
