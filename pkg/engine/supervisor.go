package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/grpc/connectivity"
)

// agentStartupTimeout bounds loading a released agent process through the same
// path used in production. A cold load may include platform binary resolution
// and competes with other agent processes during heterogeneous workspace and
// race-suite startup, so the handshake needs a larger budget than an already
// running agent's runtime initialization.
const agentStartupTimeout = 2 * time.Minute

// AgentSupervisorConfig configures an AgentSupervisor.
type AgentSupervisorConfig struct {
	Root      string
	LogWriter io.Writer
}

// AgentSupervisor owns lazy service-agent startup, runtime initialization,
// health eviction, retry support, and shutdown.
type AgentSupervisor struct {
	cfg AgentSupervisorConfig

	mu       sync.Mutex
	sessions map[string]*AgentSession
	keyLocks sync.Map
	closed   bool

	stop chan struct{}
	wg   sync.WaitGroup
}

// AgentSession is a ready service agent and its typed leaf capabilities.
type AgentSession struct {
	key        string
	descriptor *serviceDescriptor
	agent      *resources.Agent
	connection *manager.AgentConn
	code       codev0.CodeClient
	agentAPI   agentv0.AgentClient
	builder    builderv0.BuilderClient
	runtime    runtimev0.RuntimeClient
	tooling    toolingv0.ToolingClient
	builderMu  sync.Mutex
	builderOK  bool
	runtimeMu  sync.Mutex
	runtimeOK  bool
	closeOnce  sync.Once
}

type agentInitializationError struct {
	err error
}

func (e *agentInitializationError) Error() string {
	return e.err.Error()
}

func (e *agentInitializationError) Unwrap() error {
	return e.err
}

// NewAgentSupervisor creates an empty, lazy supervisor.
func NewAgentSupervisor(cfg AgentSupervisorConfig) *AgentSupervisor {
	return &AgentSupervisor{
		cfg:      cfg,
		sessions: make(map[string]*AgentSession),
		stop:     make(chan struct{}),
	}
}

func (s *AgentSupervisor) acquire(ctx context.Context, target ServiceTarget) (*AgentSession, error) {
	descriptor, err := resolveServiceDescriptor(ctx, target)
	if err != nil {
		return nil, err
	}
	keepDescriptor := false
	defer func() {
		if !keepDescriptor && descriptor.cleanup != nil {
			_ = descriptor.cleanup()
		}
	}()
	agentName, err := descriptor.agentName()
	if err != nil {
		return nil, err
	}
	key := target.Root + "\x00" + descriptor.identity.GetModule() + "\x00" + descriptor.identity.GetName() + "\x00" + agentName

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("agent supervisor is closed")
	}
	if session := s.sessions[key]; session != nil {
		s.mu.Unlock()
		return session, nil
	}
	// Count the loader before releasing mu. Close marks the supervisor closed
	// under the same lock before Wait, so no loader can be added after shutdown
	// starts.
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()

	// Serialize only identical service sessions. Different services can start
	// their agents concurrently instead of waiting behind one global mutex.
	value, _ := s.keyLocks.LoadOrStore(key, &sync.Mutex{})
	keyLock := value.(*sync.Mutex)
	keyLock.Lock()
	defer keyLock.Unlock()

	// Another caller may have completed this key while we waited.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("agent supervisor is closed")
	}
	if session := s.sessions[key]; session != nil {
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()

	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, agentName)
	if err != nil {
		return nil, fmt.Errorf("parse agent %q: %w", agentName, err)
	}
	if _, err := manager.ResolveLatest(ctx, agent); err != nil {
		return nil, fmt.Errorf("resolve agent %s version: %w", agentName, err)
	}

	loadCtx, cancel := context.WithTimeout(ctx, agentStartupTimeout)
	defer cancel()
	workDir := target.Root
	if descriptor.cleanup != nil && descriptor.service != nil {
		workDir = descriptor.service.Dir()
	}
	options := []manager.LoadOption{
		manager.WithWorkDir(workDir),
		manager.WithoutSandbox(),
		manager.WithoutPrincipal(),
	}
	if s.cfg.LogWriter != nil {
		options = append(options, manager.WithLogWriter(newPrefixWriter(s.cfg.LogWriter, fmt.Sprintf("[agent:%s] ", agent.Name))))
	}
	connection, err := manager.Load(loadCtx, agent, options...)
	if err != nil {
		return nil, fmt.Errorf("load agent %s: %w", agentName, err)
	}
	session := &AgentSession{
		key:        key,
		descriptor: descriptor,
		agent:      agent,
		connection: connection,
		code:       codev0.NewCodeClient(connection.GRPCConn()),
		agentAPI:   agentv0.NewAgentClient(connection.GRPCConn()),
		builder:    builderv0.NewBuilderClient(connection.GRPCConn()),
		runtime:    runtimev0.NewRuntimeClient(connection.GRPCConn()),
		tooling:    toolingv0.NewToolingClient(connection.GRPCConn()),
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		session.close()
		return nil, fmt.Errorf("agent supervisor is closed")
	}
	s.sessions[key] = session
	keepDescriptor = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.monitor(session)
	return session, nil
}

