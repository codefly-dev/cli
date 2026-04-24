// Package gateway implements the Mind Gateway gRPC server.
//
// The Gateway is the single interface Mind uses to interact with Codefly.
// It loads language-specific plugin agents (e.g. go-generic) via the agent
// manager, spawns them as gRPC processes, and proxies RPCs to them.
// Code operations are proxied to the plugin's Code service; Build/Test/Lint
// are delegated to the plugin's Runtime service. Generic operations (Git,
// RunCommand) are handled directly.
//
// This design supports remote agents: the plugin process could run on a
// different machine — the Gateway just needs a gRPC connection.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	wotel "github.com/codefly-dev/core/wool/otel"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"
)

// Config holds the Gateway server configuration.
type Config struct {
	WorkDir string // directory containing mind.yaml
	Port    int    // gRPC listen port
}

// Server implements the Gateway gRPC service.
type Server struct {
	gatewayv1.UnimplementedGatewayServer
	cfg      Config
	mindYAML *MindYAML
	grpcSrv  *grpc.Server

	// Plugin management: one Code agent per service.
	pluginMu   sync.Mutex
	plugins    map[string]*pluginConn // service name → plugin connection
	stopHealth chan struct{}           // closed to stop all health-monitor goroutines
	healthWg   sync.WaitGroup         // tracks running health-monitor goroutines
}

// pluginConn holds a running agent process and its gRPC clients.
type pluginConn struct {
	agent      *resources.Agent
	agentConn  *manager.AgentConn
	code       codev0.CodeClient
	agentSvc   agentv0.AgentClient
	runtime    runtimev0.RuntimeClient
}

// MindYAML mirrors the mind.yaml config structure.
type MindYAML struct {
	Service        string    `yaml:"service"`
	Plugin         string    `yaml:"plugin"`
	Config         SvcConfig `yaml:"config"`
	Infrastructure []string  `yaml:"infrastructure,omitempty"`
}

// SvcConfig holds the project layout configuration from mind.yaml.
type SvcConfig struct {
	Path string `yaml:"path"`
	Type string `yaml:"type"`
	Port int    `yaml:"port,omitempty"`
}

// NewServer creates a Gateway server. It attempts to load mind.yaml from
// cfg.WorkDir but starts successfully without it — plugin-dependent RPCs
// will return a clear error until a mind.yaml is present or the config is
// provided at runtime.
func NewServer(cfg Config) (*Server, error) {
	s := &Server{
		cfg:        cfg,
		plugins:    make(map[string]*pluginConn),
		stopHealth: make(chan struct{}),
	}

	path := filepath.Join(cfg.WorkDir, "mind.yaml")
	data, err := os.ReadFile(path)
	if err == nil {
		var my MindYAML
		if parseErr := yaml.Unmarshal(data, &my); parseErr != nil {
			return nil, fmt.Errorf("parse mind.yaml: %w", parseErr)
		}
		s.mindYAML = &my
	}
	// No mind.yaml is fine — the gateway still serves basic RPCs (git, shell)
	// and will report a clear error for plugin-dependent operations.

	return s, nil
}

// requireConfig returns an error if mind.yaml was not loaded.
// Call this before any operation that needs the service/plugin config.
func (s *Server) requireConfig() error {
	if s.mindYAML == nil {
		return status.Errorf(codes.FailedPrecondition,
			"no mind.yaml found in %s — create one or run 'mind init' first", s.cfg.WorkDir)
	}
	return nil
}

// Serve starts the gRPC server and blocks until stopped.
func (s *Server) Serve(ctx context.Context) error {
	w := wool.Get(ctx).In("gateway.Serve")

	if _, err := wotel.Enable(
		wotel.WithServiceName("codefly-gateway"),
	); err != nil {
		w.Warn("OTEL init failed (tracing disabled)", wool.ErrField(err))
	}

	addr := fmt.Sprintf("127.0.0.1:%d", s.cfg.Port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	s.grpcSrv = grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.UnaryInterceptor(rpcLogInterceptor()),
	)
	gatewayv1.RegisterGatewayServer(s.grpcSrv, s)

	if err := writePortFile(s.cfg.Port); err != nil {
		return fmt.Errorf("write port file: %w", err)
	}

	if s.mindYAML != nil {
		fmt.Printf("[gateway] Serving on %s (service: %s, plugin: %s)\n",
			addr, s.mindYAML.Service, s.mindYAML.Plugin)
	} else {
		fmt.Printf("[gateway] Serving on %s (no mind.yaml — plugin RPCs disabled until configured)\n", addr)
	}

	go func() {
		<-ctx.Done()
		fmt.Println("[gateway] Shutting down...")
		close(s.stopHealth)
		s.healthWg.Wait()
		s.shutdownPlugins()
		s.grpcSrv.GracefulStop()
	}()

	return s.grpcSrv.Serve(lis)
}

