package engine

import (
	"context"
	"errors"
	"fmt"

	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Service is the transport-independent leaf behavior for one service target.
type Service struct {
	target     ServiceTarget
	supervisor *AgentSupervisor
}

func (s *Service) withSession(ctx context.Context, operation string, retryUnavailable bool, call func(*AgentSession) error) error {
	if s == nil || s.supervisor == nil {
		return fmt.Errorf("service executor is closed")
	}
	session, err := s.supervisor.acquire(ctx, s.target)
	if err != nil {
		return err
	}
	err = call(session)
	if !retryUnavailable || !isTransientAgentError(err) {
		return err
	}
	s.supervisor.evict(session)
	session, reconnectErr := s.supervisor.acquire(ctx, s.target)
	if reconnectErr != nil {
		return fmt.Errorf("retry %s: reconnect failed: %w", operation, reconnectErr)
	}
	return call(session)
}

func isTransientAgentError(err error) bool {
	if err == nil {
		return false
	}
	// Runtime-initialization failures carry their own typed handling (Test's
	// env-blocked response) and are never transport-retried: evicting and
	// respawning the agent would only repeat a lifecycle failure rather than
	// recover a dropped connection. The wrapped cause can itself be Unavailable,
	// so this exclusion must precede the code check.
	var initializationError *agentInitializationError
	if errors.As(err, &initializationError) {
		return false
	}
	return status.Code(err) == codes.Unavailable
}

// ExecuteCode executes a typed Code leaf operation.
func (s *Service) ExecuteCode(ctx context.Context, request *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	var response *codev0.CodeResponse
	// Read-only Code inspection runs against the started agent alone. Mutating
	// operations (write, apply-edit, shell) act on the runtime-managed workspace
	// and need the runtime lifecycle like Build/Test/Lint/Stop, so they
	// initialize it on first use.
	readOnly := codeRequestRetrySafe(request)
	err := s.withSession(ctx, "code.Execute", readOnly, func(session *AgentSession) error {
		if !readOnly {
			if err := ensureRuntime(ctx, session); err != nil {
				return err
			}
		}
		var err error
		response, err = session.code.Execute(ctx, request)
		return err
	})
	return response, err
}

// GetSemanticIndex invokes the production agent's Tooling boundary. Project
// bytes and parser selection remain inside the agent; callers receive only the
// shared body-free semantic contract.
func (s *Service) GetSemanticIndex(ctx context.Context, request *toolingv0.GetSemanticIndexRequest) (*toolingv0.GetSemanticIndexResponse, error) {
	var response *toolingv0.GetSemanticIndexResponse
	err := s.withSession(ctx, "tooling.GetSemanticIndex", true, func(session *AgentSession) error {
		var err error
		response, err = session.tooling.GetSemanticIndex(ctx, request)
		return err
	})
	return response, err
}

// Build invokes the service agent's runtime build behavior.
func (s *Service) Build(ctx context.Context, request *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	var response *runtimev0.BuildResponse
	err := s.withSession(ctx, "runtime.Build", true, func(session *AgentSession) error {
		if err := ensureRuntime(ctx, session); err != nil {
			return err
		}
		var err error
		response, err = session.runtime.Build(ctx, request)
		return err
	})
	return response, err
}

// Test invokes the service agent's runtime test behavior.
func (s *Service) Test(ctx context.Context, request *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	var response *runtimev0.TestResponse
	err := s.withSession(ctx, "runtime.Test", true, func(session *AgentSession) error {
		if err := ensureRuntime(ctx, session); err != nil {
			return err
		}
		var err error
		response, err = session.runtime.Test(ctx, request)
		return err
	})
	var initializationError *agentInitializationError
	if errors.As(err, &initializationError) {
		message := "env-blocked (runtime-initialization): " + initializationError.Error()
		run := &runtimev0.TestRun{Runner: "codefly-agent"}
		if request != nil && request.GetSelection() != nil {
			run.RequestedSelection = proto.Clone(request.GetSelection()).(*runtimev0.TestSelection)
			run.SelectionId = request.GetSelectionId()
		}
		return &runtimev0.TestResponse{
			Status: &runtimev0.TestStatus{
				State: runtimev0.TestStatus_ERROR, Message: message,
			},
			Result: &runtimev0.TestRunResult{
				State: runtimev0.TestRunResult_ERRORED, Message: message,
			},
			Counts: &runtimev0.TestCounts{},
			Run:    run,
		}, nil
	}
	return response, err
}

// Configure invokes the service agent's builder configuration behavior. The
// call is not transport-retried: an unavailable response may have persisted
// the change, so replaying it would make APPEND operations ambiguous.
func (s *Service) Configure(ctx context.Context, request *builderv0.ConfigureRequest) (*builderv0.ConfigureResponse, error) {
	var response *builderv0.ConfigureResponse
	err := s.withSession(ctx, "builder.Configure", false, func(session *AgentSession) error {
		if err := initializeBuilder(ctx, session); err != nil {
			return err
		}
		var err error
		response, err = session.builder.Configure(ctx, request)
		return err
	})
	return response, err
}

// Lint invokes the service agent's runtime lint behavior.
func (s *Service) Lint(ctx context.Context, request *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	var response *runtimev0.LintResponse
	err := s.withSession(ctx, "runtime.Lint", true, func(session *AgentSession) error {
		if err := ensureRuntime(ctx, session); err != nil {
			return err
		}
		var err error
		response, err = session.runtime.Lint(ctx, request)
		return err
	})
	return response, err
}

// ListCommands returns commands advertised by the service agent. Command
// discovery is agent-level and intentionally skips the runtime lifecycle: the
// registry is populated at agent startup, not by Runtime.Load, and callers such
// as the gateway's ListAllCommands rely on it staying lazy.
func (s *Service) ListCommands(ctx context.Context, request *agentv0.ListCommandsRequest) (*agentv0.ListCommandsResponse, error) {
	var response *agentv0.ListCommandsResponse
	err := s.withSession(ctx, "agent.ListCommands", true, func(session *AgentSession) error {
		var err error
		response, err = session.agentAPI.ListCommands(ctx, request)
		return err
	})
	return response, err
}

// RunCommand invokes a command advertised by the service agent. Plugin commands
// execute against the service directory, not the runtime environment, so they
// intentionally skip the runtime lifecycle rather than coupling command
// execution to toolchain readiness.
func (s *Service) RunCommand(ctx context.Context, request *agentv0.RunPluginCommandRequest) (*agentv0.RunPluginCommandResponse, error) {
	var response *agentv0.RunPluginCommandResponse
	err := s.withSession(ctx, "agent.RunPluginCommand", false, func(session *AgentSession) error {
		var err error
		response, err = session.agentAPI.RunPluginCommand(ctx, request)
		return err
	})
	return response, err
}

// Stop asks the service runtime to stop. The supervisor continues owning the
// agent process so later operations can reuse it.
func (s *Service) Stop(ctx context.Context, request *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	var response *runtimev0.StopResponse
	err := s.withSession(ctx, "runtime.Stop", true, func(session *AgentSession) error {
		if err := ensureRuntime(ctx, session); err != nil {
			return err
		}
		var err error
		response, err = session.runtime.Stop(ctx, request)
		return err
	})
	return response, err
}

// codeRequestRetrySafe identifies read-only Code operations. An Unavailable
// response is ambiguous: a mutating request may already have taken effect, so
// those requests are evicted but never replayed automatically.
func codeRequestRetrySafe(request *codev0.CodeRequest) bool {
	if request == nil {
		return false
	}
	switch request.GetOperation().(type) {
	case *codev0.CodeRequest_ListDependencies,
		*codev0.CodeRequest_GetProjectInfo,
		*codev0.CodeRequest_GetSourceManifest,
		*codev0.CodeRequest_ReadFile,
		*codev0.CodeRequest_ListFiles,
		*codev0.CodeRequest_Search,
		*codev0.CodeRequest_GitLog,
		*codev0.CodeRequest_GitShow,
		*codev0.CodeRequest_GitBlame,
		*codev0.CodeRequest_GitDiff:
		return true
	default:
		return false
	}
}