// initializeBuilder lazily loads the builder half of the already-running
// service agent. Ordinary read/test traffic never pays this lifecycle cost;
// configuration calls use the same plugin process and service identity as the
// runtime, so a persisted formula is immediately visible to the next test.
func initializeBuilder(ctx context.Context, session *AgentSession) error {
	if session == nil || session.builder == nil || session.descriptor == nil {
		return fmt.Errorf("builder client is unavailable")
	}
	session.builderMu.Lock()
	defer session.builderMu.Unlock()
	if session.builderOK {
		return nil
	}
	response, err := session.builder.Load(ctx, &builderv0.LoadRequest{
		Identity: session.descriptor.identity,
	})
	if err != nil {
		return fmt.Errorf("builder Load: %w", err)
	}
	if response == nil || response.GetState().GetState() != builderv0.LoadStatus_READY {
		return fmt.Errorf("builder Load did not become ready: %s", response.GetState().GetMessage())
	}
	session.builderOK = true
	return nil
}

// ensureRuntime lazily runs Runtime Load+Init on the already-running service
// agent. Read-only Code/Tooling traffic never pays this lifecycle cost; the
// first Build/Test/Lint/Stop initializes it and later calls reuse it. Failures
// are returned as *agentInitializationError so Test can surface the typed
// env-blocked response, and are not cached so a later call can retry once the
// environment is ready.
func ensureRuntime(ctx context.Context, session *AgentSession) error {
	if session == nil || session.runtime == nil || session.descriptor == nil || session.agent == nil {
		return fmt.Errorf("runtime client is unavailable")
	}
	session.runtimeMu.Lock()
	defer session.runtimeMu.Unlock()
	if session.runtimeOK {
		return nil
	}
	initCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := initializeRuntime(initCtx, session); err != nil {
		return &agentInitializationError{err: fmt.Errorf("initialize agent %s runtime: %w", session.agent.Name, err)}
	}
	session.runtimeOK = true
	return nil
}

