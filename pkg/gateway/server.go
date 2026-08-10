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
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	publishcmd "github.com/codefly-dev/cli/cmd/publish"
	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/executionrecorder"
	codecore "github.com/codefly-dev/core/code"
	"github.com/codefly-dev/core/failures"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	executionv1 "github.com/codefly-dev/core/generated/go/codefly/execution/v1"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	githubtoolbox "github.com/codefly-dev/core/toolbox/github"
	"github.com/codefly-dev/core/wool"
	wotel "github.com/codefly-dev/core/wool/otel"
	codefly "github.com/codefly-dev/sdk-go"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"
)

// Config holds the Gateway server configuration.
type Config struct {
	WorkDir string // directory containing mind.yaml
	Port    int    // gRPC listen port
	// WorkspaceHost, when set, is an externally owned runtime owner this
	// gateway binds to instead of creating its own via
	// engine.NewWorkspaceHost. The caller keeps ownership: Close() will not
	// tear it down. Used when one process constructs a Gateway per repository
	// (control.NewWithHost's pattern) and must not spawn N duplicate agent
	// process pools for N repos.
	WorkspaceHost *engine.WorkspaceHost
	// Host is the bind interface. Empty defaults to "127.0.0.1" (local-only,
	// the safe default). Set to "0.0.0.0" to expose the gateway over the
	// network — required when the gateway runs inside a container that a
	// remote Mind connects to (the codefly-in-Docker / SaaS data-plane model).
	Host string
	// Token is required for every RPC when set, and is mandatory for any
	// non-loopback bind. Clients send it as "authorization: Bearer <token>".
	Token string
	// GitHubToken is passed only to the GitHub forge toolbox. Empty uses the
	// execution box's GITHUB_TOKEN, matching the standalone toolbox.
	GitHubToken string
	// TLSCertFile and TLSKeyFile configure server TLS. Both are mandatory for
	// non-loopback binds so bearer credentials and privileged RPC payloads are
	// never sent in clear text.
	TLSCertFile string
	TLSKeyFile  string
	// TLSClientCAFile optionally enables mutual TLS. When set, clients must
	// present a certificate signed by this CA in addition to the bearer token.
	TLSClientCAFile string
	// ExecutionRecorder is an optional product-neutral governed-execution
	// boundary. When absent, legacy requests without an SDK execution carrier
	// continue to work; a request that supplies authority fails closed rather
	// than silently discarding it.
	ExecutionRecorder ExecutionRecorder
	// ExecutionDispatcher delivers the recorder's durable outbox to installed
	// product-neutral exporter plugins. Exporter failure never changes the
	// underlying effect or receipt production.
	ExecutionDispatcher ExecutionDispatcher
}

// ExecutionRecorder is the narrow neutral lifecycle capability used by the
// Gateway. Warden and every other exporter stay behind Codefly's plugin API.
type ExecutionRecorder interface {
	Begin(context.Context, codefly.ExecutionContext, executionrecorder.BeginInput) (executionrecorder.BeginResult, error)
	RecoverIncomplete(context.Context, int) (int, error)
}

// ExecutionDispatcher is the lifecycle surface for additive exporter plugins.
type ExecutionDispatcher interface {
	Run(context.Context) error
}

// bindHost returns the interface to listen on, defaulting to local-only.
func (c Config) bindHost() string {
	if strings.TrimSpace(c.Host) == "" {
		return "127.0.0.1"
	}
	return c.Host
}

// Server implements the Gateway gRPC service.
type Server struct {
	gatewayv1.UnimplementedGatewayServer
	cfg       Config
	mindYAML  *MindYAML
	grpcSrv   *grpc.Server
	tlsConfig *tls.Config
	host      *engine.WorkspaceHost
	// ownsHost is false when cfg.WorkspaceHost was supplied externally: Close
	// must leave that host running for its owner.
	ownsHost bool

	serviceMu       sync.Mutex
	serviceBehavior serviceExecution
	// rootAgentServices isolates explicit formula routing from the source-derived
	// root behavior. A markerless workspace may bind the generic agent first;
	// that cache must never capture a later request carrying typed language
	// evidence.
	rootAgentServices map[string]serviceExecution
	// codeUnitServices caches exact source-boundary behaviors separately from
	// the legacy root service. Each entry owns its own plugin selection and
	// working directory; heterogeneous repositories must never share the first
	// detected root agent.
	codeUnitServices map[string]serviceExecution

	// terminals holds the PTY-backed interactive shells running in this gateway
	// (the gateway IS inside the execution box, so the PTY lives here).
	terminals *terminalManager

	workspaceChangesMu     sync.Mutex
	workspaceChanges       *codecore.WorkspaceChangeMonitor
	workspaceChangesClosed bool

	// ARCHITECTURE: prepared writes are the SaaS mutation boundary. Mind can
	// configure one coordinator trust anchor for one workspace, but cannot swap
	// it after work begins. The apply mutex makes target-hash verification and
	// the resulting write one local critical section.
	mutationAuthorityMu sync.RWMutex
	mutationAuthority   *mutationAuthorityBinding
	preparedMutationMu  sync.Mutex
	preparedMutations   map[string]*storedPreparedMutation
	preparedApplyMu     sync.Mutex

	executionRecorder   ExecutionRecorder
	executionDispatcher ExecutionDispatcher
}

// serviceExecution is the transport-independent behavior consumed by the
// Gateway adapter. engine.Service is the production implementation.
type serviceExecution interface {
	ExecuteCode(context.Context, *codev0.CodeRequest) (*codev0.CodeResponse, error)
	GetSemanticIndex(context.Context, *toolingv0.GetSemanticIndexRequest) (*toolingv0.GetSemanticIndexResponse, error)
	Build(context.Context, *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error)
	Test(context.Context, *runtimev0.TestRequest) (*runtimev0.TestResponse, error)
	Lint(context.Context, *runtimev0.LintRequest) (*runtimev0.LintResponse, error)
	ListCommands(context.Context, *agentv0.ListCommandsRequest) (*agentv0.ListCommandsResponse, error)
}

// serviceConfigurator is an optional mutation surface kept separate from the
// common execution interface. Existing read/test-only execution behaviors do
// not pretend to support plugin-owned configuration.
type serviceConfigurator interface {
	Configure(context.Context, *builderv0.ConfigureRequest) (*builderv0.ConfigureResponse, error)
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
	remote := !isLoopbackHost(cfg.bindHost())
	if remote && strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("gateway authentication token is required for non-loopback host %q", cfg.bindHost())
	}
	tlsConfig, err := loadGatewayTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	if remote && tlsConfig == nil {
		return nil, fmt.Errorf("gateway TLS certificate and key are required for non-loopback host %q", cfg.bindHost())
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		cfg.WorkDir = "."
	}
	absWorkDir, err := filepath.Abs(cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve gateway work directory: %w", err)
	}
	cfg.WorkDir = filepath.Clean(absWorkDir)
	host := cfg.WorkspaceHost
	ownsHost := false
	if host == nil {
		var err error
		host, err = engine.NewWorkspaceHost(engine.Config{
			Root:      cfg.WorkDir,
			LogWriter: os.Stderr,
		})
		if err != nil {
			return nil, fmt.Errorf("create workspace host: %w", err)
		}
		ownsHost = true
	} else if !withinRoot(host.Root(), cfg.WorkDir) {
		// Service()/normalizeTarget would otherwise fail this late and
		// opaquely the first time an RPC actually binds a service, deep
		// inside engine — catch a caller's root/WorkDir mismatch here instead.
		return nil, fmt.Errorf("gateway work directory %s is not within the supplied workspace host root %s", cfg.WorkDir, host.Root())
	}
	s := &Server{
		cfg:                 cfg,
		tlsConfig:           tlsConfig,
		host:                host,
		ownsHost:            ownsHost,
		rootAgentServices:   make(map[string]serviceExecution),
		codeUnitServices:    make(map[string]serviceExecution),
		preparedMutations:   make(map[string]*storedPreparedMutation),
		terminals:           newTerminalManager(),
		executionRecorder:   cfg.ExecutionRecorder,
		executionDispatcher: cfg.ExecutionDispatcher,
	}

	path := filepath.Join(cfg.WorkDir, "mind.yaml")
	data, err := os.ReadFile(path)
	if err == nil {
		var my MindYAML
		if parseErr := yaml.Unmarshal(data, &my); parseErr != nil {
			if ownsHost {
				_ = host.Close()
			}
			return nil, fmt.Errorf("parse mind.yaml: %w", parseErr)
		}
		s.mindYAML = &my
	}
	// No mind.yaml is fine — the gateway still serves basic RPCs (git, shell)
	// and will report a clear error for plugin-dependent operations.

	return s, nil
}

func loadGatewayTLSConfig(cfg Config) (*tls.Config, error) {
	certFile := strings.TrimSpace(cfg.TLSCertFile)
	keyFile := strings.TrimSpace(cfg.TLSKeyFile)
	caFile := strings.TrimSpace(cfg.TLSClientCAFile)
	if certFile == "" && keyFile == "" {
		if caFile != "" {
			return nil, fmt.Errorf("gateway TLS client CA requires a server certificate and key")
		}
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("gateway TLS certificate and key must be configured together")
	}

	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load gateway TLS certificate: %w", err)
	}
	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
	}
	if caFile == "" {
		return tlsConfig, nil
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read gateway TLS client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse gateway TLS client CA %s: no certificates found", caFile)
	}
	tlsConfig.ClientCAs = clientCAs
	tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	return tlsConfig, nil
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
	lifecycleContext, cancelLifecycle := context.WithCancel(ctx)
	var dispatcherWG sync.WaitGroup
	defer dispatcherWG.Wait()
	defer cancelLifecycle()

	if _, err := wotel.Enable(
		wotel.WithServiceName("codefly-gateway"),
	); err != nil {
		w.Warn("OTEL init failed (tracing disabled)", wool.ErrField(err))
	}
	if s.executionRecorder != nil {
		recovered, err := s.executionRecorder.RecoverIncomplete(ctx, 1000)
		if err != nil {
			return fmt.Errorf("recover incomplete execution receipts before serving: %w", err)
		}
		if recovered > 0 {
			w.Warn("recovered incomplete execution attempts as uncertain", wool.Field("count", recovered))
		}
	}
	if s.executionDispatcher != nil {
		dispatcherWG.Add(1)
		go func() {
			defer dispatcherWG.Done()
			if err := s.executionDispatcher.Run(lifecycleContext); err != nil && lifecycleContext.Err() == nil {
				w.Error("execution receipt dispatcher stopped", wool.ErrField(err))
			}
		}()
	}

	addr := net.JoinHostPort(s.cfg.bindHost(), fmt.Sprintf("%d", s.cfg.Port))
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	serverOptions := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(gatewayAuthUnaryInterceptor(s.cfg.Token), rpcLogInterceptor()),
		grpc.StreamInterceptor(gatewayAuthStreamInterceptor(s.cfg.Token)),
	}
	if s.tlsConfig != nil {
		serverOptions = append(serverOptions, grpc.Creds(credentials.NewTLS(s.tlsConfig.Clone())))
	}
	s.grpcSrv = grpc.NewServer(serverOptions...)
	gatewayv1.RegisterGatewayServer(s.grpcSrv, s)

	if err := writePortFile(s.cfg.Port); err != nil {
		_ = lis.Close()
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
		_ = s.Close()
		s.grpcSrv.GracefulStop()
	}()

	return s.grpcSrv.Serve(lis)
}

func (s *Server) executionServiceBehavior() (serviceExecution, error) {
	return s.executionServiceBehaviorWithAgent("")
}