// shutdownPlugins gracefully terminates all plugin agent processes.
// Each agent gets 5 seconds to shut down after SIGTERM before being killed.
func (s *Server) shutdownPlugins() {
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()

	var wg sync.WaitGroup
	for name, pc := range s.plugins {
		wg.Add(1)
		go func(name string, pc *pluginConn) {
			defer wg.Done()
			pid := pc.agentConn.ProcessInfo().PID
			fmt.Printf("[gateway] Stopping plugin %q (PID %d) gracefully...\n", name, pid)

			// Send SIGTERM for graceful shutdown.
			_ = syscall.Kill(pid, syscall.SIGTERM)

			// Poll every 100ms to see if the process exited.
			exited := make(chan struct{})
			go func() {
				for {
					if err := syscall.Kill(pid, 0); err != nil {
						close(exited)
						return
					}
					time.Sleep(100 * time.Millisecond)
				}
			}()

			select {
			case <-exited:
				// Process exited gracefully.
			case <-time.After(5 * time.Second):
				fmt.Printf("[gateway] Plugin %q (PID %d) did not exit in 5s, sending SIGKILL\n", name, pid)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}

			pc.agentConn.Close()
			fmt.Printf("[gateway] Plugin %q stopped\n", name)
		}(name, pc)
	}
	wg.Wait()
	s.plugins = make(map[string]*pluginConn)
}

// ─── Plugin Management ───────────────────────────────────────

// ensurePlugin lazily loads the agent binary for the service and returns
// the Code gRPC client.
//
// Resolution order:
//  1. Check cache (already running plugin for this service).
//  2. Try locally-installed binaries first (from `codefly agent build`).
//  3. Fall back to GitHub release pinning + download.
func (s *Server) ensurePlugin(ctx context.Context) (codev0.CodeClient, error) {
	svcName := s.mindYAML.Service

	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()

	if pc, ok := s.plugins[svcName]; ok {
		return pc.code, nil
	}

	agentName := pluginToAgentName(s.mindYAML.Plugin)
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, agentName)
	if err != nil {
		return nil, fmt.Errorf("parse agent %q: %w", agentName, err)
	}

	// Resolve version: prefer local binaries, fall back to GitHub releases.
	if err := manager.ResolveLatest(ctx, agent); err != nil {
		return nil, fmt.Errorf("resolve agent %s version: %w", agentName, err)
	}

	fmt.Printf("[gateway] Loading plugin %s (v%s)...\n", agent.Name, agent.Version)

	// Wrap the load in a 60s timeout so the gateway doesn't hang forever.
	loadCtx, loadCancel := context.WithTimeout(ctx, 60*time.Second)
	defer loadCancel()

	// Tee agent stderr to gateway stdout so debug mode surfaces agent logs.
	agentLogWriter := newPrefixWriter(os.Stderr, fmt.Sprintf("[agent:%s] ", agent.Name))

	agentConn, err := manager.Load(loadCtx, agent,
		manager.WithLogWriter(agentLogWriter),
		manager.WithWorkDir(s.cfg.WorkDir),
	)
	if err != nil {
		return nil, fmt.Errorf("load agent %s: %w", agentName, err)
	}

	grpcConn := agentConn.GRPCConn()
	codeClient := codev0.NewCodeClient(grpcConn)
	agentClient := agentv0.NewAgentClient(grpcConn)
	runtimeClient := runtimev0.NewRuntimeClient(grpcConn)

	s.plugins[svcName] = &pluginConn{
		agent:     agent,
		agentConn: agentConn,
		code:      codeClient,
		agentSvc:  agentClient,
		runtime:   runtimeClient,
	}

	// Start health monitor: evict dead plugins so they auto-reload on next call.
	s.healthWg.Add(1)
	go s.monitorPlugin(svcName, agentConn)

	fmt.Printf("[gateway] Plugin %s loaded (PID %d)\n", agent.Name, agentConn.ProcessInfo().PID)
	return codeClient, nil
}

// ensureRuntime returns the Runtime gRPC client, loading the plugin if needed.
func (s *Server) ensureRuntime(ctx context.Context) (runtimev0.RuntimeClient, error) {
	_, err := s.ensurePlugin(ctx)
	if err != nil {
		return nil, err
	}
	svcName := s.mindYAML.Service
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	pc := s.plugins[svcName]
	return pc.runtime, nil
}

// monitorPlugin checks every 5s if the agent process is still alive.
// If it dies, the plugin is evicted from cache so ensurePlugin reloads it.
func (s *Server) monitorPlugin(svcName string, ac *manager.AgentConn) {
	defer s.healthWg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopHealth:
			return
		case <-ticker.C:
			conn := ac.GRPCConn()
			if conn == nil {
				s.evictPlugin(svcName)
				return
			}
			// Check if the gRPC connection is still viable.
			state := conn.GetState()
			if state == connectivity.Shutdown {
				fmt.Printf("[gateway] Plugin %q connection shut down, evicting from cache\n", svcName)
				s.evictPlugin(svcName)
				return
			}
		}
	}
}

// evictPlugin removes a plugin from the cache so ensurePlugin will reload it.
func (s *Server) evictPlugin(svcName string) {
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	if pc, ok := s.plugins[svcName]; ok {
		pc.agentConn.Close()
		delete(s.plugins, svcName)
		fmt.Printf("[gateway] Evicted dead plugin %q\n", svcName)
	}
}

// withRetry wraps an RPC call with automatic retry on connection failure.
// If the call returns codes.Unavailable or codes.Internal, the plugin is
// evicted and reloaded, then the call is retried once.
func (s *Server) withRetry(ctx context.Context, rpcName string, fn func(codev0.CodeClient) error) error {
	code, err := s.ensurePlugin(ctx)
	if err != nil {
		return err
	}
	err = fn(code)
	if err == nil {
		return nil
	}
	// Check if this is a transient connection error worth retrying.
	st, ok := status.FromError(err)
	if !ok || (st.Code() != codes.Unavailable && st.Code() != codes.Internal) {
		return err
	}

	fmt.Printf("[gateway] %s failed with %s, reconnecting plugin...\n", rpcName, st.Code())
	s.evictPlugin(s.mindYAML.Service)

	code, err = s.ensurePlugin(ctx)
	if err != nil {
		return fmt.Errorf("retry %s: reconnect failed: %w", rpcName, err)
	}
	return fn(code)
}