func initializeRuntime(ctx context.Context, session *AgentSession) error {
	descriptor := session.descriptor
	environmentProto, err := descriptor.environment.Proto()
	if err != nil {
		return fmt.Errorf("build local environment: %w", err)
	}
	loadResponse, err := session.runtime.Load(ctx, &runtimev0.LoadRequest{
		Identity:    descriptor.identity,
		Environment: environmentProto,
	})
	if err != nil {
		return fmt.Errorf("runtime Load: %w", err)
	}
	if loadResponse == nil || loadResponse.GetStatus().GetState() != runtimev0.LoadStatus_READY {
		return fmt.Errorf("runtime Load did not become ready: %s", loadResponse.GetStatus().GetMessage())
	}
	if descriptor.workspace == nil {
		initResponse, err := session.runtime.Init(ctx, &runtimev0.InitRequest{
			RuntimeContext: resources.NewRuntimeContextNative(),
		})
		if err != nil {
			return fmt.Errorf("runtime Init: %w", err)
		}
		if initResponse == nil || initResponse.GetStatus().GetState() != runtimev0.InitStatus_READY {
			return fmt.Errorf("runtime Init did not become ready: %s", initResponse.GetStatus().GetMessage())
		}
		return nil
	}
	networkManager, err := network.NewRuntimeManager(ctx, nil)
	if err != nil {
		return fmt.Errorf("create local network manager: %w", err)
	}
	networkManager.WithTemporaryPorts()
	resourceIdentity := &resources.ServiceIdentity{
		Name:                descriptor.identity.GetName(),
		Version:             descriptor.identity.GetVersion(),
		Module:              descriptor.identity.GetModule(),
		Workspace:           descriptor.identity.GetWorkspace(),
		WorkspacePath:       descriptor.identity.GetWorkspacePath(),
		RelativeToWorkspace: descriptor.identity.GetRelativeToWorkspace(),
	}
	mappings, err := networkManager.GenerateNetworkMappings(
		ctx,
		descriptor.environment,
		descriptor.workspace,
		resourceIdentity,
		loadResponse.GetEndpoints(),
		resources.NewRuntimeContextNative(),
	)
	if err != nil {
		return fmt.Errorf("generate local runtime mappings: %w", err)
	}
	initResponse, err := session.runtime.Init(ctx, &runtimev0.InitRequest{
		RuntimeContext:          resources.NewRuntimeContextNative(),
		ProposedNetworkMappings: mappings,
	})
	if err != nil {
		return fmt.Errorf("runtime Init: %w", err)
	}
	if initResponse == nil || initResponse.GetStatus().GetState() != runtimev0.InitStatus_READY {
		return fmt.Errorf("runtime Init did not become ready: %s", initResponse.GetStatus().GetMessage())
	}
	return nil
}

func (s *AgentSupervisor) monitor(session *AgentSession) {
	defer s.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			connection := session.connection.GRPCConn()
			if connection == nil {
				s.evict(session)
				return
			}
			if process := session.connection.ProcessInfo(); process != nil && process.PID > 0 && !processAlive(process.PID) {
				s.evict(session)
				return
			}
			if connection.GetState() == connectivity.Shutdown {
				s.evict(session)
				return
			}
		}
	}
}

func processAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(syscall.Signal(0)) == nil
}

func (s *AgentSupervisor) evict(session *AgentSession) {
	if session == nil {
		return
	}
	s.mu.Lock()
	if s.sessions[session.key] == session {
		delete(s.sessions, session.key)
	}
	s.mu.Unlock()
	session.close()
}

// Close stops health monitors and all owned agent processes. It is idempotent.
func (s *AgentSupervisor) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stop)
	s.mu.Unlock()
	s.wg.Wait()

	s.mu.Lock()
	sessions := make([]*AgentSession, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, session)
	}
	s.sessions = make(map[string]*AgentSession)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, session := range sessions {
		wg.Add(1)
		go func(session *AgentSession) {
			defer wg.Done()
			session.close()
		}(session)
	}
	wg.Wait()
}

func (s *AgentSession) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.connection != nil {
			s.connection.Close()
		}
		if s.descriptor != nil && s.descriptor.cleanup != nil {
			_ = s.descriptor.cleanup()
			s.descriptor.cleanup = nil
		}
	})
}

type prefixWriter struct {
	mu     sync.Mutex
	writer io.Writer
	prefix string
	fresh  bool
}

func newPrefixWriter(writer io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{writer: writer, prefix: prefix, fresh: true}
}

func (w *prefixWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := 0
	for len(data) > 0 {
		if w.fresh {
			if _, err := io.WriteString(w.writer, w.prefix); err != nil {
				return written, err
			}
			w.fresh = false
		}
		index := 0
		for index < len(data) && data[index] != '\n' {
			index++
		}
		if index < len(data) {
			index++
			w.fresh = true
		}
		count, err := w.writer.Write(data[:index])
		written += count
		if err != nil {
			return written, err
		}
		data = data[index:]
	}
	return written, nil
}