func (s *Server) executionServiceBehaviorWithAgent(agentOverride string) (serviceExecution, error) {
	s.serviceMu.Lock()
	defer s.serviceMu.Unlock()
	if agentOverride == "" && s.serviceBehavior != nil {
		return s.serviceBehavior, nil
	}
	if s.host == nil {
		return nil, fmt.Errorf("workspace host is unavailable")
	}
	name := filepath.Base(s.cfg.WorkDir)
	agentName := ""
	if s.mindYAML != nil {
		name = s.mindYAML.Service
		agentName = pluginToAgentName(s.mindYAML.Plugin)
	} else if agentOverride != "" {
		agentName = agentOverride
	} else {
		var err error
		agentName, err = engine.DetectSourceAgent(s.cfg.WorkDir)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	if agentOverride != "" && s.mindYAML == nil {
		if service := s.rootAgentServices[agentName]; service != nil {
			return service, nil
		}
	}
	service, err := s.host.Service(engine.ServiceTarget{
		Name:  name,
		Root:  s.cfg.WorkDir,
		Agent: agentName,
	})
	if err != nil {
		return nil, fmt.Errorf("bind gateway service: %w", err)
	}
	if agentOverride != "" && s.mindYAML == nil {
		if s.rootAgentServices == nil {
			s.rootAgentServices = make(map[string]serviceExecution)
		}
		s.rootAgentServices[agentName] = service
	} else {
		s.serviceBehavior = service
	}
	return service, nil
}

func (s *Server) sourceExecute(ctx context.Context, request *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	if s.host == nil || s.host.Source() == nil {
		return nil, fmt.Errorf("workspace source behavior is unavailable")
	}
	return s.host.Source().ExecuteCode(ctx, request)
}

// proxyExecute sends a unified CodeRequest to the shared service behavior.
// Read-only requests may reconnect once; ambiguous mutations are never replayed.
func (s *Server) proxyExecute(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	service, err := s.executionServiceBehavior()
	if err != nil {
		return nil, err
	}
	return service.ExecuteCode(ctx, req)
}

func codeFailureMessage(response *codev0.CodeResponse) string {
	if response == nil || response.GetFailure() == nil {
		return ""
	}
	return response.GetFailure().GetMessage()
}

// pluginToAgentName maps mind.yaml plugin names to agent identifiers
// understood by the agent manager.
// Accepts both formats: "go-generic" (canonical) and "generic-go" (legacy).
func pluginToAgentName(plugin string) string {
	switch plugin {
	case "go-generic", "generic-go":
		return "go:latest"
	case "rust-generic", "generic-rust":
		return "rust:latest"
	case "node-generic", "generic-node":
		return "nextjs:latest"
	case "python-generic", "generic-python":
		return "python:latest"
	default:
		return plugin + ":latest"
	}
}

// serviceRoot returns the absolute path to the service source tree.
func (s *Server) serviceRoot() string {
	return s.cfg.WorkDir
}

// controlScope returns a control-plane handle scoped to this gateway's service,
// rooted at the service source tree. The Gateway is a thin adapter: generic
// operations (git today) delegate to the one control plane rather than
// re-implementing them. It uses the dir-based constructor because the Gateway
// may run without a surrounding workspace (the codefly-in-Docker model).
func (s *Server) controlScope() control.ServiceScope {
	name := ""
	if s.mindYAML != nil {
		name = s.mindYAML.Service
	}
	return control.ServiceScopeAt(name, s.serviceRoot())
}

func (s *Server) fileOps() codecore.FileOperation {
	return codecore.NewFileOps(codecore.LocalVFS{}, s.serviceRoot())
}

func (s *Server) validateService(service string) error {
	service = strings.TrimSpace(service)
	if service == "" || s.mindYAML == nil {
		return nil
	}
	if service != s.mindYAML.Service {
		return status.Errorf(codes.NotFound, "service %q not found in gateway workspace", service)
	}
	return nil
}

// withinRoot reports whether abs (an absolute, cleaned path) is root itself
// or a descendant of it.
func withinRoot(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func cleanGatewayPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.ContainsRune(p, '\x00') {
		return "", fmt.Errorf("path contains NUL byte")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be relative: %s", p)
	}
	clean := filepath.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path escapes workspace: %s", p)
	}
	return clean, nil
}

func (s *Server) resolveWorkspacePath(rel string) (root, abs string, err error) {
	root = filepath.Clean(s.serviceRoot())
	abs = filepath.Join(root, rel)
	check, err := filepath.Rel(root, abs)
	if err != nil {
		return "", "", err
	}
	if check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) || filepath.IsAbs(check) {
		return "", "", fmt.Errorf("path escapes workspace: %s", rel)
	}
	return root, abs, nil
}

func (s *Server) resolveCommandDir(rel string) (string, error) {
	clean, err := cleanGatewayPath(rel)
	if err != nil {
		return "", err
	}
	root, abs, err := s.resolveWorkspacePath(clean)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	check, err := filepath.Rel(realRoot, realDir)
	if err != nil || check == ".." || strings.HasPrefix(check, ".."+string(filepath.Separator)) || filepath.IsAbs(check) {
		return "", fmt.Errorf("working directory escapes workspace: %s", rel)
	}
	info, err := os.Stat(realDir)
	if err != nil {
		return "", fmt.Errorf("stat working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory: %s", rel)
	}
	return realDir, nil
}

func shouldSkipGatewayDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "__pycache__", "dist", "build", "target", ".cache":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

// language returns the language string for the current service.
func (s *Server) language() string {
	return pluginToLang(s.mindYAML.Plugin)
}

// ─── Topology ────────────────────────────────────────────────

func (s *Server) ListServices(_ context.Context, _ *gatewayv1.ListServicesRequest) (*gatewayv1.ListServicesResponse, error) {
	my := s.mindYAML
	// The server starts successfully without mind.yaml (plugin-dependent RPCs
	// fail individually); topology must degrade the same way, not panic. A
	// remote Mind probes ListServices at startup before any workspace is
	// configured, so a nil mindYAML is a normal state, not an error.
	if my == nil {
		return &gatewayv1.ListServicesResponse{}, nil
	}
	return &gatewayv1.ListServicesResponse{
		Services: []*gatewayv1.ServiceInfo{{
			Name:     my.Service,
			Language: pluginToLang(my.Plugin),
			Type:     my.Config.Type,
			Port:     int32(my.Config.Port),
		}},
	}, nil
}

// ─── File Operations (direct workspace I/O) ──────────────────

func (s *Server) ReadFile(ctx context.Context, req *gatewayv1.ReadFileRequest) (*gatewayv1.ReadFileResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	data, err := s.controlScope().ReadFile(ctx, rel)
	if err != nil {
		if os.IsNotExist(err) {
			return &gatewayv1.ReadFileResponse{Exists: false}, nil
		}
		displayPath := rel
		if displayPath == "" {
			displayPath = "."
		}
		return nil, status.Errorf(codes.Internal, "read %s: %s", displayPath, gatewayErrorMessage(s.serviceRoot(), err))
	}
	return &gatewayv1.ReadFileResponse{Content: string(data), Exists: true}, nil
}

func (s *Server) WriteFile(ctx context.Context, req *gatewayv1.WriteFileRequest) (*gatewayv1.WriteFileResponse, error) {
	if err := validateOptionalExecutionContext(ctx); err != nil {
		return nil, err
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return &gatewayv1.WriteFileResponse{Success: false, Error: err.Error()}, nil
	}
	if err := s.controlScope().WriteFile(ctx, rel, []byte(req.GetContent())); err != nil {
		return &gatewayv1.WriteFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.WriteFileResponse{Success: true}, nil
}

func (s *Server) ListFiles(ctx context.Context, req *gatewayv1.ListFilesRequest) (*gatewayv1.ListFilesResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	root, base, err := s.resolveWorkspacePath(rel)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	extSet := make(map[string]bool, len(req.GetExtensions()))
	for _, ext := range req.GetExtensions() {
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		extSet[ext] = true
	}

	var files []*gatewayv1.FileInfo
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && path != base && shouldSkipGatewayDir(d.Name()) {
			return filepath.SkipDir
		}
		if !req.GetRecursive() && d.IsDir() && path != base {
			return filepath.SkipDir
		}
		if path == base {
			return nil
		}
		if len(extSet) > 0 && !d.IsDir() && !extSet[filepath.Ext(path)] {
			return nil
		}
		info, _ := d.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		itemRel, _ := filepath.Rel(root, path)
		files = append(files, &gatewayv1.FileInfo{
			Path:        filepath.ToSlash(itemRel),
			SizeBytes:   size,
			IsDirectory: d.IsDir(),
		})
		return nil
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list files under %s: %v", rel, err)
	}
	return &gatewayv1.ListFilesResponse{Files: files}, nil
}

// SubscribeWorkspaceChangeEvents is the in-process Codefly capability used by
// local Gateway adapters. Observation, sequencing, replay, and overflow
// handling remain owned by Codefly even when the transport hop is elided.
func (s *Server) SubscribeWorkspaceChangeEvents(ctx context.Context, cursor codecore.WorkspaceChangeCursor) (*codecore.WorkspaceChangeSubscription, error) {
	monitor, err := s.workspaceChangeMonitor()
	if err != nil {
		return nil, err
	}
	return monitor.Subscribe(ctx, cursor)
}

func (s *Server) workspaceChangeMonitor() (*codecore.WorkspaceChangeMonitor, error) {
	s.workspaceChangesMu.Lock()
	defer s.workspaceChangesMu.Unlock()
	if s.workspaceChangesClosed {
		return nil, codecore.ErrWorkspaceChangeMonitorClosed
	}
	if s.workspaceChanges != nil {
		return s.workspaceChanges, nil
	}
	monitor, err := codecore.NewWorkspaceChangeMonitor(s.serviceRoot())
	if err != nil {
		return nil, err
	}
	s.workspaceChanges = monitor
	return monitor, nil
}

// CloseWorkspaceChanges releases the lazily-created recursive watcher. It is
// idempotent and is called by both the gRPC server lifecycle and local adapters.
func (s *Server) CloseWorkspaceChanges() error {
	s.workspaceChangesMu.Lock()
	monitor := s.workspaceChanges
	s.workspaceChanges = nil
	s.workspaceChangesClosed = true
	s.workspaceChangesMu.Unlock()
	if monitor == nil {
		return nil
	}
	return monitor.Close()
}

// Close releases every lifecycle-owned resource used by an in-process or gRPC
// Gateway. It is safe to call more than once.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	workspaceErr := s.CloseWorkspaceChanges()
	if s.terminals != nil {
		s.terminals.close()
	}
	var hostErr error
	if s.host != nil && s.ownsHost {
		hostErr = s.host.Close()
	}
	return errors.Join(workspaceErr, hostErr)
}

func (s *Server) SubscribeWorkspaceChanges(req *gatewayv1.SubscribeWorkspaceChangesRequest, stream grpc.ServerStreamingServer[gatewayv1.WorkspaceChangeEvent]) error {
	if req == nil || stream == nil {
		return status.Error(codes.InvalidArgument, "workspace change request and stream are required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return err
	}
	cursor := codecore.WorkspaceChangeCursor{}
	if after := req.GetAfter(); after != nil {
		cursor.SourceID = after.GetSourceId()
		cursor.Sequence = after.GetSequence()
	}
	subscription, err := s.SubscribeWorkspaceChangeEvents(stream.Context(), cursor)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "subscribe workspace changes: %v", err)
	}
	defer subscription.Close()
	for {
		event, err := subscription.Recv()
		if err != nil {
			if stream.Context().Err() != nil {
				return nil
			}
			return status.Errorf(codes.Unavailable, "workspace change stream: %v", err)
		}
		response := gatewayWorkspaceChangeEvent(event)
		if err := stream.Send(response); err != nil {
			return err
		}
	}
}

func gatewayWorkspaceChangeEvent(event codecore.WorkspaceChangeEvent) *gatewayv1.WorkspaceChangeEvent {
	changes := make([]*gatewayv1.WorkspaceChange, 0, len(event.Changes))
	for _, change := range event.Changes {
		changes = append(changes, &gatewayv1.WorkspaceChange{
			Operation: gatewayWorkspaceChangeOperation(change.Kind),
			Path:      change.Path, PreviousPath: change.PreviousPath, Reason: change.Reason,
		})
	}
	return &gatewayv1.WorkspaceChangeEvent{
		SourceId: event.SourceID, Sequence: event.Sequence,
		ObservedAt: timestamppb.New(event.ObservedAt), Changes: changes,
	}
}

func gatewayWorkspaceChangeOperation(kind codecore.WorkspaceChangeKind) gatewayv1.WorkspaceChangeOperation {
	switch kind {
	case codecore.WorkspaceChangeCreate:
		return gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_CREATE
	case codecore.WorkspaceChangeWrite:
		return gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_WRITE
	case codecore.WorkspaceChangeRemove:
		return gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_REMOVE
	case codecore.WorkspaceChangeMetadata:
		return gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_METADATA
	case codecore.WorkspaceChangeRescan:
		return gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_RESCAN
	default:
		return gatewayv1.WorkspaceChangeOperation_WORKSPACE_CHANGE_OPERATION_UNSPECIFIED
	}
}

