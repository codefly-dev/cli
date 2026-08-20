package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/toolbox"
	corecode "github.com/codefly-dev/core/code"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// ToolHandler is the signature for tool implementations
type ToolHandler = toolbox.Handler

// ResourceHandler is the signature for resource implementations
type ResourceHandler func(ctx context.Context) ([]ResourceContents, error)

// Server implements the MCP server
type Server struct {
	workspace *resources.Workspace
	host      *engine.WorkspaceHost
	// plane is the workspace facade over the same host used by the tool registry
	// and service-agent behavior.
	plane     control.Plane
	vfs       corecode.VFS
	toolbox   *toolbox.Registry
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

	root, rootErr := os.Getwd()
	if rootErr != nil {
		return nil, fmt.Errorf("resolve MCP workspace root: %w", rootErr)
	}
	ws, err := resources.LoadWorkspaceFromDir(ctx, root)
	if err != nil {
		w.Debug("no workspace loaded, running in limited mode", wool.ErrField(err))
	}
	host, hostErr := engine.NewWorkspaceHost(engine.Config{Root: root})
	if hostErr != nil {
		return nil, fmt.Errorf("create MCP workspace host: %w", hostErr)
	}
	s := &Server{
		workspace: ws,
		host:      host,
		plane:     control.NewWithHost(host),
		toolbox:   host.Toolbox(),
		resources: make(map[string]ResourceHandler),
		resDefs:   []Resource{},
		version:   version,
	}
	for _, o := range opts {
		o(s)
	}

	s.registerTools()
	s.registerMutationTools()
	s.registerTerminalTools()
	s.registerHelpTools()
	s.registerResources()
	return s, nil
}

// RegisterTool adds a tool to the shared registry.
func (s *Server) RegisterTool(tool Tool, handler ToolHandler) error {
	return s.toolbox.Register(tool, handler)
}

// RegisterResource adds a resource to the server
func (s *Server) RegisterResource(res Resource, handler ResourceHandler) {
	s.resources[res.URI] = handler
	s.resDefs = append(s.resDefs, res)
}

// Serve runs the MCP server in stdio mode
func (s *Server) Serve(ctx context.Context) error {
	defer s.Close()
	return s.ServeIO(ctx, os.Stdin, os.Stdout)
}

// Toolbox exposes the same in-process tool registry used by the MCP adapter.
func (s *Server) Toolbox() *toolbox.Registry {
	if s == nil {
		return nil
	}
	return s.toolbox
}

// Close releases the host, its flows, tools, and agent processes.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	var planeErr, hostErr error
	if s.plane != nil {
		planeErr = s.plane.Close()
	}
	if s.host != nil {
		hostErr = s.host.Close()
	}
	s.plane = nil
	s.host = nil
	return errors.Join(planeErr, hostErr)
}

// ServeIO runs the MCP server with custom IO (for testing)
func (s *Server) ServeIO(ctx context.Context, in io.Reader, out io.Writer) error {
	w := wool.Get(ctx).In("mcp.Serve")

	type scanResult struct {
		line []byte
		err  error
	}
	results := make(chan scanResult)
	go func() {
		defer close(results)
		scanner := bufio.NewScanner(in)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case results <- scanResult{line: line}:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case results <- scanResult{err: err}:
			case <-ctx.Done():
			}
		}
	}()

	for {
		var scanned scanResult
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case scanned, ok = <-results:
			if !ok {
				return nil
			}
		}
		if scanned.err != nil {
			return scanned.err
		}
		line := scanned.line
		if len(line) == 0 {
			continue
		}

		var req JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			w.Debug("failed to parse request", wool.ErrField(err))
			resp := s.errorResponse(nil, ParseError, "Parse error")
			if err := s.writeResponse(out, resp); err != nil {
				return err
			}
			continue
		}

		resp := s.handleRequest(ctx, &req)
		// JSON-RPC notifications deliberately omit an id and MUST NOT receive a
		// response, even when the method is unknown or the handler reports an
		// error. The handler still runs so notification side effects are kept.
		if req.ID == nil {
			continue
		}
		if err := s.writeResponse(out, resp); err != nil {
			w.Error("failed to write response", wool.ErrField(err))
			return err
		}
	}
}

func (s *Server) writeResponse(out io.Writer, resp *JSONRPCResponse) error {
	if resp == nil {
		return nil
	}
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
	case "notifications/initialized", "initialized":
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
		Tools: s.toolbox.Definitions(),
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

	content, err := s.toolbox.Call(ctx, params.Name, params.Arguments)
	if errors.Is(err, toolbox.ErrUnknownTool) {
		return s.errorResponse(req.ID, InvalidParams, fmt.Sprintf("Unknown tool: %s", params.Name))
	}
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
	return s.toolbox.Definitions()
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
