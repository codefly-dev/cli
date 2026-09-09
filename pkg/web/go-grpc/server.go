package go_grpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/grpc/reflection"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/architecture"
	cli "github.com/codefly-dev/core/generated/go/codefly/cli/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/wool"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	observabilityv0 "github.com/codefly-dev/core/generated/go/codefly/observability/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"

	"google.golang.org/grpc"
)

type Configuration struct {
	EndpointGrpc string
	EndpointRest string
}

type Server struct {
	cli.UnsafeCLIServer
	config    *Configuration
	gRPC      *grpc.Server
	listener  net.Listener
	workspace *resources.Workspace
	Wool      *wool.Wool
	Terminal  *TerminalServer
	// flows owns the orchestration flow(s) this server observes/controls —
	// registered by the caller, resolved via activeFlow() — so two flows in
	// one process can never alias each other through a shared global.
	flows *engine.FlowManager
	// history keeps the most recent log lines so a dashboard opened after the run
	// started (or reloaded) can backfill. Bounded; oldest lines are dropped.
	history *logHistory
}

// workspaceFor prefers the workspace this server was constructed with and only
// falls back to the process cwd when the server was built without one.
func (s *Server) workspaceFor(ctx context.Context) (*resources.Workspace, error) {
	if s.workspace != nil {
		return s.workspace, nil
	}
	return common.LoadWorkspace(ctx)
}

// activeFlow resolves the host-owned active flow, or nil if none is running.
func (s *Server) activeFlow() *orchestration.Flow {
	if s == nil || s.flows == nil {
		return nil
	}
	_, managed := s.flows.Active()
	flow, _ := managed.(*orchestration.Flow)
	return flow
}

func (s *Server) Ping(ctx context.Context, empty *emptypb.Empty) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *Server) StopFlow(ctx context.Context, req *cli.StopFlowRequest) (*cli.StopFlowResponse, error) {
	err := s.activeFlow().Stop()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.StopFlowResponse{}, nil
}

func (s *Server) DestroyFlow(ctx context.Context, req *cli.DestroyFlowRequest) (*cli.DestroyFlowResponse, error) {
	// Destroy is the state-removing lifecycle operation. SDK dependency stacks
	// rely on it to remove ephemeral containers, while Stop intentionally
	// preserves stopped resources for ordinary local development.
	err := s.activeFlow().Shutdown()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.DestroyFlowResponse{}, nil
}

func (s *Server) GetFlowStatus(ctx context.Context, empty *emptypb.Empty) (*cli.FlowStatus, error) {
	ready := s.activeFlow().Ready(ctx)
	return &cli.FlowStatus{
		Ready: ready,
	}, nil
}