func (s *Server) DeleteFile(ctx context.Context, req *gatewayv1.DeleteFileRequest) (*gatewayv1.DeleteFileResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return &gatewayv1.DeleteFileResponse{Success: false, Error: err.Error()}, nil
	}
	if err := s.controlScope().DeleteFile(ctx, rel); err != nil {
		if os.IsNotExist(err) {
			return &gatewayv1.DeleteFileResponse{Success: false, Error: "file not found"}, nil
		}
		return &gatewayv1.DeleteFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.DeleteFileResponse{Success: true}, nil
}

func (s *Server) MoveFile(ctx context.Context, req *gatewayv1.MoveFileRequest) (*gatewayv1.MoveFileResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	oldRel, err := cleanGatewayPath(req.GetOldPath())
	if err != nil {
		return &gatewayv1.MoveFileResponse{Success: false, Error: err.Error()}, nil
	}
	newRel, err := cleanGatewayPath(req.GetNewPath())
	if err != nil {
		return &gatewayv1.MoveFileResponse{Success: false, Error: err.Error()}, nil
	}
	if err := s.controlScope().MoveFile(ctx, oldRel, newRel); err != nil {
		return &gatewayv1.MoveFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.MoveFileResponse{Success: true}, nil
}

func (s *Server) CreateFile(ctx context.Context, req *gatewayv1.CreateFileRequest) (*gatewayv1.CreateFileResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return &gatewayv1.CreateFileResponse{Success: false, Error: err.Error()}, nil
	}
	sc := s.controlScope()
	if !req.GetOverwrite() {
		if _, err := sc.ReadFile(ctx, rel); err == nil {
			return &gatewayv1.CreateFileResponse{Success: false, Error: "file already exists"}, nil
		} else if !os.IsNotExist(err) {
			return &gatewayv1.CreateFileResponse{Success: false, Error: err.Error()}, nil
		}
	}
	if err := sc.WriteFile(ctx, rel, []byte(req.GetContent())); err != nil {
		return &gatewayv1.CreateFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.CreateFileResponse{Success: true}, nil
}

// ═══════════════════════════════════════════════════════════════
// Plugin-proxied RPCs — all delegated via the unified Execute RPC.
// Each method wraps its gateway request into a CodeRequest oneof,
// calls proxyExecute, and unpacks the CodeResponse.
// ═══════════════════════════════════════════════════════════════

// ─── Code Editing (via plugin Execute) ───────────────────────

func (s *Server) Fix(ctx context.Context, req *gatewayv1.FixRequest) (*gatewayv1.FixResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return &gatewayv1.FixResponse{Success: false, Error: err.Error()}, nil
	}
	resp, err := s.proxyExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_Fix{Fix: &codev0.FixRequest{File: rel, Mode: req.GetMode(), DryRun: req.GetDryRun()}},
	})
	if err != nil {
		return &gatewayv1.FixResponse{Success: false, Error: err.Error()}, nil
	}
	r := resp.GetFix()
	if r == nil {
		return &gatewayv1.FixResponse{Success: false, Error: codeFailureMessage(resp)}, nil
	}
	return &gatewayv1.FixResponse{
		Success: r.GetSuccess(), Content: r.GetContent(), Error: codeFailureMessage(resp), Actions: r.GetActions(),
		Changed: r.GetChanged(), BeforeSha256: r.GetBeforeSha256(), AfterSha256: r.GetAfterSha256(),
		Wrote: r.GetWrote(), Output: r.GetOutput(),
		BeforeSizeBytes: r.GetBeforeSizeBytes(), AfterSizeBytes: r.GetAfterSizeBytes(),
	}, nil
}

func (s *Server) ApplyEdit(ctx context.Context, req *gatewayv1.ApplyEditRequest) (*gatewayv1.ApplyEditResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetFile())
	if err != nil {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: err.Error()}, nil
	}
	if req.GetDryRun() {
		if contextErr := validateOptionalExecutionContext(ctx); contextErr != nil {
			return nil, contextErr
		}
	} else {
		operationInputSHA256, digestErr := deterministicProtoSHA256(&codev0.ApplyEditRequest{
			File: rel, Find: req.GetFind(), Replace: req.GetReplace(), FixMode: req.GetFixMode(), DryRun: false,
		})
		if digestErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "encode apply-edit input: %v", digestErr)
		}
		var beforeSHA256 string
		if content, readErr := s.fileOps().ReadFile(ctx, rel); readErr == nil {
			digest := sha256.Sum256(content)
			beforeSHA256 = hex.EncodeToString(digest[:])
		}
		attempt, _, beginErr := s.beginGovernedExecution(ctx, executionrecorder.BeginInput{
			OperationKind:        "code.apply-edit",
			OperationInputSHA256: operationInputSHA256,
			Assurance:            executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_PLUGIN_EXECUTED,
			Target:               executionTarget(s.executionService(req.GetService())),
			Resources: []*executionv1.ExecutionResourceV1{
				pathExecutionResource(rel, beforeSHA256, "", false),
			},
		})
		if beginErr != nil {
			return nil, beginErr
		}
		if attempt != nil {
			return s.applyEditWithReceipt(ctx, req, rel, beforeSHA256, attempt)
		}
	}
	return s.applyEdit(ctx, req, rel)
}

func (s *Server) applyEdit(
	ctx context.Context,
	req *gatewayv1.ApplyEditRequest,
	rel string,
) (*gatewayv1.ApplyEditResponse, error) {
	execute := s.proxyExecute
	if s.mindYAML == nil {
		execute = s.sourceExecute
	}
	resp, err := execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_ApplyEdit{ApplyEdit: &codev0.ApplyEditRequest{
		File: rel, Find: req.GetFind(), Replace: req.GetReplace(), FixMode: req.GetFixMode(), DryRun: req.GetDryRun(),
	}}})
	if err != nil {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: err.Error()}, nil
	}
	result := resp.GetApplyEdit()
	if result == nil {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: codeFailureMessage(resp)}, nil
	}
	return &gatewayv1.ApplyEditResponse{
		Success: result.GetSuccess(), Content: result.GetContent(), Error: codeFailureMessage(resp),
		Strategy: result.GetStrategy(), FixActions: result.GetFixActions(), Changed: result.GetChanged(),
		BeforeSha256: result.GetBeforeSha256(), AfterSha256: result.GetAfterSha256(),
		Wrote: result.GetWrote(), Output: result.GetOutput(),
		BeforeSizeBytes: result.GetBeforeSizeBytes(), AfterSizeBytes: result.GetAfterSizeBytes(),
	}, nil
}

func (s *Server) applyEditWithReceipt(
	ctx context.Context,
	req *gatewayv1.ApplyEditRequest,
	rel string,
	beforeSHA256 string,
	attempt *executionrecorder.Attempt,
) (*gatewayv1.ApplyEditResponse, error) {
	effectStarted := time.Now()
	raw, err := s.proxyExecute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_ApplyEdit{ApplyEdit: &codev0.ApplyEditRequest{
		File: rel, Find: req.GetFind(), Replace: req.GetReplace(), FixMode: req.GetFixMode(), DryRun: false,
	}}})
	if err != nil {
		finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
			Stage: executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN,
			Resources: []*executionv1.ExecutionResourceV1{
				pathExecutionResource(rel, beforeSHA256, "", false),
			},
			Result: &executionv1.ExecutionResultV1{
				Status: "uncertain", ErrorCode: errorCode("gateway-rpc-outcome-unknown"),
				DurationMs: durationMilliseconds(effectStarted),
			},
		})
		return &gatewayv1.ApplyEditResponse{Success: false, Error: err.Error()}, nil
	}
	rawResult := raw.GetApplyEdit()
	if rawResult == nil {
		finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
			Stage: executionv1.ExecutionStage_EXECUTION_STAGE_FAILED,
			Resources: []*executionv1.ExecutionResourceV1{
				pathExecutionResource(rel, beforeSHA256, "", false),
			},
			Result: &executionv1.ExecutionResultV1{
				Status: "failed", ErrorCode: errorCode("invalid-plugin-response"),
				DurationMs: durationMilliseconds(effectStarted),
			},
		})
		return &gatewayv1.ApplyEditResponse{Success: false, Error: codeFailureMessage(raw)}, nil
	}
	response := &gatewayv1.ApplyEditResponse{
		Success: rawResult.GetSuccess(), Content: rawResult.GetContent(), Error: codeFailureMessage(raw),
		Strategy: rawResult.GetStrategy(), FixActions: rawResult.GetFixActions(), Changed: rawResult.GetChanged(),
		BeforeSha256: rawResult.GetBeforeSha256(), AfterSha256: rawResult.GetAfterSha256(),
		Wrote: rawResult.GetWrote(), Output: rawResult.GetOutput(),
		BeforeSizeBytes: rawResult.GetBeforeSizeBytes(), AfterSizeBytes: rawResult.GetAfterSizeBytes(),
	}
	stage := executionv1.ExecutionStage_EXECUTION_STAGE_FAILED
	statusValue := "failed"
	errorCodeValue := (*string)(nil)
	if response.GetSuccess() {
		stage = executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED
		statusValue = "succeeded"
	} else if response.GetError() != "" {
		errorCodeValue = errorCode("apply-edit-failed")
	}
	resultBefore := response.GetBeforeSha256()
	if !canonicalSHA256(resultBefore) {
		resultBefore = beforeSHA256
	}
	finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
		Stage: stage,
		Resources: []*executionv1.ExecutionResourceV1{
			pathExecutionResource(rel, resultBefore, response.GetAfterSha256(), response.GetChanged()),
		},
		Result: &executionv1.ExecutionResultV1{
			Status: statusValue, ErrorCode: errorCodeValue, DurationMs: durationMilliseconds(effectStarted),
		},
	})
	return response, nil
}

// ApplySymbolPatch forwards one exact analyzer-qualified declaration mutation
// to the owning Codefly agent. The Gateway response is source-free: complete
// post-edit bytes remain inside Codefly even for dry-run preparation.
func (s *Server) ApplySymbolPatch(ctx context.Context, req *gatewayv1.ApplySymbolPatchRequest) (*gatewayv1.ApplySymbolPatchResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetFile())
	if err != nil {
		return &gatewayv1.ApplySymbolPatchResponse{Success: false, Error: err.Error()}, nil
	}
	if req.GetDryRun() {
		if contextErr := validateOptionalExecutionContext(ctx); contextErr != nil {
			return nil, contextErr
		}
	} else {
		operationInputSHA256, digestErr := deterministicProtoSHA256(&codev0.ApplySymbolPatchRequest{
			File: rel, QualifiedName: req.GetQualifiedName(), ExpectedDeclarationSha256: req.GetExpectedDeclarationSha256(),
			NewSource: req.GetNewSource(), FixMode: req.GetFixMode(), DryRun: false,
		})
		if digestErr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "encode apply-symbol-patch input: %v", digestErr)
		}
		var beforeSHA256 string
		if content, readErr := s.fileOps().ReadFile(ctx, rel); readErr == nil {
			digest := sha256.Sum256(content)
			beforeSHA256 = hex.EncodeToString(digest[:])
		}
		attempt, _, beginErr := s.beginGovernedExecution(ctx, executionrecorder.BeginInput{
			OperationKind:        "code.apply-symbol-patch",
			OperationInputSHA256: operationInputSHA256,
			Assurance:            executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_PLUGIN_EXECUTED,
			Target:               executionTarget(s.executionService(req.GetService())),
			Resources: []*executionv1.ExecutionResourceV1{
				pathExecutionResource(rel, beforeSHA256, "", false),
			},
		})
		if beginErr != nil {
			return nil, beginErr
		}
		if attempt != nil {
			return s.applySymbolPatchWithReceipt(ctx, req, rel, beforeSHA256, attempt)
		}
	}
	raw, err := s.executeSymbolPatch(ctx, req, rel)
	if err != nil {
		return &gatewayv1.ApplySymbolPatchResponse{Success: false, Error: err.Error()}, nil
	}
	return gatewaySymbolPatchResponse(raw), nil
}