// proxyExecute sends a unified CodeRequest to the plugin via Execute RPC,
// with automatic retry on transient connection failures.
func (s *Server) proxyExecute(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	var result *codev0.CodeResponse
	err := s.withRetry(ctx, "Execute", func(code codev0.CodeClient) error {
		resp, err := code.Execute(ctx, req)
		if err != nil {
			return err
		}
		result = resp
		return nil
	})
	return result, err
}

// pluginToAgentName maps mind.yaml plugin names to agent identifiers
// understood by the agent manager.
// Accepts both formats: "go-generic" (canonical) and "generic-go" (legacy).
func pluginToAgentName(plugin string) string {
	switch plugin {
	case "go-generic", "generic-go":
		return "go-generic:latest"
	case "rust-generic", "generic-rust":
		return "rust-generic:latest"
	case "node-generic", "generic-node":
		return "node-generic:latest"
	case "python-generic", "generic-python":
		return "python-generic:latest"
	default:
		return plugin + ":latest"
	}
}

// serviceRoot returns the absolute path to the service source tree.
func (s *Server) serviceRoot() string {
	return s.cfg.WorkDir
}

// language returns the language string for the current service.
func (s *Server) language() string {
	return pluginToLang(s.mindYAML.Plugin)
}

// ─── Topology ────────────────────────────────────────────────

func (s *Server) ListServices(_ context.Context, _ *gatewayv1.ListServicesRequest) (*gatewayv1.ListServicesResponse, error) {
	my := s.mindYAML
	return &gatewayv1.ListServicesResponse{
		Services: []*gatewayv1.ServiceInfo{{
			Name:     my.Service,
			Language: pluginToLang(my.Plugin),
			Type:     my.Config.Type,
			Port:     int32(my.Config.Port),
		}},
	}, nil
}

// ═══════════════════════════════════════════════════════════════
// Plugin-proxied RPCs — all delegated via the unified Execute RPC.
// Each method wraps its gateway request into a CodeRequest oneof,
// calls proxyExecute, and unpacks the CodeResponse.
// ═══════════════════════════════════════════════════════════════

// ─── Code Intelligence (via plugin Execute → LSP) ────────────

func (s *Server) ListSymbols(ctx context.Context, req *gatewayv1.ListSymbolsRequest) (*gatewayv1.ListSymbolsResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ListSymbols{ListSymbols: &codev0.ListSymbolsRequest{File: req.File}},
	})
	if err != nil {
		return &gatewayv1.ListSymbolsResponse{Error: err.Error()}, nil
	}
	r := resp.GetListSymbols()
	if r.GetStatus() != nil && r.GetStatus().Message != "" {
		return &gatewayv1.ListSymbolsResponse{Error: r.GetStatus().Message}, nil
	}
	var symbols []*gatewayv1.Symbol
	for _, cs := range r.GetSymbols() {
		symbols = append(symbols, codeSymbolToGateway(cs, s.mindYAML.Service))
	}
	return &gatewayv1.ListSymbolsResponse{Symbols: symbols}, nil
}

func (s *Server) GetDiagnostics(ctx context.Context, req *gatewayv1.GetDiagnosticsRequest) (*gatewayv1.GetDiagnosticsResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetDiagnostics{GetDiagnostics: &codev0.GetDiagnosticsRequest{File: req.File}},
	})
	if err != nil {
		return &gatewayv1.GetDiagnosticsResponse{Error: err.Error()}, nil
	}
	r := resp.GetGetDiagnostics()
	var out []*gatewayv1.Diagnostic
	for _, d := range r.GetDiagnostics() {
		out = append(out, &gatewayv1.Diagnostic{
			File: d.File, Line: d.Line, Column: d.Column, EndLine: d.EndLine, EndColumn: d.EndColumn,
			Message: d.Message, Severity: diagSeverityToString(d.Severity), Source: d.Source, Code: d.Code,
		})
	}
	return &gatewayv1.GetDiagnosticsResponse{Diagnostics: out}, nil
}

func (s *Server) GoToDefinition(ctx context.Context, req *gatewayv1.GoToDefinitionRequest) (*gatewayv1.GoToDefinitionResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GoToDefinition{GoToDefinition: &codev0.GoToDefinitionRequest{
			File: req.File, Line: req.Line, Column: req.Column,
		}},
	})
	if err != nil {
		return &gatewayv1.GoToDefinitionResponse{Error: err.Error()}, nil
	}
	return &gatewayv1.GoToDefinitionResponse{Locations: codeLocsToGateway(resp.GetGoToDefinition().GetLocations())}, nil
}

func (s *Server) FindReferences(ctx context.Context, req *gatewayv1.FindReferencesRequest) (*gatewayv1.FindReferencesResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_FindReferences{FindReferences: &codev0.FindReferencesRequest{
			File: req.File, Line: req.Line, Column: req.Column,
		}},
	})
	if err != nil {
		return &gatewayv1.FindReferencesResponse{Error: err.Error()}, nil
	}
	return &gatewayv1.FindReferencesResponse{Locations: codeLocsToGateway(resp.GetFindReferences().GetLocations())}, nil
}

