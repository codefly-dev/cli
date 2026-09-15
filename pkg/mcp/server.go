package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/toolbox"
	"github.com/codefly-dev/core/agents"
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
	plane             control.Plane
	vfs               corecode.VFS
	toolbox           *toolbox.Registry
	resources         map[string]ResourceHandler
	resDefs           []Resource
	resourceTemplates []ResourceTemplate
	version           string
	// runCtx governs flows started by run_service. It outlives any single tool call
	// and is cancelled by Close, so a stack started over MCP stops with the server.
	runCtx    context.Context
	cancelRun context.CancelFunc
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
		// The plane has no terminal of its own; unset, it drops every line a
		// flow logs — including the playbook's "service X failing" and the
		// errors wrapped out of Flow.Start, which is what an operator needs
		// when a run driven from here fails. stderr is safe and visible.
		plane:     control.NewWithHost(host, control.WithNarration(protocolSafeLogger{})),
		toolbox:   host.Toolbox(),
		resources: make(map[string]ResourceHandler),
		resDefs:   []Resource{},
		version:   version,
	}
	s.runCtx, s.cancelRun = context.WithCancel(context.WithoutCancel(ctx))
	for _, o := range opts {
		o(s)
	}

	s.registerTools()
	s.registerAgentTools()
	s.registerMutationTools()
	s.registerTerminalTools()
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

// protocolSafeLogger writes wool output to stderr.
//
// wool resolves a logger per call: a context carrying a provider uses that
// provider's, and everything without one falls back to a Console that prints to
// stdout. In stdio mode stdout is the JSON-RPC stream, so such a line corrupts
// the protocol. These logs are not hypothetical and not all ours to route —
// host construction reaps stale process groups with context.Background()
// (pkg/engine/host.go), which no context this server passes around can reach.
// stderr keeps them visible to the operator instead of discarding them.
//
// It does not re-check the level: wool.process already filtered this line
// against the effective level, which honours the per-scope overrides of
// CODEFLY_LOG (wool.Wool.LogLevel). Comparing against the global level a second
// time here drops exactly the lines a scope override was set to surface.
type protocolSafeLogger struct{}

func (protocolSafeLogger) Process(msg *wool.Log) {
	writeProtocolSafe(nil, msg)
}

func (protocolSafeLogger) ProcessWithSource(source *wool.Identifier, msg *wool.Log) {
	writeProtocolSafe(source, msg)
}

func writeProtocolSafe(source *wool.Identifier, msg *wool.Log) {
	if msg == nil {
		return
	}
	line := msg.String()
	if source != nil && source.Unique != "" {
		line = source.Unique + " | " + line
	}
	fmt.Fprintln(os.Stderr, line)
}

// agentLogRelay carries the logs of spawned agents to stderr while the protocol
// guard is installed.
//
// agents.AddProcessor has no removal, so the relay is registered once per
// process and gated on the guard rather than added and removed with it.
type agentLogRelay struct{}

func (agentLogRelay) Process(msg *wool.Log) {
	if protocolGuardActive.Load() {
		writeProtocolSafe(nil, msg)
	}
}

func (agentLogRelay) ProcessWithSource(source *wool.Identifier, msg *wool.Log) {
	if protocolGuardActive.Load() {
		writeProtocolSafe(source, msg)
	}
}

var (
	protocolGuardMu       sync.Mutex
	protocolGuardDepth    int
	protocolGuardActive   atomic.Bool
	protocolAgentRelaying sync.Once
)

// ProtectStdout claims stdout for the JSON-RPC protocol and returns the undo.
//
// Three separate process-global writers put log lines on stdout, and a stdio
// server has to silence all of them — routing one leaves the stream corrupt:
//
//   - wool's fallback sink, used by every context with no provider. The reaper
//     run by engine.NewWorkspaceHost logs through it with context.Background().
//   - pkg/cli's logger, which is the wool logger cmd/common.NewContext installs
//     and therefore where the serve command's own narration goes.
//   - that same pkg/cli logger registered process-wide with agents.AddProcessor
//     (pkg/cli/logger.go's init), which is where every spawned agent's log lines
//     are fanned out — by far the highest-volume writer once a run starts.
//
// Each is pointed at stderr rather than dropped: these lines are what an
// operator reads when a run driven over MCP fails.
//
// Call this BEFORE constructing anything, not just before serving: the reaper
// above runs inside engine.NewWorkspaceHost, so a guard installed later misses
// the very lines it names. Calls nest; the guard lifts on the last undo.
func ProtectStdout() func() {
	protocolGuardMu.Lock()
	protocolGuardDepth++
	if protocolGuardDepth == 1 {
		wool.SetFallbackLogger(protocolSafeLogger{})
		cli.SetOutputSink(func(_ wool.Loglevel, message string) {
			fmt.Fprintln(os.Stderr, message)
		})
		cli.SuppressOutput()
		protocolGuardActive.Store(true)
		protocolAgentRelaying.Do(func() { agents.AddProcessor(agentLogRelay{}) })
	}
	protocolGuardMu.Unlock()

	var undo sync.Once
	return func() {
		undo.Do(func() {
			protocolGuardMu.Lock()
			defer protocolGuardMu.Unlock()
			protocolGuardDepth--
			if protocolGuardDepth > 0 {
				return
			}
			protocolGuardActive.Store(false)
			cli.RestoreOutput()
			cli.SetOutputSink(nil)
			wool.SetFallbackLogger(nil)
		})
	}
}