func (s *Server) executeSymbolPatch(ctx context.Context, req *gatewayv1.ApplySymbolPatchRequest, rel string) (*codev0.CodeResponse, error) {
	execute := s.proxyExecute
	if s.mindYAML == nil {
		execute = s.sourceExecute
	}
	return execute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_ApplySymbolPatch{ApplySymbolPatch: &codev0.ApplySymbolPatchRequest{
		File: rel, QualifiedName: req.GetQualifiedName(), ExpectedDeclarationSha256: req.GetExpectedDeclarationSha256(),
		NewSource: req.GetNewSource(), FixMode: req.GetFixMode(), DryRun: req.GetDryRun(),
	}}})
}

func gatewaySymbolPatchResponse(raw *codev0.CodeResponse) *gatewayv1.ApplySymbolPatchResponse {
	if raw == nil {
		return &gatewayv1.ApplySymbolPatchResponse{Success: false, Error: "Codefly agent returned no symbol-patch response"}
	}
	result := raw.GetApplySymbolPatch()
	if result == nil {
		return &gatewayv1.ApplySymbolPatchResponse{Success: false, Error: codeFailureMessage(raw), Failure: failures.Clone(raw.GetFailure())}
	}
	return &gatewayv1.ApplySymbolPatchResponse{
		Success: result.GetSuccess(), Error: codeFailureMessage(raw), Strategy: result.GetStrategy(),
		FixActions: append([]string(nil), result.GetFixActions()...), Changed: result.GetChanged(),
		BeforeSha256: result.GetBeforeSha256(), AfterSha256: result.GetAfterSha256(),
		DeclarationSha256: result.GetDeclarationSha256(), Wrote: result.GetWrote(), Output: result.GetOutput(),
		Failure: failures.Clone(raw.GetFailure()), FailureReason: result.GetFailureReason(),
		BeforeSizeBytes: result.GetBeforeSizeBytes(), AfterSizeBytes: result.GetAfterSizeBytes(),
	}
}

func (s *Server) applySymbolPatchWithReceipt(ctx context.Context, req *gatewayv1.ApplySymbolPatchRequest, rel, beforeSHA256 string, attempt *executionrecorder.Attempt) (*gatewayv1.ApplySymbolPatchResponse, error) {
	effectStarted := time.Now()
	raw, err := s.proxyExecute(ctx, &codev0.CodeRequest{Operation: &codev0.CodeRequest_ApplySymbolPatch{ApplySymbolPatch: &codev0.ApplySymbolPatchRequest{
		File: rel, QualifiedName: req.GetQualifiedName(), ExpectedDeclarationSha256: req.GetExpectedDeclarationSha256(),
		NewSource: req.GetNewSource(), FixMode: req.GetFixMode(), DryRun: false,
	}}})
	if err != nil {
		finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
			Stage: executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN,
			Resources: []*executionv1.ExecutionResourceV1{
				pathExecutionResource(rel, beforeSHA256, "", false),
			},
			Result: &executionv1.ExecutionResultV1{Status: "uncertain", ErrorCode: errorCode("gateway-rpc-outcome-unknown"), DurationMs: durationMilliseconds(effectStarted)},
		})
		return &gatewayv1.ApplySymbolPatchResponse{Success: false, Error: err.Error()}, nil
	}
	response := gatewaySymbolPatchResponse(raw)
	stage := executionv1.ExecutionStage_EXECUTION_STAGE_FAILED
	statusValue := "failed"
	errorCodeValue := (*string)(nil)
	if response.GetSuccess() {
		stage = executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED
		statusValue = "succeeded"
	} else if response.GetError() != "" {
		errorCodeValue = errorCode("apply-symbol-patch-failed")
	}
	resultBefore := response.GetBeforeSha256()
	if !canonicalSHA256(resultBefore) {
		resultBefore = beforeSHA256
	}
	finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
		Stage: stage,
		Resources: []*executionv1.ExecutionResourceV1{
			pathExecutionResource(rel, resultBefore, response.GetAfterSha256(), response.GetChanged()),
		},
		Result: &executionv1.ExecutionResultV1{Status: statusValue, ErrorCode: errorCodeValue, DurationMs: durationMilliseconds(effectStarted)},
	})
	return response, nil
}

func (s *Server) BatchApplyEdits(ctx context.Context, req *gatewayv1.BatchApplyEditsRequest) (*gatewayv1.BatchApplyEditsResponse, error) {
	type stagedEdit struct {
		path     string
		original []byte
		response *gatewayv1.ApplyEditResponse
	}
	staged := make([]stagedEdit, 0, len(req.GetEdits()))
	seen := make(map[string]struct{}, len(req.GetEdits()))
	results := make([]*gatewayv1.EditResult, 0, len(req.GetEdits()))
	var stageFailed bool

	for _, edit := range req.GetEdits() {
		item := &gatewayv1.EditResult{Service: edit.GetService(), File: edit.GetFile()}
		if err := s.validateService(edit.GetService()); err != nil {
			item.Error = err.Error()
			stageFailed = true
			results = append(results, item)
			continue
		}
		rel, err := cleanGatewayPath(edit.GetFile())
		if err != nil {
			item.Error = err.Error()
			stageFailed = true
			results = append(results, item)
			continue
		}
		key := edit.GetService() + "\x00" + rel
		if _, duplicate := seen[key]; duplicate {
			item.Error = "batch contains multiple edits for the same file"
			stageFailed = true
			results = append(results, item)
			continue
		}
		seen[key] = struct{}{}
		original, err := s.fileOps().ReadFile(ctx, rel)
		if err != nil {
			item.Error = err.Error()
			stageFailed = true
			results = append(results, item)
			continue
		}
		preview, err := s.ApplyEdit(ctx, &gatewayv1.ApplyEditRequest{
			Service: edit.GetService(), File: rel, Find: edit.GetFind(), Replace: edit.GetReplace(),
			FixMode: edit.GetFixMode(), DryRun: true,
		})
		if err != nil || !preview.GetSuccess() {
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Error = preview.GetError()
			}
			stageFailed = true
			results = append(results, item)
			continue
		}
		item.Strategy = preview.GetStrategy()
		staged = append(staged, stagedEdit{path: rel, original: original, response: preview})
		results = append(results, item)
	}
	if stageFailed {
		for _, result := range results {
			if result.GetError() == "" {
				result.Error = "batch aborted because another edit failed validation"
			}
		}
		return &gatewayv1.BatchApplyEditsResponse{Results: results, Failed: int32(len(results))}, nil
	}

	written := make([]stagedEdit, 0, len(staged))
	for _, edit := range staged {
		if !edit.response.GetChanged() {
			continue
		}
		if err := s.fileOps().WriteFile(ctx, edit.path, []byte(edit.response.GetContent())); err != nil {
			// A failed write can still leave a truncated or partially-written file.
			// Restore it as well as every file committed earlier in the batch.
			_ = s.fileOps().WriteFile(ctx, edit.path, edit.original)
			for i := len(written) - 1; i >= 0; i-- {
				_ = s.fileOps().WriteFile(ctx, written[i].path, written[i].original)
			}
			for _, result := range results {
				result.Success = false
				result.Error = fmt.Sprintf("batch commit failed and was rolled back: %v", err)
			}
			return &gatewayv1.BatchApplyEditsResponse{Results: results, Failed: int32(len(results))}, nil
		}
		written = append(written, edit)
	}
	for _, result := range results {
		result.Success = true
	}
	return &gatewayv1.BatchApplyEditsResponse{Results: results, Succeeded: int32(len(results))}, nil
}

func (s *Server) Search(ctx context.Context, req *gatewayv1.SearchRequest) (*gatewayv1.SearchResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	rel, err := cleanGatewayPath(req.GetPath())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result, err := s.fileOps().Search(ctx, codecore.SearchOpts{
		Pattern:         req.GetPattern(),
		Literal:         req.GetLiteral(),
		CaseInsensitive: req.GetCaseInsensitive(),
		Path:            rel,
		Extensions:      req.GetExtensions(),
		Exclude:         req.GetExclude(),
		MaxResults:      int(req.GetMaxResults()),
		ContextLines:    int(req.GetContextLines()),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search: %v", err)
	}
	matches := make([]*gatewayv1.SearchMatch, 0, len(result.Matches))
	for _, m := range result.Matches {
		matches = append(matches, &gatewayv1.SearchMatch{
			File: filepath.ToSlash(m.File),
			Line: int32(m.Line),
			Text: m.Text,
		})
	}
	return &gatewayv1.SearchResponse{
		Matches:      matches,
		Truncated:    result.Truncated,
		TotalMatches: int32(len(result.Matches)),
	}, nil
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
	return &gatewayv1.ListDependenciesResponse{Dependencies: deps, Error: codeFailureMessage(resp)}, nil
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
	return &gatewayv1.AddDependencyResponse{Success: r.GetSuccess(), InstalledVersion: r.GetInstalledVersion(), Error: codeFailureMessage(resp)}, nil
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
	return &gatewayv1.RemoveDependencyResponse{Success: r.GetSuccess(), Error: codeFailureMessage(resp)}, nil
}

// ─── Project Analysis ────────────────────────────────────────

func (s *Server) GetProjectInfo(ctx context.Context, req *gatewayv1.GetProjectInfoRequest) (*gatewayv1.GetProjectInfoResponse, error) {
	requestedService := ""
	if req != nil {
		requestedService = req.GetService()
	}
	if err := s.validateService(requestedService); err != nil {
		return nil, err
	}
	execute := s.proxyExecute
	var inspected *normalizedCodeUnitTarget
	if req != nil && req.GetCodeUnit() != nil {
		targets, err := s.normalizeCodeUnitTargets([]*gatewayv1.CodeUnitTarget{req.GetCodeUnit()})
		if err != nil {
			return &gatewayv1.GetProjectInfoResponse{Failure: failures.New(basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT, "gateway.get-project-info", err.Error())}, nil
		}
		target := targets[0]
		inspected = &target
		service, err := s.serviceBehaviorForCodeUnit(target, "")
		if err != nil {
			return &gatewayv1.GetProjectInfoResponse{
				CodeUnit: cloneCodeUnitTarget(req.GetCodeUnit()),
				Failure:  gatewayProjectInfoFailure(err),
			}, nil
		}
		execute = service.ExecuteCode
	}
	resp, err := execute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetProjectInfo{GetProjectInfo: &codev0.GetProjectInfoRequest{}},
	})
	if err != nil {
		response := &gatewayv1.GetProjectInfoResponse{Failure: gatewayProjectInfoFailure(err)}
		if inspected != nil {
			response.CodeUnit = &gatewayv1.CodeUnitTarget{Id: inspected.id, Path: inspected.path}
		}
		return response, nil
	}
	pi := resp.GetGetProjectInfo()
	if pi == nil {
		response := &gatewayv1.GetProjectInfoResponse{
			Failure: failures.Ensure(resp.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "gateway.get-project-info", "agent returned no project-info result"),
		}
		if inspected != nil {
			response.CodeUnit = &gatewayv1.CodeUnitTarget{Id: inspected.id, Path: inspected.path}
		}
		return response, nil
	}
	var pkgs []*gatewayv1.PackageInfo
	for _, p := range pi.GetPackages() {
		relativePath := p.GetRelativePath()
		if inspected != nil {
			relativePath = rebaseCodeUnitFile(inspected.path, relativePath)
		}
		pkgs = append(pkgs, &gatewayv1.PackageInfo{
			Name: p.Name, RelativePath: relativePath, Files: append([]string(nil), p.Files...), Imports: append([]string(nil), p.Imports...), Doc: p.Doc,
		})
	}
	var deps []*gatewayv1.Dependency
	for _, d := range pi.GetDependencies() {
		deps = append(deps, &gatewayv1.Dependency{Name: d.Name, Version: d.Version, Direct: d.Direct})
	}
	sourceFiles := make([]*gatewayv1.SourceFileInfo, 0, len(pi.GetSourceFiles()))
	for _, file := range pi.GetSourceFiles() {
		filePath := file.GetPath()
		if inspected != nil {
			filePath = rebaseCodeUnitFile(inspected.path, filePath)
		}
		sourceFiles = append(sourceFiles, &gatewayv1.SourceFileInfo{Path: filePath, Imports: append([]string(nil), file.GetImports()...)})
	}
	fileHashes := make(map[string]string, len(pi.GetFileHashes()))
	for file, digest := range pi.GetFileHashes() {
		if inspected != nil {
			file = rebaseCodeUnitFile(inspected.path, file)
		}
		fileHashes[file] = digest
	}
	response := &gatewayv1.GetProjectInfoResponse{
		Module: pi.GetModule(), Language: pi.GetLanguage(), LanguageVersion: pi.GetLanguageVersion(),
		Packages: pkgs, Dependencies: deps, FileHashes: fileHashes, SourceFiles: sourceFiles,
		Failure: failures.Clone(resp.GetFailure()),
	}
	if inspected != nil {
		response.CodeUnit = &gatewayv1.CodeUnitTarget{Id: inspected.id, Path: inspected.path}
	}
	return response, nil
}