func (s *Server) RenameSymbol(ctx context.Context, req *gatewayv1.RenameSymbolRequest) (*gatewayv1.RenameSymbolResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_RenameSymbol{RenameSymbol: &codev0.RenameSymbolRequest{
			File: req.File, Line: req.Line, Column: req.Column, NewName: req.NewName,
		}},
	})
	if err != nil {
		return &gatewayv1.RenameSymbolResponse{Success: false, Error: err.Error()}, nil
	}
	r := resp.GetRenameSymbol()
	if !r.GetSuccess() {
		return &gatewayv1.RenameSymbolResponse{Success: false, Error: r.GetError()}, nil
	}
	var edits []*gatewayv1.TextEdit
	fileSet := make(map[string]bool)
	for _, e := range r.GetEdits() {
		edits = append(edits, &gatewayv1.TextEdit{
			File: e.File, StartLine: e.StartLine, StartColumn: e.StartColumn,
			EndLine: e.EndLine, EndColumn: e.EndColumn, NewText: e.NewText,
		})
		fileSet[e.File] = true
	}
	var files []string
	for f := range fileSet {
		files = append(files, f)
	}
	return &gatewayv1.RenameSymbolResponse{Success: true, Edits: edits, Files: files}, nil
}

func (s *Server) GetHoverInfo(ctx context.Context, req *gatewayv1.GetHoverInfoRequest) (*gatewayv1.GetHoverInfoResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetHoverInfo{GetHoverInfo: &codev0.GetHoverInfoRequest{
			File: req.File, Line: req.Line, Column: req.Column,
		}},
	})
	if err != nil {
		return &gatewayv1.GetHoverInfoResponse{Error: err.Error()}, nil
	}
	r := resp.GetGetHoverInfo()
	return &gatewayv1.GetHoverInfoResponse{Content: r.GetContent(), Language: r.GetLanguage()}, nil
}

// ─── Code Editing (via plugin Execute) ───────────────────────

func (s *Server) Fix(ctx context.Context, req *gatewayv1.FixRequest) (*gatewayv1.FixResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{File: req.Path}},
	})
	if err != nil {
		return &gatewayv1.FixResponse{Success: false, Error: err.Error()}, nil
	}
	r := resp.GetFix()
	return &gatewayv1.FixResponse{
		Success: r.GetSuccess(), Content: r.GetContent(), Error: r.GetError(), Actions: r.GetActions(),
	}, nil
}

func (s *Server) ApplyEdit(ctx context.Context, req *gatewayv1.ApplyEditRequest) (*gatewayv1.ApplyEditResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ApplyEdit{ApplyEdit: &codev0.ApplyEditRequest{
			File: req.File, Find: req.Find, Replace: req.Replace, AutoFix: req.AutoFix,
		}},
	})
	if err != nil {
		return nil, err
	}
	r := resp.GetApplyEdit()
	return &gatewayv1.ApplyEditResponse{
		Success: r.GetSuccess(), Content: r.GetContent(), Strategy: r.GetStrategy(),
		Error: r.GetError(), FixActions: r.GetFixActions(),
	}, nil
}

func (s *Server) BatchApplyEdits(ctx context.Context, req *gatewayv1.BatchApplyEditsRequest) (*gatewayv1.BatchApplyEditsResponse, error) {
	var results []*gatewayv1.EditResult
	var succeeded, failed int32

	for _, edit := range req.Edits {
		resp, err := s.ApplyEdit(ctx, edit)
		r := &gatewayv1.EditResult{Service: edit.Service, File: edit.File}
		if err != nil {
			r.Success = false
			r.Error = err.Error()
			failed++
		} else if !resp.Success {
			r.Success = false
			r.Error = resp.Error
			failed++
		} else {
			r.Success = true
			r.Strategy = resp.Strategy
			succeeded++
		}
		results = append(results, r)
	}
	return &gatewayv1.BatchApplyEditsResponse{Results: results, Succeeded: succeeded, Failed: failed}, nil
}

// ─── Dependencies (via plugin Execute) ───────────────────────

func (s *Server) ListDependencies(ctx context.Context, _ *gatewayv1.ListDependenciesRequest) (*gatewayv1.ListDependenciesResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_ListDependencies{ListDependencies: &codev0.ListDependenciesRequest{}},
	})
	if err != nil {
		return &gatewayv1.ListDependenciesResponse{Error: err.Error()}, nil
	}
	r := resp.GetListDependencies()
	var deps []*gatewayv1.Dependency
	for _, d := range r.GetDependencies() {
		deps = append(deps, &gatewayv1.Dependency{Name: d.Name, Version: d.Version, Direct: d.Direct})
	}
	return &gatewayv1.ListDependenciesResponse{Dependencies: deps, Error: r.GetError()}, nil
}

func (s *Server) AddDependency(ctx context.Context, req *gatewayv1.AddDependencyRequest) (*gatewayv1.AddDependencyResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_AddDependency{AddDependency: &codev0.AddDependencyRequest{
			PackageName: req.PackageName, Version: req.Version,
		}},
	})
	if err != nil {
		return &gatewayv1.AddDependencyResponse{Success: false, Error: err.Error()}, nil
	}
	r := resp.GetAddDependency()
	return &gatewayv1.AddDependencyResponse{Success: r.GetSuccess(), InstalledVersion: r.GetInstalledVersion(), Error: r.GetError()}, nil
}