func (s *Server) GetDependenciesNetworkMappings(ctx context.Context, req *cli.GetNetworkMappingsRequest) (*cli.GetNetworkMappingsResponse, error) {
	flow := s.activeFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	svc, err := flow.ServiceFromUnique(unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	mappings, err := flow.GetDependenciesNetworkMappingsFor(ctx, svc)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetNetworkMappingsResponse{NetworkMappings: mappings}, nil
}

func (s *Server) GetConfiguration(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationResponse, error) {
	flow := s.activeFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	svc, err := flow.ServiceFromUnique(unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id, err := svc.Identity()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	conf, err := flow.ConfigurationManager.GetServiceConfiguration(ctx, id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationResponse{
		Configuration: conf,
	}, nil
}

func (s *Server) GetDependenciesConfigurations(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationsResponse, error) {
	flow := s.activeFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	svc, err := flow.ServiceFromUnique(unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id, err := svc.Identity()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	confs, err := flow.SharedState.GetDependentConfigurationsFor(ctx, id)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationsResponse{
		Configurations: confs,
	}, nil
}

func (s *Server) GetRuntimeConfigurations(ctx context.Context, req *cli.GetConfigurationRequest) (*cli.GetConfigurationsResponse, error) {
	flow := s.activeFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	unique := resources.ServiceUnique(req.Module, req.Service)
	confs, err := flow.ConfigurationManager.GetSharedServiceConfiguration(ctx, unique)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetConfigurationsResponse{
		Configurations: confs,
	}, nil
}

func (s *Server) GetAddresses(ctx context.Context, req *cli.GetAddressRequest) (*cli.GetAddressResponse, error) {
	flow := s.activeFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	address, err := flow.GetAddressForEndpoint(ctx, req.Module, req.Service, req.Endpoint)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.GetAddressResponse{
		Address: address,
	}, nil
}

/* Active information */

func (s *Server) GetActive(ctx context.Context, empty *emptypb.Empty) (*cli.ActiveResponse, error) {
	flow := s.activeFlow()
	if flow == nil {
		return nil, status.Error(codes.Internal, "nothing running")
	}
	id, err := flow.Origin().Identity()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &cli.ActiveResponse{
		Workspace: flow.ActiveWorkspace().Name,
		Module:    id.Module,
		Service:   flow.Origin().Name,
	}, nil

}

func (s *Server) ActiveLogHistory(ctx context.Context, request *observabilityv0.LogRequest) (*observabilityv0.LogResponse, error) {
	return s.logHistoryResponse(request), nil
}

/* Overall information */

func (s *Server) GetAgentInformation(ctx context.Context, request *cli.GetAgentInformationRequest) (*agentv0.AgentInformation, error) {
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, request.Agent)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	loaded, err := services.LoadAgent(ctx, agent, "")
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return loaded.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})

}

func (s *Server) GetWorkspaceInventory(ctx context.Context, request *emptypb.Empty) (*basev0.Workspace, error) {
	workspace, err := s.workspaceFor(ctx)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	view, err := architecture.LoadWorkspace(ctx, workspace)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return view, nil
}

func (s *Server) GetWorkspaceServiceDependencyGraph(ctx context.Context, _ *emptypb.Empty) (*observabilityv0.GraphResponse, error) {
	workspace, err := s.workspaceFor(ctx)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	deps, err := architecture.NewServiceDependencies(ctx, workspace)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return serviceGraphResponse(deps), nil
}

// serviceGraphResponse renders a ServiceDependencies graph as the dashboard's
// GraphNode/GraphEdge shape: one MODULE node per distinct module prefix, one
// SERVICE node per service, an edge from each module to its services, and an
// edge for each service dependency. Sorted so output is deterministic.
func serviceGraphResponse(deps *architecture.ServiceDependencies) *observabilityv0.GraphResponse {
	resp := &observabilityv0.GraphResponse{}
	seenModule := make(map[string]bool)
	for _, svc := range deps.Services() {
		module, _, _ := strings.Cut(svc.Unique, "/")
		if !seenModule[module] {
			seenModule[module] = true
			resp.Nodes = append(resp.Nodes, &observabilityv0.GraphNode{Id: module, Type: observabilityv0.GraphNode_MODULE})
		}
		resp.Nodes = append(resp.Nodes, &observabilityv0.GraphNode{Id: svc.Unique, Type: observabilityv0.GraphNode_SERVICE})
		resp.Edges = append(resp.Edges, &observabilityv0.GraphEdge{From: module, To: svc.Unique})
	}
	for _, dep := range deps.Dependencies() {
		resp.Edges = append(resp.Edges, &observabilityv0.GraphEdge{From: dep.From.Unique, To: dep.To.Unique})
	}
	sort.Slice(resp.Nodes, func(i, j int) bool {
		if resp.Nodes[i].Type != resp.Nodes[j].Type {
			return resp.Nodes[i].Type < resp.Nodes[j].Type
		}
		return resp.Nodes[i].Id < resp.Nodes[j].Id
	})
	sort.Slice(resp.Edges, func(i, j int) bool {
		if resp.Edges[i].From != resp.Edges[j].From {
			return resp.Edges[i].From < resp.Edges[j].From
		}
		return resp.Edges[i].To < resp.Edges[j].To
	})
	return resp
}

func (s *Server) GetWorkspacePublicModulesDependencyGraph(ctx context.Context, request *emptypb.Empty) (*cli.MultiGraphResponse, error) {
	workspace, err := s.workspaceFor(ctx)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	gs, err := architecture.LoadPublicModuleGraph(ctx, workspace)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	resp := &cli.MultiGraphResponse{}
	for _, g := range gs {
		resp.Graphs = append(resp.Graphs, architecture.ToGraphResponse(g))
	}
	return resp, nil
}

func (s *Server) LogHistory(ctx context.Context, request *observabilityv0.LogRequest) (*observabilityv0.LogResponse, error) {
	return s.logHistoryResponse(request), nil
}

func (s *Server) logHistoryResponse(req *observabilityv0.LogRequest) *observabilityv0.LogResponse {
	var from, to *timestamppb.Timestamp
	if req != nil {
		from, to = req.From, req.To
	}
	return &observabilityv0.LogResponse{Groups: []*observabilityv0.LogSessionGroup{{Logs: s.history.Snapshot(from, to)}}}
}

// removeANSICodes strips ANSI escape codes from the input string.
func removeANSICodes(input string) string {
	// ANSI escape codes start with the escape character (ASCII 27, octal \033)
	// followed by '[' and end with 'm'. This regex also accounts for other sequences.
	re := regexp.MustCompile(`\x1B\[[0-9;]*[a-zA-Z]`)
	return re.ReplaceAllString(input, "")
}

func (s *Server) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	if source.IsSystem() {
		return
	}
	service, err := resources.ParseServiceWithOptionalModule(source.Unique)
	if err != nil {
		s.Wool.Error("cannot parse service from source", wool.Field("source", source), wool.Field("error", err))
		return
	}
	if source.Kind != "service" {
		return
	}
	logEntry := &observabilityv0.Log{
		At:      timestamppb.New(time.Now()),
		Module:  service.Module,
		Service: service.Name,
		Message: removeANSICodes(log.String()),
		Kind:    source.Kind,
	}
	s.recordLog(logEntry)
}