// GetSemanticIndex binds one exact code-unit root and invokes the selected
// production agent's Tooling service. The Gateway only rebases locations; it
// never chooses a parser or interprets project syntax.
func (s *Server) GetSemanticIndex(ctx context.Context, req *gatewayv1.GetSemanticIndexRequest) (*gatewayv1.GetSemanticIndexResponse, error) {
	requestedService := ""
	if req != nil {
		requestedService = req.GetService()
	}
	if err := s.validateService(requestedService); err != nil {
		return nil, err
	}
	if req == nil || req.GetCodeUnit() == nil {
		return &gatewayv1.GetSemanticIndexResponse{Failure: failures.New(basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT, "gateway.get-semantic-index", "code_unit is required")}, nil
	}
	targets, err := s.normalizeCodeUnitTargets([]*gatewayv1.CodeUnitTarget{req.GetCodeUnit()})
	if err != nil {
		return &gatewayv1.GetSemanticIndexResponse{CodeUnit: cloneCodeUnitTarget(req.GetCodeUnit()), Failure: failures.New(basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT, "gateway.get-semantic-index", err.Error())}, nil
	}
	target := targets[0]
	inspected := &gatewayv1.CodeUnitTarget{Id: target.id, Path: target.path}
	service, err := s.serviceBehaviorForCodeUnit(target, "")
	if err != nil {
		return &gatewayv1.GetSemanticIndexResponse{CodeUnit: inspected, Failure: gatewaySemanticIndexFailure(err)}, nil
	}
	response, err := service.GetSemanticIndex(ctx, &toolingv0.GetSemanticIndexRequest{})
	if err != nil {
		return &gatewayv1.GetSemanticIndexResponse{CodeUnit: inspected, Failure: gatewaySemanticIndexFailure(err)}, nil
	}
	if response.GetIndex() == nil {
		return &gatewayv1.GetSemanticIndexResponse{
			CodeUnit: inspected,
			Failure:  failures.Ensure(response.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "gateway.get-semantic-index", "agent returned no semantic index"),
		}, nil
	}
	return &gatewayv1.GetSemanticIndexResponse{
		Index:   rebaseCodeUnitSemanticIndex(target.path, response.GetIndex()),
		Failure: failures.Clone(response.GetFailure()), CodeUnit: inspected,
	}, nil
}

// GetSourceManifest invokes the language-neutral rooted Code behavior so every
// workspace has the same body-free artifact inventory regardless of which
// language agents are installed. The Gateway forwards typed identities only;
// it never reads or reconstructs project bytes itself.
func (s *Server) GetSourceManifest(ctx context.Context, req *gatewayv1.GetSourceManifestRequest) (*gatewayv1.GetSourceManifestResponse, error) {
	requestedService, revision := "", ""
	identityMode := basev0.SourceManifestIdentityMode_SOURCE_MANIFEST_IDENTITY_MODE_NATIVE
	if req != nil {
		requestedService = req.GetService()
		revision = req.GetRevision()
		identityMode = req.GetIdentityMode()
	}
	if err := s.validateService(requestedService); err != nil {
		return nil, err
	}
	response, err := s.sourceExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_GetSourceManifest{GetSourceManifest: &codev0.GetSourceManifestRequest{Revision: revision, IdentityMode: identityMode}},
	})
	if err != nil {
		return &gatewayv1.GetSourceManifestResponse{Failure: gatewaySourceManifestFailure(err)}, nil
	}
	value := response.GetGetSourceManifest()
	if value == nil {
		return &gatewayv1.GetSourceManifestResponse{
			Failure: failures.Ensure(response.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "gateway.get-source-manifest", "source behavior returned no manifest"),
		}, nil
	}
	return &gatewayv1.GetSourceManifestResponse{Manifest: value, Failure: failures.Clone(response.GetFailure())}, nil
}

func cloneCodeUnitTarget(target *gatewayv1.CodeUnitTarget) *gatewayv1.CodeUnitTarget {
	if target == nil {
		return nil
	}
	return &gatewayv1.CodeUnitTarget{Id: target.GetId(), Path: target.GetPath()}
}

func gatewayProjectInfoFailure(err error) *basev0.Failure {
	if failure, ok := failures.Extract(err); ok {
		return failures.Clone(failure)
	}
	return failures.FromError("gateway.get-project-info", err)
}

func gatewaySemanticIndexFailure(err error) *basev0.Failure {
	if failure, ok := failures.Extract(err); ok {
		return failures.Clone(failure)
	}
	return failures.FromError("gateway.get-semantic-index", err)
}

func gatewaySourceManifestFailure(err error) *basev0.Failure {
	if failure, ok := failures.Extract(err); ok {
		return failures.Clone(failure)
	}
	return failures.FromError("gateway.get-source-manifest", err)
}

// DiscoverCodeUnits serves Codefly's language-neutral structural inventory
// directly from the rooted source behavior. Discovery must not first select or
// launch a language plugin: the inventory is what downstream consumers use to
// choose a runtime for each independently routable boundary.
func (s *Server) DiscoverCodeUnits(ctx context.Context, req *gatewayv1.DiscoverCodeUnitsRequest) (*gatewayv1.DiscoverCodeUnitsResponse, error) {
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	response, err := s.sourceExecute(ctx, &codev0.CodeRequest{
		Operation: &codev0.CodeRequest_DiscoverCodeUnits{
			DiscoverCodeUnits: &codev0.DiscoverCodeUnitsRequest{},
		},
	})
	if err != nil {
		return &gatewayv1.DiscoverCodeUnitsResponse{Error: gatewayErrorMessage(s.serviceRoot(), err)}, nil
	}
	discovered := response.GetDiscoverCodeUnits()
	if discovered == nil {
		return &gatewayv1.DiscoverCodeUnitsResponse{Error: codeFailureMessage(response)}, nil
	}
	units := make([]*gatewayv1.CodeUnitInfo, 0, len(discovered.GetCodeUnits()))
	for _, unit := range discovered.GetCodeUnits() {
		units = append(units, &gatewayv1.CodeUnitInfo{
			Path: unit.GetPath(), Name: unit.GetName(), PrimaryLanguage: unit.GetPrimaryLanguage(),
			Languages:     append([]string(nil), unit.GetLanguages()...),
			ManifestPaths: append([]string(nil), unit.GetManifestPaths()...),
			RuntimeAgent:  unit.GetRuntimeAgent(),
		})
	}
	return &gatewayv1.DiscoverCodeUnitsResponse{CodeUnits: units, Error: codeFailureMessage(response)}, nil
}

// ═══════════════════════════════════════════════════════════════
// Gateway-direct RPCs — handled locally, not proxied to plugins.
// ═══════════════════════════════════════════════════════════════

// ─── Build / Lint / Test ─────────────────────────────────────

// composeStatusOutput surfaces a typed runtime status message alongside raw
// agent output. Agents that already fold the actionable message into raw output
// (as Generic does) would otherwise show it twice, so the message is only
// prepended when the raw output does not already carry it as its own line(s).
func composeStatusOutput(message, output string) string {
	message = strings.TrimSpace(message)
	if message == "" || outputCarriesMessage(output, message) {
		return output
	}
	if output == "" {
		return message
	}
	return message + "\n" + output
}

// outputCarriesMessage reports whether message already appears in output as a
// contiguous run of whole lines. Matching whole lines rather than a raw substring
// keeps a message that is merely a fragment of a larger, distinct line — where
// the status context is genuinely additive — from being dropped. Only trailing
// whitespace is normalized: leading indentation is significant, so an indented
// output line is treated as distinct from an unindented status message.
func outputCarriesMessage(output, message string) bool {
	messageLines := strings.Split(message, "\n")
	outputLines := strings.Split(output, "\n")
	for start := 0; start+len(messageLines) <= len(outputLines); start++ {
		matched := true
		for offset, messageLine := range messageLines {
			if trimTrailingSpace(outputLines[start+offset]) != trimTrailingSpace(messageLine) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func trimTrailingSpace(s string) string {
	return strings.TrimRight(s, " \t\r\v\f")
}

func (s *Server) Build(ctx context.Context, _ *gatewayv1.BuildRequest) (*gatewayv1.BuildResponse, error) {
	service, err := s.executionServiceBehavior()
	if err != nil {
		return &gatewayv1.BuildResponse{Success: false, Output: fmt.Sprintf("plugin unavailable: %v", err)}, nil
	}
	resp, err := service.Build(ctx, &runtimev0.BuildRequest{})
	if err != nil {
		return &gatewayv1.BuildResponse{Success: false, Output: fmt.Sprintf("plugin build RPC failed: %v", err)}, nil
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.BuildStatus_SUCCESS
	output := resp.Output
	if !success && resp.Status != nil {
		output = composeStatusOutput(resp.Status.Message, output)
	}
	var buildErrors []*gatewayv1.BuildError
	if !success {
		buildErrors = parseBuildErrors(output)
	}
	return &gatewayv1.BuildResponse{Success: success, Errors: buildErrors, Output: output}, nil
}

func (s *Server) Lint(ctx context.Context, _ *gatewayv1.LintRequest) (*gatewayv1.LintResponse, error) {
	service, err := s.executionServiceBehavior()
	if err != nil {
		return &gatewayv1.LintResponse{Success: false, Output: fmt.Sprintf("plugin unavailable: %v", err)}, nil
	}
	resp, err := service.Lint(ctx, &runtimev0.LintRequest{})
	if err != nil {
		return &gatewayv1.LintResponse{Success: false, Output: fmt.Sprintf("plugin lint RPC failed: %v", err)}, nil
	}
	success := resp.Status != nil && resp.Status.State == runtimev0.LintStatus_SUCCESS
	output := resp.Output
	if !success && resp.Status != nil {
		output = composeStatusOutput(resp.Status.Message, output)
	}
	var lintErrors []*gatewayv1.BuildError
	if !success {
		lintErrors = parseBuildErrors(output)
	}
	return &gatewayv1.LintResponse{Success: success, Errors: lintErrors, Output: output}, nil
}

func (s *Server) Test(ctx context.Context, req *gatewayv1.TestRequest) (*gatewayv1.TestResponse, error) {
	requestedService := ""
	if req != nil {
		requestedService = req.GetService()
	}
	if err := s.validateService(requestedService); err != nil {
		return nil, err
	}
	runtimeReq := &runtimev0.TestRequest{}
	if req != nil && req.GetRuntimeRequest() != nil {
		runtimeReq = req.GetRuntimeRequest()
	}
	codeUnits, err := s.normalizeCodeUnitTargets(req.GetCodeUnits())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid code-unit targets: %v", err)
	}
	var service serviceExecution
	if len(codeUnits) == 0 {
		agentOverride := ""
		if s.mindYAML == nil {
			if formula := runtimeReq.GetFormula(); formula != nil {
				agentOverride = engine.DetectFormulaAgent(formula.GetCommand())
			}
		}
		if agentOverride != "" {
			service, err = s.executionServiceBehaviorWithAgent(agentOverride)
		} else {
			service, err = s.executionServiceBehavior()
		}
		if err != nil {
			return &gatewayv1.TestResponse{Success: false, Output: fmt.Sprintf("plugin unavailable: %v", err)}, nil
		}
	}
	selectionContract := proto.Message(runtimeReq)
	if len(codeUnits) > 0 {
		// The target set is part of the governed operation. Hashing only the
		// runtime selector would allow a retry to substitute different unit
		// boundaries under the same execution authority.
		selectionContract = req
	}
	selectionSHA256, err := deterministicProtoSHA256(selectionContract)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "encode test selection: %v", err)
	}
	selectionReference := "sha256:" + selectionSHA256
	attempt, _, err := s.beginGovernedExecution(ctx, executionrecorder.BeginInput{
		OperationKind:        "test.run",
		OperationInputSHA256: selectionSHA256,
		Assurance:            executionv1.ExecutionAssurance_EXECUTION_ASSURANCE_PLUGIN_EXECUTED,
		Target:               executionTarget(s.executionService(requestedService)),
		Resources: []*executionv1.ExecutionResourceV1{{
			Kind: "test.selection", Reference: selectionReference,
		}},
	})
	if err != nil {
		return nil, err
	}
	// ARCHITECTURE: The gateway routes typed code-unit boundaries; language
	// plugins still own native execution. Structured selections and their
	// acknowledgement identity reach the selected plugin unchanged apart from
	// a repository-relative path becoming relative to its owning unit root.
	effectStarted := time.Now()
	var resp *runtimev0.TestResponse
	if len(codeUnits) > 0 {
		resp, err = s.testCodeUnits(ctx, runtimeReq, codeUnits)
	} else {
		resp, err = service.Test(ctx, runtimeReq)
	}
	if err != nil {
		finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
			Stage: executionv1.ExecutionStage_EXECUTION_STAGE_UNCERTAIN,
			Resources: []*executionv1.ExecutionResourceV1{{
				Kind: "test.selection", Reference: selectionReference,
			}},
			Result: &executionv1.ExecutionResultV1{
				Status: "uncertain", ErrorCode: errorCode("plugin-rpc-outcome-unknown"),
				DurationMs: durationMilliseconds(effectStarted),
			},
		})
		return &gatewayv1.TestResponse{Success: false, Output: fmt.Sprintf("plugin test RPC failed: %v", err)}, nil
	}
	success := runtimeTestSuccess(resp)
	output := runtimeTestOutput(resp, success)
	run, passed, failed, skipped := runtimeTestCounts(resp)
	response := &gatewayv1.TestResponse{
		Success:         success,
		Output:          output,
		TestsRun:        run,
		TestsPassed:     passed,
		TestsFailed:     failed,
		TestsSkipped:    skipped,
		CoveragePct:     runtimeTestCoverage(resp),
		Failures:        runtimeTestFailures(resp),
		RuntimeResponse: resp,
	}
	stage := executionv1.ExecutionStage_EXECUTION_STAGE_FAILED
	statusValue := "failed"
	errorCodeValue := errorCode("test-failed")
	if success {
		stage = executionv1.ExecutionStage_EXECUTION_STAGE_SUCCEEDED
		statusValue = "passed"
		errorCodeValue = nil
	}
	finishGovernedExecution(ctx, attempt, executionrecorder.FinishInput{
		Stage: stage,
		Resources: []*executionv1.ExecutionResourceV1{{
			Kind: "test.selection", Reference: selectionReference,
		}},
		Result: &executionv1.ExecutionResultV1{
			Status: statusValue, ErrorCode: errorCodeValue, DurationMs: durationMilliseconds(effectStarted),
			PassedCount: boundedCount(passed), FailedCount: boundedCount(failed), SkippedCount: boundedCount(skipped),
		},
	})
	return response, nil
}