func (s *Server) RemoveDependency(ctx context.Context, req *gatewayv1.RemoveDependencyRequest) (*gatewayv1.RemoveDependencyResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_RemoveDependency{RemoveDependency: &codev0.RemoveDependencyRequest{
			PackageName: req.PackageName,
		}},
	})
	if err != nil {
		return &gatewayv1.RemoveDependencyResponse{Success: false, Error: err.Error()}, nil
	}
	r := resp.GetRemoveDependency()
	return &gatewayv1.RemoveDependencyResponse{Success: r.GetSuccess(), Error: r.GetError()}, nil
}

// ─── Project Analysis ────────────────────────────────────────

func (s *Server) GetProjectInfo(ctx context.Context, _ *gatewayv1.GetProjectInfoRequest) (*gatewayv1.GetProjectInfoResponse, error) {
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoRequest{}},
	})
	if err != nil {
		return &gatewayv1.GetProjectInfoResponse{Error: err.Error()}, nil
	}
	pi := resp.GetGetProjectInfo()
	var pkgs []*gatewayv1.PackageInfo
	for _, p := range pi.GetPackages() {
		pkgs = append(pkgs, &gatewayv1.PackageInfo{
			Name: p.Name, RelativePath: p.RelativePath, Files: p.Files, Imports: p.Imports, Doc: p.Doc,
		})
	}
	var deps []*gatewayv1.Dependency
	for _, d := range pi.GetDependencies() {
		deps = append(deps, &gatewayv1.Dependency{Name: d.Name, Version: d.Version, Direct: d.Direct})
	}
	return &gatewayv1.GetProjectInfoResponse{
		Module: pi.GetModule(), Language: pi.GetLanguage(), LanguageVersion: pi.GetLanguageVersion(),
		Packages: pkgs, Dependencies: deps, FileHashes: pi.GetFileHashes(), Error: pi.GetError(),
	}, nil
}

// ═══════════════════════════════════════════════════════════════
// Gateway-direct RPCs — handled locally, not proxied to plugins.
// ═══════════════════════════════════════════════════════════════

// ─── Build / Lint / Test ─────────────────────────────────────

func (s *Server) Build(ctx context.Context, _ *gatewayv1.BuildRequest) (*gatewayv1.BuildResponse, error) {
	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return &gatewayv1.BuildResponse{Success: false, Output: fmt.Sprintf("plugin unavailable: %v", err)}, nil
	}
	resp, err := rt.Build(ctx, &runtimev0.BuildRequest{})
	if err != nil {
		return &gatewayv1.BuildResponse{Success: false, Output: fmt.Sprintf("plugin build RPC failed: %v", err)}, nil
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.BuildStatus_SUCCESS
	output := resp.Output
	if !success && resp.Status != nil {
		output = resp.Status.Message + "\n" + output
	}
	var buildErrors []*gatewayv1.BuildError
	if !success {
		buildErrors = parseBuildErrors(output)
	}
	return &gatewayv1.BuildResponse{Success: success, Errors: buildErrors, Output: output}, nil
}

func (s *Server) Lint(ctx context.Context, _ *gatewayv1.LintRequest) (*gatewayv1.LintResponse, error) {
	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return &gatewayv1.LintResponse{Success: false, Output: fmt.Sprintf("plugin unavailable: %v", err)}, nil
	}
	resp, err := rt.Lint(ctx, &runtimev0.LintRequest{})
	if err != nil {
		return &gatewayv1.LintResponse{Success: false, Output: fmt.Sprintf("plugin lint RPC failed: %v", err)}, nil
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.LintStatus_SUCCESS
	output := resp.Output
	if !success && resp.Status != nil {
		output = resp.Status.Message + "\n" + output
	}
	var lintErrors []*gatewayv1.BuildError
	if !success {
		lintErrors = parseBuildErrors(output)
	}
	return &gatewayv1.LintResponse{Success: success, Errors: lintErrors, Output: output}, nil
}

func (s *Server) Test(ctx context.Context, _ *gatewayv1.TestRequest) (*gatewayv1.TestResponse, error) {
	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return &gatewayv1.TestResponse{Success: false, Output: fmt.Sprintf("plugin unavailable: %v", err)}, nil
	}
	resp, err := rt.Test(ctx, &runtimev0.TestRequest{})
	if err != nil {
		return &gatewayv1.TestResponse{Success: false, Output: fmt.Sprintf("plugin test RPC failed: %v", err)}, nil
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.TestStatus_SUCCESS
	output := resp.Output
	if !success && resp.Status != nil {
		output = resp.Status.Message + "\n" + output
	}
	return &gatewayv1.TestResponse{
		Success:     success,
		Output:      output,
		TestsRun:    resp.TestsRun,
		TestsPassed: resp.TestsPassed,
		TestsFailed: resp.TestsFailed,
	}, nil
}

// ─── Execution ───────────────────────────────────────────────

