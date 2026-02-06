# MCP Integration Design for Codefly

## Overview

Model Context Protocol (MCP) is Anthropic's open protocol for connecting AI models to external tools and data sources. Integrating MCP into codefly enables:

1. **AI-assisted development** - LLMs can directly invoke codefly commands
2. **Architecture proposals** - AI can query workspace structure and suggest improvements
3. **Vibe coding** - Seamless loop from idea → code → deploy with AI assistance

## Integration Directions

### Direction 1: Codefly as MCP Server

Expose codefly's development API as MCP tools that any MCP client (Claude, etc.) can use.

```
┌─────────────────┐     MCP Protocol      ┌─────────────────┐
│   AI Client     │ ◄──────────────────── │  Codefly MCP    │
│  (Claude, etc)  │      (stdio/SSE)      │     Server      │
└─────────────────┘                       └─────────────────┘
                                                  │
                                                  ▼
                                          ┌─────────────────┐
                                          │  Codefly CLI    │
                                          │   (existing)    │
                                          └─────────────────┘
```

#### Proposed MCP Tools

| Tool | Description | Parameters |
|------|-------------|------------|
| `workspace_info` | Get current workspace structure | none |
| `list_services` | List all services | `module?` |
| `service_info` | Get service details | `module`, `service` |
| `add_service` | Add a new service | `name`, `module`, `agent` |
| `add_dependency` | Add service dependency | `service`, `depends_on`, `endpoints` |
| `run_service` | Start a service locally | `module`, `service` |
| `build_service` | Build a service | `module`, `service` |
| `deploy_service` | Deploy to environment | `module`, `service`, `env` |
| `get_logs` | Get service logs | `module`, `service`, `lines?` |
| `propose_architecture` | AI suggests architecture | `description` |

#### Proposed MCP Resources

| Resource | URI Pattern | Description |
|----------|-------------|-------------|
| Workspace | `codefly://workspace` | Workspace configuration |
| Module | `codefly://module/{name}` | Module configuration |
| Service | `codefly://service/{module}/{name}` | Service configuration |
| Endpoints | `codefly://endpoints/{module}/{service}` | Service endpoints |
| Dependencies | `codefly://deps/{module}/{service}` | Service dependencies |

### Direction 2: Codefly Agents as MCP Clients

Allow codefly service agents to consume external MCP servers (databases, APIs, etc.).

```
┌─────────────────┐     gRPC/Plugin      ┌─────────────────┐
│   Codefly CLI   │ ◄─────────────────── │  Service Agent  │
│                 │                       │  (go-grpc, etc) │
└─────────────────┘                       └─────────────────┘
                                                  │
                                                  │ MCP Client
                                                  ▼
                                          ┌─────────────────┐
                                          │  External MCP   │
                                          │    Servers      │
                                          └─────────────────┘
```

This enables service agents to:
- Query external databases through MCP
- Use AI-powered code generation tools
- Access documentation and knowledge bases

## Implementation Plan

### Phase 1: MCP Server (Priority)

Create a new command: `codefly mcp serve`

```go
// cmd/mcp.go
var MCPCmd = &cobra.Command{
    Use:   "mcp",
    Short: "MCP server for AI integration",
}

var MCPServeCmd = &cobra.Command{
    Use:   "serve",
    Short: "Start MCP server (stdio mode for Claude Desktop)",
    Run:   runMCPServer,
}
```

#### Proto Definitions

```protobuf
// proto/codefly/mcp/v0/mcp.proto
syntax = "proto3";

package codefly.mcp.v0;

// MCP Tool Definition (matches MCP spec)
message Tool {
    string name = 1;
    string description = 2;
    map<string, InputSchema> input_schema = 3;
}

message InputSchema {
    string type = 1;
    string description = 2;
    bool required = 3;
}

// MCP Resource Definition
message Resource {
    string uri = 1;
    string name = 2;
    string mime_type = 3;
    string description = 4;
}

// Tool Call
message ToolCallRequest {
    string name = 1;
    map<string, string> arguments = 2;
}

message ToolCallResponse {
    bool is_error = 1;
    repeated Content content = 2;
}

message Content {
    string type = 1;  // "text", "image", "resource"
    string text = 2;
    string data = 3;
    string mime_type = 4;
}

// Resource Read
message ResourceReadRequest {
    string uri = 1;
}

message ResourceReadResponse {
    repeated Content contents = 1;
}
```

#### Core Implementation