// ConfigureService forwards typed configuration to the owning plugin's
// Builder.Configure RPC. The gateway neither interprets dotted paths nor
// writes project files; the plugin validates and persists its own schema.
func (s *Server) ConfigureService(ctx context.Context, req *gatewayv1.ConfigureServiceRequest) (*gatewayv1.ConfigureServiceResponse, error) {
	requestedService := ""
	if req != nil {
		requestedService = req.GetService()
	}
	if err := s.validateService(requestedService); err != nil {
		return nil, err
	}
	service, err := s.executionServiceBehavior()
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "plugin unavailable: %v", err)
	}
	configurator, ok := service.(serviceConfigurator)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "service plugin does not expose configuration")
	}
	configureReq := &builderv0.ConfigureRequest{}
	if req != nil {
		configureReq.Changes = req.GetChanges()
	}
	response, err := configurator.Configure(ctx, configureReq)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "plugin configure RPC failed: %v", err)
	}
	return &gatewayv1.ConfigureServiceResponse{Response: response}, nil
}

func (s *Server) Format(ctx context.Context, req *gatewayv1.FormatRequest) (*gatewayv1.FormatResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "format request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	paths := append([]string(nil), req.GetPaths()...)
	if len(paths) == 0 {
		gitStatus, err := s.controlScope().GitStatus(ctx)
		if err != nil {
			return &gatewayv1.FormatResponse{Success: false, Output: fmt.Sprintf("discover changed files: %v", err)}, nil
		}
		paths = append(paths, formattableChangedPaths(gitStatus)...)
	}
	changed := make([]string, 0, len(paths))
	var diagnostics []*gatewayv1.BuildError
	var output strings.Builder
	for _, requested := range paths {
		path, err := cleanGatewayPath(requested)
		if err != nil || path == "" {
			if err == nil {
				err = fmt.Errorf("format path cannot be empty")
			}
			diagnostics = append(diagnostics, &gatewayv1.BuildError{File: requested, Message: err.Error(), Severity: "error"})
			continue
		}
		result, err := s.Fix(ctx, &gatewayv1.FixRequest{
			Service: req.GetService(),
			Path:    path,
			Mode:    basev0.FixMode_FIX_MODE_SAFE,
		})
		if err != nil {
			diagnostics = append(diagnostics, &gatewayv1.BuildError{File: path, Message: err.Error(), Severity: "error"})
			continue
		}
		if !result.GetSuccess() {
			diagnostics = append(diagnostics, &gatewayv1.BuildError{File: path, Message: result.GetError(), Severity: "error"})
			continue
		}
		if result.GetChanged() {
			changed = append(changed, path)
		}
		if text := strings.TrimSpace(result.GetOutput()); text != "" {
			fmt.Fprintf(&output, "%s: %s\n", path, text)
		}
	}
	sort.Strings(changed)
	state := "failed"
	if len(diagnostics) == 0 {
		state = "formatted"
		if len(changed) == 0 {
			state = "clean"
		}
	}
	return &gatewayv1.FormatResponse{
		Success:      len(diagnostics) == 0,
		ChangedFiles: changed,
		Errors:       diagnostics,
		Output:       strings.TrimSpace(output.String()),
		Act:          actReceipt("code.format.applied", req.GetService(), state, s.headRevision(ctx), "codefly-plugin"),
	}, nil
}

// formattableChangedPaths selects the working-tree paths a formatter can act on
// from a git status: deletions have nothing on disk to format, and renames are
// reported as "old -> new", so the destination is the file that now exists.
func formattableChangedPaths(st control.GitStatus) []string {
	paths := make([]string, 0, len(st.Files))
	for _, f := range st.Files {
		if strings.ContainsRune(f.Code, 'D') {
			continue
		}
		path := f.Path
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+len(" -> "):]
		}
		paths = append(paths, path)
	}
	return paths
}

// headRevision resolves the current commit for act attribution. It is empty when
// the repository has no commits yet — a format act mutates the working tree and
// is not itself a commit, so this is best-effort context, never fatal.
func (s *Server) headRevision(ctx context.Context) string {
	commits, err := s.controlScope().GitLog(ctx, control.GitLogRequest{Limit: 1})
	if err != nil || len(commits) == 0 {
		return ""
	}
	return commits[0].SHA
}

func validateOptionalExecutionContext(ctx context.Context) error {
	_, _, err := codefly.GRPCExecutionContextFromIncomingIfPresent(ctx)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "invalid Codefly execution context: %v", err)
	}
	return nil
}

func (s *Server) beginGovernedExecution(
	ctx context.Context,
	input executionrecorder.BeginInput,
) (*executionrecorder.Attempt, bool, error) {
	execution, present, err := codefly.GRPCExecutionContextFromIncomingIfPresent(ctx)
	if err != nil {
		return nil, false, status.Errorf(codes.InvalidArgument, "invalid Codefly execution context: %v", err)
	}
	if !present {
		return nil, false, nil
	}
	if s.executionRecorder == nil {
		return nil, true, status.Error(
			codes.FailedPrecondition,
			"Codefly execution authority was supplied but governed execution is not configured",
		)
	}
	result, err := s.executionRecorder.Begin(ctx, execution, input)
	if err != nil {
		if errors.Is(err, executionrecorder.ErrConflict) {
			return nil, true, status.Errorf(
				codes.AlreadyExists,
				"governed operation identity conflict: %v",
				err,
			)
		}
		return nil, true, status.Errorf(codes.PermissionDenied, "governed execution admission failed: %v", err)
	}
	if result.Existing != nil {
		receipt := result.Existing.Attestation.GetReceipt()
		return nil, true, status.Errorf(
			codes.AlreadyExists,
			"operation %q already has durable stage %s; effect was not re-executed",
			receipt.GetOperationId(),
			receipt.GetStage(),
		)
	}
	if result.Attempt == nil {
		return nil, true, status.Error(codes.Internal, "governed execution admission returned no attempt")
	}
	return result.Attempt, true, nil
}

func finishGovernedExecution(
	requestContext context.Context,
	attempt *executionrecorder.Attempt,
	input executionrecorder.FinishInput,
) {
	if attempt == nil {
		return
	}
	// The effect may have completed after the client cancelled. Persist the
	// terminal fact independently, with a short bound, so cancellation cannot
	// erase execution evidence.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), 2*time.Second)
	defer cancel()
	if _, err := attempt.Finish(ctx, input); err != nil {
		wool.Get(requestContext).In("gateway.execution").Error(
			"effect completed but terminal execution receipt is pending reconciliation",
			wool.ErrField(err),
		)
	}
}

func executionTarget(service string) *executionv1.ExecutionTargetV1 {
	return &executionv1.ExecutionTargetV1{Service: service}
}

func (s *Server) executionService(requested string) string {
	if requested != "" {
		return requested
	}
	if s.mindYAML == nil {
		return ""
	}
	return s.mindYAML.Service
}