func (s *Server) RunCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	timeout := 30 * time.Second
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir := s.serviceRoot()
	if req.WorkingDir != "" {
		dir = filepath.Join(dir, req.WorkingDir)
	}

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &gatewayv1.RunCommandResponse{ExitCode: -1, Stderr: err.Error()}, nil
		}
	}
	return &gatewayv1.RunCommandResponse{
		ExitCode: int32(exitCode),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

// ─── Plugin Commands ─────────────────────────────────────────

// ListAllCommands aggregates commands from all loaded plugin agents and returns
// them alongside built-in commands.
func (s *Server) ListAllCommands(ctx context.Context, _ *gatewayv1.ListAllCommandsRequest) (*gatewayv1.ListAllCommandsResponse, error) {
	var commands []*gatewayv1.AvailableCommand

	builtins := []*gatewayv1.AvailableCommand{
		{Name: "gs", Description: "Git status", Aliases: []string{"git status"}, Tags: []string{"git"}},
		{Name: "gd", Description: "Git diff", Aliases: []string{"git diff"}, Tags: []string{"git"}},
		{Name: "gl", Description: "Git log", Aliases: []string{"git log"}, Tags: []string{"git"}},
		{Name: "gc", Description: "Git commit", Aliases: []string{"git commit"}, Tags: []string{"git"}},
		{Name: "build", Description: "Build the project", Tags: []string{"build"}},
		{Name: "test", Description: "Run tests", Tags: []string{"test"}},
		{Name: "lint", Description: "Run linter", Tags: []string{"lint"}},
	}
	commands = append(commands, builtins...)

	// Aggregate commands from loaded plugin agents.
	s.pluginMu.Lock()
	plugins := make(map[string]*pluginConn, len(s.plugins))
	for k, v := range s.plugins {
		plugins[k] = v
	}
	s.pluginMu.Unlock()

	for svcName, pc := range plugins {
		if pc.agentSvc == nil {
			continue
		}
		resp, err := pc.agentSvc.ListCommands(ctx, &agentv0.ListCommandsRequest{})
		if err != nil {
			continue
		}
		for _, cmd := range resp.GetCommands() {
			commands = append(commands, &gatewayv1.AvailableCommand{
				Name:        cmd.GetName(),
				Description: cmd.GetDescription(),
				Aliases:     cmd.GetAliases(),
				Tags:        cmd.GetTags(),
				Plugin:      svcName,
			})
		}
	}

	return &gatewayv1.ListAllCommandsResponse{Commands: commands}, nil
}

// ─── Runtime / Checks ────────────────────────────────────────

func (s *Server) RunChecks(ctx context.Context, req *gatewayv1.RunChecksRequest) (*gatewayv1.RunChecksResponse, error) {
	var results []*gatewayv1.CheckResult
	serviceURL := ""
	if s.mindYAML.Config.Port > 0 {
		serviceURL = fmt.Sprintf("http://localhost:%d", s.mindYAML.Config.Port)
	}

	for _, ch := range req.Checks {
		var r *gatewayv1.CheckResult
		switch ct := ch.CheckType.(type) {
		case *gatewayv1.Check_Command:
			r = s.runCommandCheck(ctx, ch.Name, ct.Command)
		case *gatewayv1.Check_Http:
			r = s.runHTTPCheck(ctx, ch.Name, ct.Http, serviceURL)
		case *gatewayv1.Check_PluginBuild:
			_ = ct
			resp, err := s.Build(ctx, &gatewayv1.BuildRequest{})
			if err != nil {
				r = &gatewayv1.CheckResult{Name: ch.Name, Passed: false, Error: err.Error()}
			} else {
				r = &gatewayv1.CheckResult{Name: ch.Name, Passed: resp.Success, Output: resp.Output, Error: buildErrorsToString(resp.Errors)}
			}
		case *gatewayv1.Check_PluginTest:
			_ = ct
			resp, err := s.Test(ctx, &gatewayv1.TestRequest{})
			if err != nil {
				r = &gatewayv1.CheckResult{Name: ch.Name, Passed: false, Error: err.Error()}
			} else {
				r = &gatewayv1.CheckResult{Name: ch.Name, Passed: resp.Success, Output: resp.Output}
			}
		case *gatewayv1.Check_PluginLint:
			_ = ct
			resp, err := s.Lint(ctx, &gatewayv1.LintRequest{})
			if err != nil {
				r = &gatewayv1.CheckResult{Name: ch.Name, Passed: false, Error: err.Error()}
			} else {
				r = &gatewayv1.CheckResult{Name: ch.Name, Passed: resp.Success, Output: resp.Output, Error: buildErrorsToString(resp.Errors)}
			}
		default:
			r = &gatewayv1.CheckResult{Name: ch.Name, Passed: false, Error: "unknown check type"}
		}
		results = append(results, r)
	}
	return &gatewayv1.RunChecksResponse{Results: results}, nil
}

// ─── Version Control ─────────────────────────────────────────

func (s *Server) GitStatus(ctx context.Context, _ *gatewayv1.GitStatusRequest) (*gatewayv1.GitStatusResponse, error) {
	dir := s.serviceRoot()

	branchCmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = dir
	branchOut, _ := branchCmd.Output()
	branch := strings.TrimSpace(string(branchOut))

	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return &gatewayv1.GitStatusResponse{Error: fmt.Sprintf("git status: %v", err)}, nil
	}

	var files []*gatewayv1.GitFileStatus
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		path := strings.TrimSpace(line[3:])
		staged := xy[0] != ' ' && xy[0] != '?'
		status := gitStatusString(xy)
		files = append(files, &gatewayv1.GitFileStatus{
			Path: path, Status: status, Staged: staged,
		})
	}
	return &gatewayv1.GitStatusResponse{Files: files, Branch: branch}, nil
}