```go
// pkg/mcp/server.go
package mcp

import (
    "context"
    "encoding/json"
    "io"
)

type Server struct {
    workspace *resources.Workspace
    tools     map[string]ToolHandler
    resources map[string]ResourceHandler
}

type ToolHandler func(ctx context.Context, args map[string]any) ([]Content, error)
type ResourceHandler func(ctx context.Context) ([]Content, error)

func NewServer(ctx context.Context) (*Server, error) {
    ws, err := resources.LoadWorkspaceFromDir(ctx, ".")
    if err != nil {
        return nil, err
    }

    s := &Server{
        workspace: ws,
        tools:     make(map[string]ToolHandler),
        resources: make(map[string]ResourceHandler),
    }

    s.registerTools()
    s.registerResources()
    return s, nil
}

func (s *Server) registerTools() {
    // Workspace tools
    s.tools["workspace_info"] = s.workspaceInfo
    s.tools["list_services"] = s.listServices
    s.tools["service_info"] = s.serviceInfo

    // Mutation tools
    s.tools["add_service"] = s.addService
    s.tools["add_dependency"] = s.addDependency

    // Runtime tools
    s.tools["run_service"] = s.runService
    s.tools["build_service"] = s.buildService
    s.tools["deploy_service"] = s.deployService

    // AI-assisted tools
    s.tools["propose_architecture"] = s.proposeArchitecture
}

// Run MCP server in stdio mode (for Claude Desktop)
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
    // JSON-RPC 2.0 over stdio (MCP standard)
    decoder := json.NewDecoder(in)
    encoder := json.NewEncoder(out)

    for {
        var req JSONRPCRequest
        if err := decoder.Decode(&req); err != nil {
            if err == io.EOF {
                return nil
            }
            return err
        }

        resp := s.handleRequest(ctx, &req)
        if err := encoder.Encode(resp); err != nil {
            return err
        }
    }
}
```

### Phase 2: Claude Desktop Integration

Create configuration for Claude Desktop:

```json
// ~/.claude/claude_desktop_config.json
{
    "mcpServers": {
        "codefly": {
            "command": "codefly",
            "args": ["mcp", "serve"],
            "env": {
                "CODEFLY_WORKSPACE": "/path/to/workspace"
            }
        }
    }
}
```

### Phase 3: MCP Client in Agents

Add MCP client capability to service agents:

```go
// core/agents/mcp/client.go
package mcp

type Client struct {
    transport Transport
}

func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) ([]Content, error)
func (c *Client) ReadResource(ctx context.Context, uri string) ([]Content, error)
func (c *Client) ListTools(ctx context.Context) ([]Tool, error)
```

Agents can then use MCP servers:

```go
// In a service agent
func (s *Runtime) Init(ctx context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
    // Connect to external MCP server
    mcpClient, err := mcp.Connect(ctx, "postgres-mcp://localhost:5432")
    if err != nil {
        return nil, err
    }

    // Use MCP tools
    result, err := mcpClient.CallTool(ctx, "query", map[string]any{
        "sql": "SELECT * FROM users LIMIT 10",
    })
}
```

## File Structure

```
cli/
├── cmd/
│   └── mcp.go                    # MCP command group
├── pkg/
│   └── mcp/
│       ├── server.go             # MCP server implementation
│       ├── tools.go              # Tool handlers
│       ├── resources.go          # Resource handlers
│       ├── transport.go          # stdio/SSE transport
│       └── protocol.go           # JSON-RPC types

core/
├── agents/
│   └── mcp/
│       ├── client.go             # MCP client for agents
│       └── types.go              # Shared MCP types

proto/
└── codefly/
    └── mcp/
        └── v0/
            └── mcp.proto         # MCP proto definitions
```

## Usage Examples

### Example 1: AI Creates a Service

User tells Claude: "Add a user service with REST API in the backend module"

Claude calls MCP tools:
```json
// 1. Check workspace structure
{"method": "tools/call", "params": {"name": "workspace_info"}}

// 2. Add the service
{"method": "tools/call", "params": {
    "name": "add_service",
    "arguments": {
        "name": "user",
        "module": "backend",
        "agent": "go-grpc"
    }
}}

// 3. Verify creation
{"method": "tools/call", "params": {
    "name": "service_info",
    "arguments": {"module": "backend", "service": "user"}
}}
```

### Example 2: AI Proposes Architecture

User: "I need a microservices setup for an e-commerce platform"

Claude calls:
```json
{"method": "tools/call", "params": {
    "name": "propose_architecture",
    "arguments": {
        "description": "e-commerce platform with user management, product catalog, shopping cart, and checkout"
    }
}}
```

Returns structured proposal with services, dependencies, and agent recommendations.

## Next Steps

1. [ ] Create `proto/codefly/mcp/v0/mcp.proto`
2. [ ] Implement `pkg/mcp/server.go` with stdio transport
3. [ ] Add `codefly mcp serve` command
4. [ ] Implement core tools (workspace_info, list_services, etc.)
5. [ ] Test with Claude Desktop
6. [ ] Add mutation tools (add_service, add_dependency)
7. [ ] Document Claude Desktop setup
8. [ ] Phase 2: MCP client for agents