func deterministicProtoSHA256(message proto.Message) (string, error) {
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func pathExecutionResource(path, beforeSHA256, afterSHA256 string, changed bool) *executionv1.ExecutionResourceV1 {
	resource := &executionv1.ExecutionResourceV1{
		Kind:      "workspace.path",
		Reference: filepath.ToSlash(path),
		Changed:   changed,
	}
	if canonicalSHA256(beforeSHA256) {
		value := beforeSHA256
		resource.BeforeSha256 = &value
	}
	if canonicalSHA256(afterSHA256) {
		value := afterSHA256
		resource.AfterSha256 = &value
	}
	return resource
}

func canonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func boundedCount(value int32) uint64 {
	if value <= 0 {
		return 0
	}
	return uint64(value)
}

func durationMilliseconds(started time.Time) uint64 {
	elapsed := time.Since(started)
	if elapsed <= 0 {
		return 0
	}
	return uint64(elapsed / time.Millisecond)
}

func errorCode(value string) *string {
	return &value
}

func runtimeTestSuccess(resp *runtimev0.TestResponse) bool {
	if resp == nil {
		return false
	}
	if result := resp.GetResult(); result != nil {
		switch result.GetState() {
		case runtimev0.TestRunResult_PASSED:
			return true
		case runtimev0.TestRunResult_FAILED, runtimev0.TestRunResult_ERRORED, runtimev0.TestRunResult_TIMED_OUT:
			return false
		}
	}
	if status := resp.GetStatus(); status != nil {
		return status.GetState() == runtimev0.TestStatus_SUCCESS
	}
	return resp.GetTestsFailed() == 0 && len(resp.GetFailures()) == 0
}

func runtimeTestOutput(resp *runtimev0.TestResponse, success bool) string {
	if resp == nil {
		return ""
	}
	output := resp.GetOutput()
	if success {
		return output
	}
	var msg string
	if result := resp.GetResult(); result != nil {
		msg = result.GetMessage()
	}
	if msg == "" {
		if status := resp.GetStatus(); status != nil {
			msg = status.GetMessage()
		}
	}
	if msg == "" {
		return output
	}
	if output == "" {
		return msg
	}
	return msg + "\n" + output
}

func runtimeTestCounts(resp *runtimev0.TestResponse) (run, passed, failed, skipped int32) {
	if resp == nil {
		return 0, 0, 0, 0
	}
	if counts := resp.GetCounts(); counts != nil {
		return counts.GetTotal(), counts.GetPassed(), counts.GetFailed() + counts.GetErrored(), counts.GetSkipped()
	}
	return resp.GetTestsRun(), resp.GetTestsPassed(), resp.GetTestsFailed(), resp.GetTestsSkipped()
}

func runtimeTestCoverage(resp *runtimev0.TestResponse) float32 {
	if resp == nil {
		return 0
	}
	if coverage := resp.GetCoverage(); coverage != nil {
		return coverage.GetTotalPct()
	}
	return resp.GetCoveragePct()
}

func runtimeTestFailures(resp *runtimev0.TestResponse) []string {
	if resp == nil {
		return nil
	}
	if len(resp.GetFailures()) > 0 {
		return append([]string(nil), resp.GetFailures()...)
	}
	failures := runtimeTestSuiteFailures(nil, resp.GetSuites())
	if len(failures) == 0 {
		if result := resp.GetResult(); result != nil && result.GetMessage() != "" && !runtimeTestSuccess(resp) {
			failures = append(failures, result.GetMessage())
		}
	}
	return failures
}

func runtimeTestSuiteFailures(out []string, suites []*runtimev0.TestSuite) []string {
	for _, suite := range suites {
		for _, tc := range suite.GetCases() {
			failure := tc.GetFailure()
			if failure == nil {
				continue
			}
			name := tc.GetFullName()
			if name == "" {
				name = tc.GetName()
			}
			msg := failure.GetMessage()
			if msg == "" {
				msg = failure.GetDetail()
			}
			if name != "" && msg != "" {
				out = append(out, name+": "+msg)
			} else if name != "" {
				out = append(out, name)
			} else if msg != "" {
				out = append(out, msg)
			}
		}
		out = runtimeTestSuiteFailures(out, suite.GetSuites())
	}
	return out
}

// ─── Execution ───────────────────────────────────────────────

func (s *Server) RunCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "run command request is required")
	}
	if err := validateUnstructuredUse(req.GetUnstructuredUse()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetCommand()) == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}
	const maxTimeoutSeconds = int32(600)
	if req.TimeoutSeconds > maxTimeoutSeconds {
		return nil, status.Errorf(codes.InvalidArgument, "timeout_seconds must be at most %d", maxTimeoutSeconds)
	}
	timeout := 30 * time.Second
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dir, err := s.resolveCommandDir(req.GetWorkingDir())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = dir

	// Optional single-shot stdin. CONTRACT: the full payload is written
	// to the child's standard input and the stream closes at EOF; stdout
	// and stderr are read to completion afterwards. Not a bidirectional
	// pipe — enough for batch protocols with a fixed upfront request
	// list, e.g. feeding `git cat-file --batch` a list of object names.
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	stdout := newBoundedCommandBuffer()
	stderr := newBoundedCommandBuffer()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &gatewayv1.RunCommandResponse{ExitCode: -1, Stdout: stdout.Output(), Stderr: appendCommandError(stderr.Output(), err)}, nil
		}
	}
	return &gatewayv1.RunCommandResponse{
		ExitCode: int32(exitCode),
		Stdout:   stdout.Output(),
		Stderr:   stderr.Output(),
	}, nil
}

