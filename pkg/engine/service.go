package engine

import (
	"context"
	"errors"
	"fmt"

	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	codev1 "github.com/codefly-dev/core/generated/go/codefly/services/code/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
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
func (s *Service) ExecuteCode(ctx context.Context, request *codev1.CodeRequest) (*codev1.CodeResponse, error) {
	var response *codev1.CodeResponse
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

// Build invokes the service agent's runtime build behavior.
func (s *Service) Build(ctx context.Context, request *runtimev1.BuildRequest) (*runtimev1.BuildResponse, error) {
	var response *runtimev1.BuildResponse
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
func (s *Service) Test(ctx context.Context, request *runtimev1.TestRequest) (*runtimev1.TestResponse, error) {
	var response *runtimev1.TestResponse
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
		run := &runtimev1.TestRun{Runner: "codefly-agent"}
		if request != nil && request.GetSelection() != nil {
			if selection, ok := proto.Clone(request.GetSelection()).(*runtimev1.TestSelection); ok {
				run.RequestedSelection = selection
			}
			run.SelectionId = request.GetSelectionId()
		}
		//nolint:staticcheck // SA1019: the proto keeps the flat fields populated for consumers that have not migrated to the structured tree
		return &runtimev1.TestResponse{
			Status: &runtimev1.TestStatus{
				State: runtimev1.TestStatus_ERROR, Message: message,
			},
			Result: &runtimev1.TestRunResult{
				State: runtimev1.TestRunResult_ERRORED, Message: message,
			},
			Counts: &runtimev1.TestCounts{},
			Run:    run,
		}, nil
	}
	return response, err
}

// Configure invokes the service agent's builder configuration behavior. The
// call is not transport-retried: an unavailable response may have persisted
// the change, so replaying it would make APPEND operations ambiguous.
func (s *Service) Configure(ctx context.Context, request *builderv1.ConfigureRequest) (*builderv1.ConfigureResponse, error) {
	var response *builderv1.ConfigureResponse
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
func (s *Service) Lint(ctx context.Context, request *runtimev1.LintRequest) (*runtimev1.LintResponse, error) {
	var response *runtimev1.LintResponse
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
func (s *Service) ListCommands(ctx context.Context, request *agentv1.ListCommandsRequest) (*agentv1.ListCommandsResponse, error) {
	var response *agentv1.ListCommandsResponse
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
func (s *Service) RunCommand(ctx context.Context, request *agentv1.RunPluginCommandRequest) (*agentv1.RunPluginCommandResponse, error) {
	var response *agentv1.RunPluginCommandResponse
	err := s.withSession(ctx, "agent.RunPluginCommand", false, func(session *AgentSession) error {
		var err error
		response, err = session.agentAPI.RunPluginCommand(ctx, request)
		return err
	})
	return response, err
}

// Stop asks the service runtime to stop. The supervisor continues owning the
// agent process so later operations can reuse it.
func (s *Service) Stop(ctx context.Context, request *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	var response *runtimev1.StopResponse
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
func codeRequestRetrySafe(request *codev1.CodeRequest) bool {
	if request == nil {
		return false
	}
	switch request.GetOperation().(type) {
	case *codev1.CodeRequest_ListDependencies,
		*codev1.CodeRequest_GetProjectInfo,
		*codev1.CodeRequest_GetSourceManifest,
		*codev1.CodeRequest_ReadFile,
		*codev1.CodeRequest_ListFiles,
		*codev1.CodeRequest_Search,
		*codev1.CodeRequest_GitLog,
		*codev1.CodeRequest_GitShow,
		*codev1.CodeRequest_GitBlame,
		*codev1.CodeRequest_GitDiff:
		return true
	default:
		return false
	}
}