func (s *Server) GitDiff(ctx context.Context, req *gatewayv1.GitDiffRequest) (*gatewayv1.GitDiffResponse, error) {
	dir := s.serviceRoot()
	args := []string{"diff"}
	if req.Staged {
		args = append(args, "--cached")
	}
	if req.Path != "" {
		args = append(args, "--", req.Path)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return &gatewayv1.GitDiffResponse{Error: fmt.Sprintf("git diff: %v", err)}, nil
	}
	return &gatewayv1.GitDiffResponse{Diff: string(out)}, nil
}

func (s *Server) GitLog(ctx context.Context, req *gatewayv1.GitLogRequest) (*gatewayv1.GitLogResponse, error) {
	dir := s.serviceRoot()
	count := int(req.Count)
	if count <= 0 {
		count = 10
	}
	cmd := exec.CommandContext(ctx, "git", "log",
		fmt.Sprintf("--max-count=%d", count),
		"--format=%H|%h|%an|%s|%ai",
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return &gatewayv1.GitLogResponse{Error: fmt.Sprintf("git log: %v", err)}, nil
	}

	var commits []*gatewayv1.GitCommitInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 5)
		if len(parts) < 5 {
			continue
		}
		commits = append(commits, &gatewayv1.GitCommitInfo{
			Hash: parts[0], ShortHash: parts[1], Author: parts[2],
			Message: parts[3], Date: parts[4],
		})
	}
	return &gatewayv1.GitLogResponse{Commits: commits}, nil
}

func (s *Server) GitCommit(ctx context.Context, req *gatewayv1.GitCommitRequest) (*gatewayv1.GitCommitResponse, error) {
	dir := s.serviceRoot()

	if len(req.Paths) > 0 {
		args := append([]string{"add"}, req.Paths...)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return &gatewayv1.GitCommitResponse{Success: false, Error: fmt.Sprintf("git add: %s", string(out))}, nil
		}
	}

	cmd := exec.CommandContext(ctx, "git", "commit", "-m", req.Message)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &gatewayv1.GitCommitResponse{Success: false, Error: fmt.Sprintf("git commit: %s", string(out))}, nil
	}

	hashCmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	hashCmd.Dir = dir
	hashOut, _ := hashCmd.Output()
	return &gatewayv1.GitCommitResponse{
		Success: true,
		Hash:    strings.TrimSpace(string(hashOut)),
	}, nil
}

// ═══════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════

func (s *Server) runCommandCheck(ctx context.Context, name string, ch *gatewayv1.CommandCheck) *gatewayv1.CheckResult {
	cmd := exec.CommandContext(ctx, "sh", "-c", ch.Run)
	cmd.Dir = s.serviceRoot()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	combined := stdout.String() + stderr.String()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &gatewayv1.CheckResult{Name: name, Passed: false, Output: combined, Error: fmt.Sprintf("exec: %v", err)}
		}
	}
	if exitCode != int(ch.ExpectedExitCode) {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Output: combined, Error: fmt.Sprintf("exit code %d, expected %d", exitCode, ch.ExpectedExitCode)}
	}
	if ch.OutputContains != "" && !strings.Contains(combined, ch.OutputContains) {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Output: combined, Error: fmt.Sprintf("output does not contain %q", ch.OutputContains)}
	}
	return &gatewayv1.CheckResult{Name: name, Passed: true, Output: combined}
}

func (s *Server) runHTTPCheck(ctx context.Context, name string, ch *gatewayv1.HttpCheck, baseURL string) *gatewayv1.CheckResult {
	if baseURL == "" {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Error: "no service URL (service not running or no port in mind.yaml)"}
	}
	url := strings.TrimRight(baseURL, "/") + ch.Path
	var bodyReader io.Reader
	if ch.Body != "" {
		bodyReader = strings.NewReader(ch.Body)
	}
	req, err := http.NewRequestWithContext(ctx, ch.Method, url, bodyReader)
	if err != nil {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Error: fmt.Sprintf("build request: %v", err)}
	}
	if ch.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Error: fmt.Sprintf("http: %v", err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if ch.ExpectedStatus > 0 && resp.StatusCode != int(ch.ExpectedStatus) {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Output: bodyStr, Error: fmt.Sprintf("status %d, expected %d", resp.StatusCode, ch.ExpectedStatus)}
	}
	if ch.BodyContains != "" && !strings.Contains(bodyStr, ch.BodyContains) {
		return &gatewayv1.CheckResult{Name: name, Passed: false, Output: bodyStr, Error: fmt.Sprintf("body does not contain %q", ch.BodyContains)}
	}
	return &gatewayv1.CheckResult{Name: name, Passed: true, Output: bodyStr}
}

// prefixWriter wraps an io.Writer, prepending a fixed prefix to each
// line of output. It buffers partial lines until a newline arrives.
type prefixWriter struct {
	w      io.Writer
	prefix string
	buf    []byte
	mu     sync.Mutex
}

func newPrefixWriter(w io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{w: w, prefix: prefix}
}

func (pw *prefixWriter) Write(p []byte) (int, error) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	n := len(p)
	pw.buf = append(pw.buf, p...)
	for {
		idx := bytes.IndexByte(pw.buf, '\n')
		if idx < 0 {
			break
		}
		line := pw.buf[:idx]
		pw.buf = pw.buf[idx+1:]
		fmt.Fprintf(pw.w, "%s%s\n", pw.prefix, line)
	}
	return n, nil
}