// recordLog is split out from ProcessWithSource so tests can inject log
// entries directly without constructing wool types.
func (s *Server) recordLog(entry *observabilityv0.Log) {
	s.history.Add(entry)
}

// Logs streams the full log history followed by a live tail. Subscribing to
// history and registering for live entries happens atomically (logHistory.
// Subscribe), so every entry is delivered exactly once — as part of the
// initial snapshot or over the live channel, never both — and every stream,
// including concurrent ones from multiple dashboard tabs, gets its own
// independent copy of the live feed instead of racing others for lines off a
// single shared channel.
func (s *Server) Logs(empty *emptypb.Empty, server cli.CLI_LogsServer) error {
	const liveBuffer = 1000
	snapshot, live, unsubscribe := s.history.Subscribe(liveBuffer)
	defer unsubscribe()

	for _, entry := range snapshot {
		if err := server.Send(entry); err != nil {
			return err
		}
	}

	ctx := server.Context()
	for {
		select {
		case entry := <-live:
			if err := server.Send(entry); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func NewServer(c *Configuration, w *resources.Workspace, flows *engine.FlowManager) (*Server, error) {
	grpcServer := grpc.NewServer()

	// Resolve workspace directory for terminal sessions
	workspaceDir := "."
	if w != nil && w.Dir() != "" {
		workspaceDir = w.Dir()
	}

	s := Server{
		config:    c,
		workspace: w,
		gRPC:      grpcServer,
		Terminal:  NewTerminalServer(workspaceDir),
		flows:     flows,
		history:   newLogHistory(5000),
	}
	cli.RegisterCLIServer(grpcServer, &s)
	cli.RegisterTerminalServiceServer(grpcServer, s.Terminal)
	reflection.Register(grpcServer)
	return &s, nil
}

// Address is the gRPC endpoint this server binds, e.g. "127.0.0.1:10000".
func (s *Server) Address() string {
	return s.config.EndpointGrpc
}

// Listen claims the gRPC control address. Binding is a separate step from
// Run so a caller can establish ownership of the control channel — and fail
// when something else already holds it — before it provisions anything an
// aborted run would have to strand.
func (s *Server) Listen() error {
	if s.listener != nil {
		return nil
	}
	lis, err := net.Listen("tcp", s.config.EndpointGrpc)
	if err != nil {
		return err
	}
	s.listener = lis
	return nil
}

// Close releases an address claimed by Listen but never served.
func (s *Server) Close() {
	if s.listener == nil {
		return
	}
	_ = s.listener.Close()
	s.listener = nil
}

func (s *Server) Run(ctx context.Context) error {
	w := wool.Get(ctx).In("cli.Server")
	s.Wool = w
	agents.AddProcessor(s)
	if err := s.Listen(); err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}
	lis := s.listener
	// Defensive cleanup: if Serve exits unexpectedly (e.g. transient
	// listener error), GracefulStop is idempotent and lis.Close is
	// safe to call after gRPC has already closed it. Without these
	// the listener would leak the bound port on non-shutdown exits.
	defer func() {
		s.gRPC.GracefulStop()
		s.Close()
	}()
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			// Close terminal PTYs first so active attach streams can finish, then
			// stop the transport. Stop (rather than an unbounded GracefulStop)
			// guarantees SIGTERM can actually terminate a server with a live RPC.
			s.Terminal.Shutdown()
			s.gRPC.Stop()
		case <-stopped:
		}
	}()

	if err := s.gRPC.Serve(lis); err != nil {
		if ctx.Err() != nil || errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return fmt.Errorf("failed to serve: %s", err)
	}
	return nil
}

// Shutdown gracefully stops the gRPC server. Safe to call multiple
// times. Callers external to Run() should prefer this over relying on
// the Run-internal defer (e.g. when the server is wrapped by a
// supervisor that owns the lifecycle).
func (s *Server) Shutdown() {
	s.Terminal.Shutdown()
	s.gRPC.GracefulStop()
}