func validateUnstructuredUse(use *gatewayv1.UnstructuredUse) error {
	if use == nil {
		return fmt.Errorf("unstructured_use attribution is required")
	}
	for name, value := range map[string]string{
		"intent": use.GetIntent(), "why_no_tool": use.GetWhyNoTool(),
		"code_unit_id": use.GetCodeUnitId(), "objective_id": use.GetObjectiveId(),
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("unstructured_use.%s is required", name)
		}
	}
	if use.GetCommandClass() == gatewayv1.CommandClass_COMMAND_CLASS_UNSPECIFIED {
		return fmt.Errorf("unstructured_use.command_class is required")
	}
	return nil
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

	// Ask the configured service behavior. Agent startup remains lazy and is
	// owned by engine.WorkspaceHost rather than this transport adapter.
	service, err := s.executionServiceBehavior()
	if err == nil {
		resp, listErr := service.ListCommands(ctx, &agentv0.ListCommandsRequest{})
		if listErr == nil {
			for _, cmd := range resp.GetCommands() {
				commands = append(commands, &gatewayv1.AvailableCommand{
					Name:        cmd.GetName(),
					Description: cmd.GetDescription(),
					Aliases:     cmd.GetAliases(),
					Tags:        cmd.GetTags(),
					Plugin:      s.mindYAML.Service,
				})
			}
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
	st, err := s.controlScope().GitStatus(ctx)
	if err != nil {
		return &gatewayv1.GitStatusResponse{Error: err.Error()}, nil
	}
	var files []*gatewayv1.GitFileStatus
	for _, f := range st.Files {
		files = append(files, &gatewayv1.GitFileStatus{
			Path: f.Path, Status: gitStatusString(f.Code), Staged: f.Staged,
		})
	}
	return &gatewayv1.GitStatusResponse{
		Files: files, Branch: st.Branch, RepositoryRoot: st.RepositoryRoot,
	}, nil
}

func (s *Server) GitDiff(ctx context.Context, req *gatewayv1.GitDiffRequest) (*gatewayv1.GitDiffResponse, error) {
	if !req.GetStaged() {
		path, err := cleanGatewayPath(req.GetPath())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		response, err := s.sourceExecute(ctx, &codev0.CodeRequest{
			Operation: &codev0.CodeRequest_GitDiff{GitDiff: &codev0.GitDiffRequest{Path: path}},
		})
		if err != nil {
			return &gatewayv1.GitDiffResponse{Error: gatewayErrorMessage(s.serviceRoot(), err)}, nil
		}
		result := response.GetGitDiff()
		if result == nil {
			return &gatewayv1.GitDiffResponse{Error: codeFailureMessage(response)}, nil
		}
		return &gatewayv1.GitDiffResponse{Diff: result.GetDiff(), Error: codeFailureMessage(response)}, nil
	}
	dr := control.GitDiffRequest{Staged: req.Staged}
	if req.Path != "" {
		path, err := cleanGatewayPath(req.Path)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		dr.Paths = []string{path}
	}
	diff, err := s.controlScope().GitDiff(ctx, dr)
	if err != nil {
		return &gatewayv1.GitDiffResponse{Error: err.Error()}, nil
	}
	return &gatewayv1.GitDiffResponse{Diff: diff}, nil
}

func gatewayErrorMessage(root string, err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	cleanRoot := filepath.Clean(root)
	if cleanRoot != "" && cleanRoot != "." {
		message = strings.ReplaceAll(message, cleanRoot+string(filepath.Separator), "")
		message = strings.ReplaceAll(message, cleanRoot, ".")
	}
	return message
}

func (s *Server) GitLog(ctx context.Context, req *gatewayv1.GitLogRequest) (*gatewayv1.GitLogResponse, error) {
	count := int(req.Count)
	if count <= 0 {
		count = 10
	}
	if count > 1000 {
		return nil, status.Error(codes.InvalidArgument, "git log count must be at most 1000")
	}
	commits, err := s.controlScope().GitLog(ctx, control.GitLogRequest{Limit: count})
	if err != nil {
		return &gatewayv1.GitLogResponse{Error: err.Error()}, nil
	}
	var out []*gatewayv1.GitCommitInfo
	for _, c := range commits {
		out = append(out, &gatewayv1.GitCommitInfo{
			Hash: c.SHA, ShortHash: c.ShortHash, Author: c.Author,
			Message: c.Message, Date: c.Date,
		})
	}
	return &gatewayv1.GitLogResponse{Commits: out}, nil
}

func (s *Server) GitCommit(ctx context.Context, req *gatewayv1.GitCommitRequest) (*gatewayv1.GitCommitResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git commit request is required")
	}
	if req.GetAll() && len(req.GetPaths()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "git commit all and paths are mutually exclusive")
	}
	paths := make([]string, 0, len(req.Paths))
	for _, requested := range req.Paths {
		path, err := cleanGatewayPath(requested)
		if err != nil || path == "" {
			if err == nil {
				err = fmt.Errorf("git path cannot be empty")
			}
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		paths = append(paths, path)
	}
	commit, err := s.controlScope().GitCommit(ctx, control.GitCommitRequest{Message: req.Message, Paths: paths, All: req.GetAll()})
	if err != nil {
		return &gatewayv1.GitCommitResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.GitCommitResponse{Success: true, Hash: commit.SHA}, nil
}

func (s *Server) GitBranch(ctx context.Context, req *gatewayv1.GitBranchRequest) (*gatewayv1.GitBranchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git branch request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.controlScope().GitBranch(ctx, control.GitBranchRequest{
		Name: req.GetName(), StartPoint: req.GetStartPoint(), Force: req.GetForce(),
	})
	if err != nil {
		return &gatewayv1.GitBranchResponse{Success: false, Error: err.Error()}, nil
	}
	kind := "git.branch.created"
	if req.GetForce() {
		kind = "git.branch.updated"
	}
	return &gatewayv1.GitBranchResponse{
		Success:  true,
		Branch:   result.Target,
		Revision: result.Revision,
		Act:      gitActReceipt(kind, result.Target, result.Revision),
	}, nil
}

func (s *Server) GitCheckout(ctx context.Context, req *gatewayv1.GitCheckoutRequest) (*gatewayv1.GitCheckoutResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git checkout request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.controlScope().GitCheckout(ctx, control.GitCheckoutRequest{Ref: req.GetRef(), Detach: req.GetDetach()})
	if err != nil {
		return &gatewayv1.GitCheckoutResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.GitCheckoutResponse{
		Success:  true,
		Branch:   result.Branch,
		Revision: result.Revision,
		Act:      gitActReceipt("git.checkout.applied", result.Target, result.Revision),
	}, nil
}

func (s *Server) GitPush(ctx context.Context, req *gatewayv1.GitPushRequest) (*gatewayv1.GitPushResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git push request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	mode := control.GitPushMode("")
	switch req.GetMode() {
	case gatewayv1.GitPushMode_GIT_PUSH_MODE_FAST_FORWARD_ONLY:
		mode = control.GitPushFastForwardOnly
	case gatewayv1.GitPushMode_GIT_PUSH_MODE_FORCE_WITH_LEASE:
		mode = control.GitPushForceWithLease
	default:
		return nil, status.Error(codes.InvalidArgument, "git push mode is required")
	}
	result, err := s.controlScope().GitPush(ctx, control.GitPushRequest{
		Remote: req.GetRemote(), Branch: req.GetBranch(), SetUpstream: req.GetSetUpstream(), Mode: mode,
	})
	if err != nil {
		return &gatewayv1.GitPushResponse{Success: false, Error: err.Error()}, nil
	}
	target := result.Remote + "/" + result.Branch
	return &gatewayv1.GitPushResponse{
		Success: true, Remote: result.Remote, Branch: result.Branch, Revision: result.Revision,
		Act: gitActReceipt("git.branch.pushed", target, result.Revision),
	}, nil
}

func (s *Server) GitTag(ctx context.Context, req *gatewayv1.GitTagRequest) (*gatewayv1.GitTagResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git tag request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.controlScope().GitTag(ctx, control.GitTagRequest{
		Name: req.GetName(), Revision: req.GetRevision(), Message: req.GetMessage(), Sign: req.GetSign(),
	})
	if err != nil {
		return &gatewayv1.GitTagResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.GitTagResponse{
		Success: true, Tag: result.Target, Revision: result.Revision,
		Act: gitActReceipt("git.tag.created", result.Target, result.Revision),
	}, nil
}

func (s *Server) GitMerge(ctx context.Context, req *gatewayv1.GitMergeRequest) (*gatewayv1.GitMergeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git merge request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.controlScope().GitMerge(ctx, control.GitMergeRequest{
		Ref: req.GetRef(), Message: req.GetMessage(), NoFastForward: req.GetNoFastForward(),
	})
	if err != nil {
		return &gatewayv1.GitMergeResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.GitMergeResponse{
		Success: true, Branch: result.Branch, Revision: result.Revision,
		Act: gitActReceipt("git.merge.applied", result.Branch+"<-"+result.Target, result.Revision),
	}, nil
}

func (s *Server) GitRevert(ctx context.Context, req *gatewayv1.GitRevertRequest) (*gatewayv1.GitRevertResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "git revert request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.controlScope().GitRevert(ctx, control.GitRevertRequest{Revision: req.GetRevision()})
	if err != nil {
		return &gatewayv1.GitRevertResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.GitRevertResponse{
		Success: true, RevertedRevision: result.Target, Revision: result.Revision,
		Act: gitActReceipt("git.revert.applied", result.Target, result.Revision),
	}, nil
}

// MaterializeRepositorySnapshot delegates the complete clone/fetch/worktree
// transaction to Codefly's typed VCS control plane. The access enum is the
// authority boundary that prevents public sources from inheriting host Git
// rewrites or credentials.
func (s *Server) MaterializeRepositorySnapshot(
	ctx context.Context,
	req *gatewayv1.MaterializeRepositorySnapshotRequest,
) (*gatewayv1.MaterializeRepositorySnapshotResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "repository snapshot materialization request is required")
	}
	cacheDirectory, err := cleanGatewayPath(req.GetCacheDirectory())
	if err != nil || cacheDirectory == "" {
		if err == nil {
			err = fmt.Errorf("repository cache directory is required")
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	snapshotDirectory, err := cleanGatewayPath(req.GetSnapshotDirectory())
	if err != nil || snapshotDirectory == "" {
		if err == nil {
			err = fmt.Errorf("repository snapshot directory is required")
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	access := control.RepositoryRemoteAccess("")
	switch req.GetRemoteAccess() {
	case gatewayv1.RepositoryRemoteAccess_REPOSITORY_REMOTE_ACCESS_PUBLIC_HTTPS:
		access = control.RepositoryRemoteAccessPublicHTTPS
	case gatewayv1.RepositoryRemoteAccess_REPOSITORY_REMOTE_ACCESS_CONFIGURED:
		access = control.RepositoryRemoteAccessConfigured
	case gatewayv1.RepositoryRemoteAccess_REPOSITORY_REMOTE_ACCESS_LOCAL_FILE:
		access = control.RepositoryRemoteAccessLocalFile
	default:
		return nil, status.Error(codes.InvalidArgument, "repository remote access is required")
	}
	result, err := s.controlScope().MaterializeRepositorySnapshot(ctx, control.MaterializeRepositorySnapshotRequest{
		RepositoryURL: req.GetRepositoryUrl(), CacheDirectory: cacheDirectory,
		Revision: req.GetRevision(), FetchIdentity: req.GetFetchIdentity(),
		SnapshotDirectory: snapshotDirectory, RemoteAccess: access,
	})
	if err != nil {
		return &gatewayv1.MaterializeRepositorySnapshotResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.MaterializeRepositorySnapshotResponse{
		Success: true, Revision: result.Revision, SnapshotDirectory: result.SnapshotDirectory,
	}, nil
}

func (s *Server) PrepareRepositoryCheckout(
	ctx context.Context,
	req *gatewayv1.PrepareRepositoryCheckoutRequest,
) (*gatewayv1.PrepareRepositoryCheckoutResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "repository checkout preparation request is required")
	}
	cacheDirectory, err := cleanGatewayPath(req.GetCacheDirectory())
	if err != nil || cacheDirectory == "" {
		if err == nil {
			err = fmt.Errorf("repository cache directory is required")
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	access := control.RepositoryRemoteAccess("")
	switch req.GetRemoteAccess() {
	case gatewayv1.RepositoryRemoteAccess_REPOSITORY_REMOTE_ACCESS_PUBLIC_HTTPS:
		access = control.RepositoryRemoteAccessPublicHTTPS
	case gatewayv1.RepositoryRemoteAccess_REPOSITORY_REMOTE_ACCESS_CONFIGURED:
		access = control.RepositoryRemoteAccessConfigured
	case gatewayv1.RepositoryRemoteAccess_REPOSITORY_REMOTE_ACCESS_LOCAL_FILE:
		access = control.RepositoryRemoteAccessLocalFile
	default:
		return nil, status.Error(codes.InvalidArgument, "repository remote access is required")
	}
	result, err := s.controlScope().PrepareRepositoryCheckout(ctx, control.PrepareRepositoryCheckoutRequest{
		RepositoryURL: req.GetRepositoryUrl(), CacheDirectory: cacheDirectory,
		Revision: req.GetRevision(), FetchIdentity: req.GetFetchIdentity(), RemoteAccess: access,
	})
	if err != nil {
		return &gatewayv1.PrepareRepositoryCheckoutResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.PrepareRepositoryCheckoutResponse{
		Success: true, Revision: result.Revision, DefaultBranch: result.DefaultBranch,
	}, nil
}

func (s *Server) ReleaseRepositorySnapshot(
	ctx context.Context,
	req *gatewayv1.ReleaseRepositorySnapshotRequest,
) (*gatewayv1.ReleaseRepositorySnapshotResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "repository snapshot release request is required")
	}
	cacheDirectory, err := cleanGatewayPath(req.GetCacheDirectory())
	if err != nil || cacheDirectory == "" {
		if err == nil {
			err = fmt.Errorf("repository cache directory is required")
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	snapshotDirectory, err := cleanGatewayPath(req.GetSnapshotDirectory())
	if err != nil || snapshotDirectory == "" {
		if err == nil {
			err = fmt.Errorf("repository snapshot directory is required")
		}
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.controlScope().ReleaseRepositorySnapshot(ctx, control.ReleaseRepositorySnapshotRequest{
		CacheDirectory: cacheDirectory, SnapshotDirectory: snapshotDirectory,
	}); err != nil {
		return &gatewayv1.ReleaseRepositorySnapshotResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.ReleaseRepositorySnapshotResponse{Success: true}, nil
}

func (s *Server) Release(ctx context.Context, req *gatewayv1.ReleaseRequest) (*gatewayv1.ReleaseResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "release request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	bump := ""
	switch req.GetBump() {
	case gatewayv1.ReleaseBump_RELEASE_BUMP_PATCH:
		bump = "patch"
	case gatewayv1.ReleaseBump_RELEASE_BUMP_MINOR:
		bump = "minor"
	case gatewayv1.ReleaseBump_RELEASE_BUMP_MAJOR:
		bump = "major"
	default:
		return nil, status.Error(codes.InvalidArgument, "release bump is required")
	}
	units := make([]publishcmd.CodeUnitRelease, 0, len(req.GetUnits()))
	for _, unit := range req.GetUnits() {
		if unit == nil {
			return nil, status.Error(codes.InvalidArgument, "release units cannot be nil")
		}
		units = append(units, publishcmd.CodeUnitRelease{
			CodeUnitID: unit.GetCodeUnitId(), VersionFile: unit.GetVersionFile(),
		})
	}
	result, err := publishcmd.ReleaseCodeUnits(ctx, s.serviceRoot(), bump, units)
	if err != nil {
		return &gatewayv1.ReleaseResponse{Success: false, Error: err.Error()}, nil
	}
	released := make([]*gatewayv1.ReleasedUnit, 0, len(result.Units))
	for _, unit := range result.Units {
		released = append(released, &gatewayv1.ReleasedUnit{
			CodeUnitId: unit.CodeUnitID, VersionFile: unit.VersionFile, Version: result.Version,
		})
	}
	return &gatewayv1.ReleaseResponse{
		Success: true, Tag: result.Tag, Revision: result.Revision, Units: released,
		Act: actReceipt("release.published", result.Tag, "applied", result.Revision, "git"),
	}, nil
}

func (s *Server) ForgePullRequestStatus(ctx context.Context, req *gatewayv1.ForgePullRequestStatusRequest) (*gatewayv1.ForgePullRequestStatusResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "forge status request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.forgeToolbox().PullRequestStatus(ctx, req.GetRepository(), req.GetNumber())
	if err != nil {
		return &gatewayv1.ForgePullRequestStatusResponse{Error: err.Error()}, nil
	}
	return &gatewayv1.ForgePullRequestStatusResponse{Status: result}, nil
}

func (s *Server) ForgeMergePullRequest(ctx context.Context, req *gatewayv1.ForgeMergePullRequestRequest) (*gatewayv1.ForgeMergePullRequestResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "forge merge request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.forgeToolbox().MergePullRequest(ctx, req)
	if err != nil {
		return &gatewayv1.ForgeMergePullRequestResponse{Success: false, Error: err.Error()}, nil
	}
	return result, nil
}

func (s *Server) ForgeRequestReview(ctx context.Context, req *gatewayv1.ForgeRequestReviewRequest) (*gatewayv1.ForgeRequestReviewResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "forge review request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	result, err := s.forgeToolbox().RequestReview(ctx, req)
	if err != nil {
		return &gatewayv1.ForgeRequestReviewResponse{Success: false, Error: err.Error()}, nil
	}
	return result, nil
}

func (s *Server) ForgeNormalizeWebhook(_ context.Context, req *gatewayv1.ForgeNormalizeWebhookRequest) (*gatewayv1.ForgeNormalizeWebhookResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "forge webhook request is required")
	}
	if err := s.validateService(req.GetService()); err != nil {
		return nil, err
	}
	event, err := s.forgeToolbox().NormalizeWebhook(req)
	if err != nil {
		return &gatewayv1.ForgeNormalizeWebhookResponse{Error: err.Error()}, nil
	}
	return &gatewayv1.ForgeNormalizeWebhookResponse{Event: event}, nil
}

func gitActReceipt(kind, target, revision string) *gatewayv1.ActReceipt {
	return actReceipt(kind, target, "applied", revision, "git")
}

func actReceipt(kind, target, state, revision, source string) *gatewayv1.ActReceipt {
	return &gatewayv1.ActReceipt{
		EventId:             fmt.Sprintf("%s:%s:%s", kind, target, revision),
		Kind:                kind,
		Target:              target,
		State:               state,
		Revision:            revision,
		AuthoritativeSource: source,
		ObservedAt:          timestamppb.Now(),
	}
}

func (s *Server) forgeToolbox() *githubtoolbox.Server {
	token := strings.TrimSpace(s.cfg.GitHubToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	return githubtoolbox.New(s.serviceRoot(), token, "gateway")
}

// ═══════════════════════════════════════════════════════════════
// Helpers
// ═══════════════════════════════════════════════════════════════

func (s *Server) runCommandCheck(ctx context.Context, name string, ch *gatewayv1.CommandCheck) *gatewayv1.CheckResult {
	cmd := exec.CommandContext(ctx, "sh", "-c", ch.Run)
	cmd.Dir = s.serviceRoot()

	stdout := newBoundedCommandBuffer()
	stderr := newBoundedCommandBuffer()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	combined := stdout.Output() + stderr.Output()
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

const maxGatewayCommandOutput = 4 << 20

type boundedCommandBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func newBoundedCommandBuffer() boundedCommandBuffer {
	var b boundedCommandBuffer
	b.buf.Grow(64 << 10)
	return b
}

func (b *boundedCommandBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := maxGatewayCommandOutput - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	if original > remaining {
		b.truncated = true
	}
	// Report the full write and keep draining the pipe; returning a short write
	// would stop os/exec's copier and can deadlock or SIGPIPE the child.
	return original, nil
}

func (b *boundedCommandBuffer) Output() string {
	out := b.buf.String()
	if b.truncated {
		out += "\n[codefly gateway: output truncated]\n"
	}
	return out
}

func appendCommandError(output string, err error) string {
	if output == "" {
		return err.Error()
	}
	return output + "\n" + err.Error()
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

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authenticateGatewayRequest(ctx context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "gateway authentication required")
	}
	want := "Bearer " + token
	for _, value := range md.Get("authorization") {
		if subtle.ConstantTimeCompare([]byte(value), []byte(want)) == 1 {
			return nil
		}
	}
	for _, value := range md.Get("x-codefly-gateway-token") {
		if subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1 {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid gateway authentication token")
}

func gatewayAuthUnaryInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authenticateGatewayRequest(ctx, token); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func gatewayAuthStreamInterceptor(token string) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authenticateGatewayRequest(stream.Context(), token); err != nil {
			return err
		}
		return handler(srv, stream)
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