// Serve runs the MCP server in stdio mode
func (s *Server) Serve(ctx context.Context) error {
	// From here stdout belongs to the protocol, so nothing may log to it. The
	// serve command installs the same guard before NewServer — construction
	// logs too — so this is the nested claim that covers a caller who reached
	// Serve some other way, and it is lifted on return rather than left pinned
	// on a process that goes on to do something else.
	//
	// Registered before the Close below so it is undone AFTER it: Close joins
	// the run goroutines and tears the host down, and that teardown logs.
	defer ProtectStdout()()
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
	if s.cancelRun != nil {
		s.cancelRun()
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

// ServeIO runs the MCP server with custom IO (for testing).
//
// Requests are dispatched to their own goroutine rather than handled inline:
// a tool call is free to block for a long time (run_service's wait, for
// instance), and this server has exactly one stdio stream per client, so
// handling requests one at a time would leave every other tool — including
// flow_status/stop_flow, whose entire purpose is to be usable *while*
// run_service is still working — unreachable until the blocking call
// returns. Writes to out are serialized with writeMu since concurrent
// handlers now write to the same stream. ServeIO waits for every dispatched
// goroutine to finish before returning, so Close() (always invoked by Serve
// only after ServeIO returns) never races a handler that is still reading
// s.plane/s.host.
func (s *Server) ServeIO(ctx context.Context, in io.Reader, out io.Writer) (retErr error) {
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

	var writeMu sync.Mutex
	writeResponse := func(resp *JSONRPCResponse) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return s.writeResponse(out, resp)
	}

	// writeErrs carries the first fatal write failure from a request
	// goroutine back to this loop, preserving the prior behavior of ServeIO
	// returning that error. Buffered by one: only the first failure matters.
	writeErrs := make(chan error, 1)

	var wg sync.WaitGroup
	defer func() {
		// Drain in-flight handlers before returning, then let a write failure
		// any of them hit override whatever this loop already decided to
		// return (e.g. nil from a clean EOF that raced the failure).
		wg.Wait()
		select {
		case err := <-writeErrs:
			retErr = err
		default:
		}
	}()

	for {
		var scanned scanResult
		var ok bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-writeErrs:
			return err
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
			if err := writeResponse(resp); err != nil {
				return err
			}
			continue
		}

		wg.Add(1)
		go func(req JSONRPCRequest) {
			defer wg.Done()
			resp := s.handleRequest(ctx, &req)
			// JSON-RPC notifications deliberately omit an id and MUST NOT receive a
			// response, even when the method is unknown or the handler reports an
			// error. The handler still runs so notification side effects are kept.
			if req.ID == nil {
				return
			}
			if err := writeResponse(resp); err != nil {
				w.Error("failed to write response", wool.ErrField(err))
				select {
				case writeErrs <- err:
				default:
				}
			}
		}(req)
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
	case "resources/templates/list":
		return s.handleListResourceTemplates(ctx, req)
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
		Resources: s.listConcreteResources(ctx),
	}
	return s.successResponse(req.ID, result)
}

func (s *Server) handleListResourceTemplates(_ context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	result := ListResourceTemplatesResult{
		ResourceTemplates: s.resourceTemplates,
	}
	return s.successResponse(req.ID, result)
}

func (s *Server) handleReadResource(ctx context.Context, req *JSONRPCRequest) *JSONRPCResponse {
	var params ReadResourceParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errorResponse(req.ID, InvalidParams, "Invalid params")
	}

	handler, ok := s.resolveResource(params.URI)
	if !ok {
		return s.errorResponse(req.ID, ResourceNotFound, fmt.Sprintf("Resource not found: %s", params.URI))
	}

	contents, err := handler(ctx)
	if err != nil {
		if errors.Is(err, errResourceNotFound) {
			return s.errorResponse(req.ID, ResourceNotFound, err.Error())
		}
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