// rpcLogInterceptor returns a gRPC unary server interceptor that logs
// every incoming RPC with method name, duration, and error status.
func rpcLogInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		dur := time.Since(start)

		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		// Short method name: "/mind.gateway.v1.Gateway/ReadFile" -> "ReadFile"
		method := info.FullMethod
		if idx := strings.LastIndex(method, "/"); idx >= 0 {
			method = method[idx+1:]
		}
		fmt.Printf("[gateway] %s %dms %s\n", method, dur.Milliseconds(), errStr)
		return resp, err
	}
}

func buildErrorsToString(errs []*gatewayv1.BuildError) string {
	if len(errs) == 0 {
		return ""
	}
	var msgs []string
	for _, e := range errs {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "\n")
}

func parseBuildErrors(output string) []*gatewayv1.BuildError {
	var errors []*gatewayv1.BuildError
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ":", 4)
		if len(parts) >= 4 {
			lineNum := parseInt(parts[1])
			col := parseInt(parts[2])
			if lineNum > 0 {
				errors = append(errors, &gatewayv1.BuildError{
					File:     parts[0],
					Line:     int32(lineNum),
					Column:   int32(col),
					Message:  strings.TrimSpace(parts[3]),
					Severity: "error",
				})
			}
		}
	}
	return errors
}

func parseInt(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func pluginToLang(plugin string) string {
	switch plugin {
	case "go-generic", "generic-go":
		return "go"
	case "rust-generic", "generic-rust":
		return "rust"
	case "node-generic", "generic-node":
		return "node"
	case "python-generic", "generic-python":
		return "python"
	default:
		return plugin
	}
}

func diagSeverityToString(sev codev0.DiagnosticSeverity) string {
	switch sev {
	case codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_ERROR:
		return "error"
	case codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_WARNING:
		return "warning"
	case codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_INFORMATION:
		return "information"
	case codev0.DiagnosticSeverity_DIAGNOSTIC_SEVERITY_HINT:
		return "hint"
	default:
		return "unknown"
	}
}

func codeSymbolToGateway(cs *codev0.Symbol, service string) *gatewayv1.Symbol {
	sym := &gatewayv1.Symbol{
		Name:          cs.GetName(),
		Kind:          symbolKindString(int32(cs.GetKind())),
		Signature:     cs.GetSignature(),
		Service:       service,
		Documentation: cs.GetDocumentation(),
		Parent:        cs.GetParent(),
	}
	if loc := cs.GetLocation(); loc != nil {
		sym.File = loc.GetFile()
		sym.Line = loc.GetLine()
	}
	return sym
}

func symbolKindString(kind int32) string {
	switch kind {
	case 1:
		return "function"
	case 2:
		return "method"
	case 3:
		return "struct"
	case 4:
		return "interface"
	case 5:
		return "constant"
	case 6:
		return "variable"
	case 7:
		return "type_alias"
	case 8:
		return "package"
	case 9:
		return "field"
	case 10:
		return "enum"
	case 11:
		return "class"
	default:
		return "unknown"
	}
}

func codeLocsToGateway(locs []*codev0.Location) []*gatewayv1.Location {
	var out []*gatewayv1.Location
	for _, l := range locs {
		out = append(out, &gatewayv1.Location{
			File:      l.File,
			Line:      l.Line,
			Column:    l.Column,
			EndLine:   l.EndLine,
			EndColumn: l.EndColumn,
		})
	}
	return out
}

func gitStatusString(xy string) string {
	if len(xy) < 2 {
		return "unknown"
	}
	switch {
	case xy == "??":
		return "untracked"
	case xy[0] == 'A' || xy[1] == 'A':
		return "added"
	case xy[0] == 'D' || xy[1] == 'D':
		return "deleted"
	case xy[0] == 'R' || xy[1] == 'R':
		return "renamed"
	case xy[0] == 'C' || xy[1] == 'C':
		return "copied"
	case xy[0] == 'M' || xy[1] == 'M':
		return "modified"
	default:
		return "modified"
	}
}

func parseGoDeps(jsonOutput string) []*gatewayv1.Dependency {
	type goMod struct {
		Path    string `json:"Path"`
		Version string `json:"Version"`
		Main    bool   `json:"Main"`
		Dir     string `json:"Dir"`
	}

	var deps []*gatewayv1.Dependency
	decoder := json.NewDecoder(strings.NewReader(jsonOutput))
	for {
		var m goMod
		if err := decoder.Decode(&m); err != nil {
			break
		}
		if m.Main {
			continue
		}
		deps = append(deps, &gatewayv1.Dependency{
			Name: m.Path, Version: m.Version, Direct: m.Dir != "",
		})
	}
	return deps
}

// ─── Port file management ────────────────────────────────────

const portFileName = "gateway.port"

func portFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".codefly")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, portFileName), nil
}

func writePortFile(port int) error {
	path, err := portFilePath()
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", port)), 0o644)
}

// ReadPortFile reads the gateway port from the port file.
func ReadPortFile() (int, error) {
	path, err := portFilePath()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseInt(strings.TrimSpace(string(data))), nil
}

// RemovePortFile removes the gateway port file.
func RemovePortFile() error {
	path, err := portFilePath()
	if err != nil {
		return err
	}
	return os.Remove(path)
}
