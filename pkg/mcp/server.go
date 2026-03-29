package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	corecode "github.com/codefly-dev/core/code"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// ToolHandler is the signature for tool implementations
type ToolHandler func(ctx context.Context, args map[string]string) ([]Content, error)

// ResourceHandler is the signature for resource implementations
type ResourceHandler func(ctx context.Context) ([]ResourceContents, error)

// Server implements the MCP server
type Server struct {
	workspace *resources.Workspace
	vfs       corecode.VFS
	tools     map[string]ToolHandler
	toolDefs  []Tool
	resources map[string]ResourceHandler
	resDefs   []Resource
	version   string
}

// WithVFS sets the VFS for file operations. If not set, falls back to os calls.
func WithVFS(vfs corecode.VFS) func(*Server) {
	return func(s *Server) { s.vfs = vfs }
}

// NewServer creates a new MCP server
func NewServer(ctx context.Context, version string, opts ...func(*Server)) (*Server, error) {
	w := wool.Get(ctx).In("mcp.NewServer")

	ws, err := resources.LoadWorkspaceFromDir(ctx, ".")
	if err != nil {
		w.Debug("no workspace loaded, running in limited mode", wool.ErrField(err))
	}

	s := &Server{
		workspace: ws,
		tools:     make(map[string]ToolHandler),
		toolDefs:  []Tool{},
		resources: make(map[string]ResourceHandler),
		resDefs:   []Resource{},
		version:   version,
	}
	for _, o := range opts {
		o(s)
	}

	s.registerTools()
	s.registerMutationTools()
	s.registerSymbolTools()
	s.registerResources()
	return s, nil
}

// RegisterTool adds a tool to the server
func (s *Server) RegisterTool(tool Tool, handler ToolHandler) {
	s.tools[tool.Name] = handler
	s.toolDefs = append(s.toolDefs, tool)
}

// RegisterResource adds a resource to the server
func (s *Server) RegisterResource(res Resource, handler ResourceHandler) {
	s.resources[res.URI] = handler
	s.resDefs = append(s.resDefs, res)
}

// Serve runs the MCP server in stdio mode
func (s *Server) Serve(ctx context.Context) error {
	return s.ServeIO(ctx, os.Stdin, os.Stdout)
}

// ServeIO runs the MCP server with custom IO (for testing)
func (s *Server) ServeIO(ctx context.Context, in io.Reader, out io.Writer) error {
	w := wool.Get(ctx).In("mcp.Serve")

	scanner := bufio.NewScanner(in)
	// Increase buffer size for large messages
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			w.Debug("failed to parse request", wool.ErrField(err))
			resp := s.errorResponse(nil, ParseError, "Parse error")
			s.writeResponse(out, resp)
			continue
		}

		resp := s.handleRequest(ctx, &req)
		if err := s.writeResponse(out, resp); err != nil {
			w.Error("failed to write response", wool.ErrField(err))
			return err
		}
	}

	return scanner.Err()
}

func (s *Server) writeResponse(out io.Writer, resp *JSONRPCResponse) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s\n", data)
	return err
}

func (s *Server) handleRequest(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	w := wool.Get(ctx).In("mcp.handleRequest")
	w.Debug("handling request", wool.Field("method", req.Method))

	switch req.Method {
	case "initialize":
		return s.handleInitialize(ctx, req)
	case "initialized":
		// Client acknowledgment, no response needed
		return nil
	case "tools/list":
		return s.handleListTools(ctx, req)
	case "tools/call":
		return s.handleCallTool(ctx, req)
	case "resources/list":
		return s.handleListResources(ctx, req)
	case "resources/read":
		return s.handleReadResource(ctx, req)
	case "ping":
		return s.successResponse(req.ID, map[string]any{})
	default:
		return s.errorResponse(req.ID, MethodNotFound, fmt.Sprintf("Method not found: %s", req.Method))
	}
}

func (s *Server) handleInitialize(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	result := InitializeResult{
		ProtocolVersion: MCPProtocolVersion,
		Capabilities: ServerCapabilities{
			Tools:     &ToolsCapability{},
			Resources: &ResourceCapability{},
		},
		ServerInfo: ServerInfo{
			Name:    "codefly",
			Version: s.version,
		},
	}
	return s.successResponse(req.ID, result)
}

func (s *Server) handleListTools(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	result := ListToolsResult{
		Tools: s.toolDefs,
	}
	return s.successResponse(req.ID, result)
}

func (s *Server) handleCallTool(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	w := wool.Get(ctx).In("mcp.handleCallTool")

	var params CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errorResponse(req.ID, InvalidParams, "Invalid params")
	}

	w.Debug("calling tool", wool.Field("name", params.Name), wool.Field("args", params.Arguments))

	handler, ok := s.tools[params.Name]
	if !ok {
		return s.errorResponse(req.ID, InvalidParams, fmt.Sprintf("Unknown tool: %s", params.Name))
	}

	content, err := handler(ctx, params.Arguments)
	if err != nil {
		w.Error("tool error", wool.ErrField(err))
		return s.successResponse(req.ID, CallToolResult{
			Content: []Content{ErrorContent(err)},
			IsError: true,
		})
	}

	return s.successResponse(req.ID, CallToolResult{
		Content: content,
	})
}

func (s *Server) handleListResources(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	result := ListResourcesResult{
		Resources: s.resDefs,
	}
	return s.successResponse(req.ID, result)
}

func (s *Server) handleReadResource(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	var params ReadResourceParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errorResponse(req.ID, InvalidParams, "Invalid params")
	}

	handler, ok := s.resources[params.URI]
	if !ok {
		return s.errorResponse(req.ID, InvalidParams, fmt.Sprintf("Unknown resource: %s", params.URI))
	}

	contents, err := handler(ctx)
	if err != nil {
		return s.errorResponse(req.ID, InternalError, err.Error())
	}

	return s.successResponse(req.ID, ReadResourceResult{
		Contents: contents,
	})
}

// ListTools returns all registered tools (for CLI inspection)
func (s *Server) ListTools() []Tool {
	return s.toolDefs
}

func (s *Server) successResponse(id any, result any) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Result:  result,
	}
}

func (s *Server) errorResponse(id any, code int, message string) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
}
