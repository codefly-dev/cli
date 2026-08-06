package control

import (
	"context"

	"github.com/codefly-dev/cli/pkg/engine"
	"github.com/codefly-dev/cli/pkg/orchestration"
	codecore "github.com/codefly-dev/core/code"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
)

// ServiceScope is a view of the plane bound to ONE service. File and git
// operations root at that service's directory (not the workspace), and
// build/check/command operations target that service — so callers never pass a
// service name or a workspace-relative path.
//
// This is the seam service-scoped adapters delegate to. The Gateway, for
// example, is single-service (its cfg.WorkDir is one service's tree, possibly
// with no surrounding workspace in the codefly-in-Docker model): it resolves a
// scope once and forwards its service-relative RPCs straight through.
type ServiceScope interface {
	// Source (rooted at the service dir)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	CreateFile(ctx context.Context, path string, content []byte) error
	DeleteFile(ctx context.Context, path string) error
	MoveFile(ctx context.Context, from, to string) error
	ListFiles(ctx context.Context, dir string) ([]FileInfo, error)
	Search(ctx context.Context, req SearchRequest) ([]SearchHit, error)
	ApplyEdit(ctx context.Context, edit Edit) error
	BatchApplyEdits(ctx context.Context, edits []Edit) error

	// VCS (rooted at the service dir; any Dir on the request is ignored)
	GitStatus(ctx context.Context) (GitStatus, error)
	GitDiff(ctx context.Context, req GitDiffRequest) (string, error)
	GitLog(ctx context.Context, req GitLogRequest) ([]GitCommit, error)
	GitCommit(ctx context.Context, req GitCommitRequest) (GitCommit, error)
	GitBranch(ctx context.Context, req GitBranchRequest) (GitAct, error)
	GitCheckout(ctx context.Context, req GitCheckoutRequest) (GitAct, error)
	GitPush(ctx context.Context, req GitPushRequest) (GitPushResult, error)
	GitTag(ctx context.Context, req GitTagRequest) (GitAct, error)
	GitMerge(ctx context.Context, req GitMergeRequest) (GitAct, error)
	GitRevert(ctx context.Context, req GitRevertRequest) (GitAct, error)
	MaterializeRepositorySnapshot(ctx context.Context, req MaterializeRepositorySnapshotRequest) (MaterializedRepositorySnapshot, error)
	PrepareRepositoryCheckout(ctx context.Context, req PrepareRepositoryCheckoutRequest) (PreparedRepositoryCheckout, error)
	ReleaseRepositorySnapshot(ctx context.Context, req ReleaseRepositorySnapshotRequest) error

	// Lifecycle / checks / commands (this service; any Service on the request is
	// overridden with the scoped one)
	Build(ctx context.Context, req BuildRequest) (BuildResult, error)
	Test(ctx context.Context, req TestRequest) (CheckResult, error)
	Lint(ctx context.Context, req CheckRequest) (CheckResult, error)
	Compile(ctx context.Context, req CheckRequest) (CheckResult, error)
	RunChecks(ctx context.Context, req CheckRequest) (CheckResult, error)
	Stop(ctx context.Context) error
	ListCommands(ctx context.Context) ([]Command, error)
	RunCommand(ctx context.Context, command string, args []string) (CommandResult, error)

	// Name is the resolved "module/service" this scope is bound to.
	Name() string
	// Dir is the absolute service source directory operations root at.
	Dir() string
}

// Service resolves name ("module/service" or a bare service) and returns a
// handle scoped to it.
func (p *planeImpl) Service(ctx context.Context, name string) (ServiceScope, error) {
	_, _, service, err := p.loadTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	var behavior *engine.Service
	if p.host != nil {
		behavior, err = p.host.Service(engine.ServiceTarget{Name: service.Name, Root: service.Dir()})
		if err != nil {
			return nil, err
		}
	}
	return &serviceScope{plane: p, behavior: behavior, name: name, dir: service.Dir()}, nil
}

// ServiceScopeAt returns a scope whose file/git operations root at dir WITHOUT
// resolving a workspace — for a service-scoped adapter (the Gateway) that
// already knows the service directory and may run with no surrounding workspace
// (the codefly-in-Docker model). The lifecycle/command methods still resolve a
// workspace on use and will error when there is none, so an adapter in that mode
// should only call the source/VCS methods.
func ServiceScopeAt(name, dir string) ServiceScope {
	return &serviceScope{plane: newPlaneRooted(dir), name: name, dir: dir}
}

// serviceScope implements ServiceScope. File/git ops reuse the shared *With /
// *At helpers rooted at the service dir; lifecycle/commands delegate to the
// workspace-scoped plane methods with the bound service name.
type serviceScope struct {
	plane    *planeImpl
	behavior *engine.Service
	name     string
	dir      string
}

func (s *serviceScope) Name() string { return s.name }
func (s *serviceScope) Dir() string  { return s.dir }

func (s *serviceScope) fileOps() codecore.FileOperation {
	return codecore.NewFileOps(codecore.LocalVFS{}, s.dir)
}

// --- Source (service-rooted) ---

func (s *serviceScope) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.fileOps().ReadFile(ctx, path)
}

func (s *serviceScope) WriteFile(ctx context.Context, path string, content []byte) error {
	return s.fileOps().WriteFile(ctx, path, content)
}

func (s *serviceScope) CreateFile(ctx context.Context, path string, content []byte) error {
	return createFileWith(ctx, s.fileOps(), path, content)
}

func (s *serviceScope) DeleteFile(ctx context.Context, path string) error {
	return s.fileOps().DeleteFile(ctx, path)
}

func (s *serviceScope) MoveFile(ctx context.Context, from, to string) error {
	return s.fileOps().MoveFile(ctx, from, to)
}

func (s *serviceScope) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	return listFilesWith(ctx, s.fileOps(), s.dir, dir)
}

func (s *serviceScope) Search(ctx context.Context, req SearchRequest) ([]SearchHit, error) {
	return searchWith(ctx, s.fileOps(), req)
}

func (s *serviceScope) ApplyEdit(ctx context.Context, edit Edit) error {
	return applyEditWith(ctx, s.fileOps(), edit)
}

func (s *serviceScope) BatchApplyEdits(ctx context.Context, edits []Edit) error {
	return batchApplyEditsWith(ctx, s.fileOps(), edits)
}

// --- VCS (service-rooted) ---

func (s *serviceScope) GitStatus(ctx context.Context) (GitStatus, error) {
	return gitStatusAt(ctx, s.dir)
}

func (s *serviceScope) GitDiff(ctx context.Context, req GitDiffRequest) (string, error) {
	return gitDiffAt(ctx, s.dir, req)
}

func (s *serviceScope) GitLog(ctx context.Context, req GitLogRequest) ([]GitCommit, error) {
	return gitLogAt(ctx, s.dir, req)
}

func (s *serviceScope) GitCommit(ctx context.Context, req GitCommitRequest) (GitCommit, error) {
	return gitCommitAt(ctx, s.dir, req)
}

func (s *serviceScope) GitBranch(ctx context.Context, req GitBranchRequest) (GitAct, error) {
	return gitBranchAt(ctx, s.dir, req)
}

func (s *serviceScope) GitCheckout(ctx context.Context, req GitCheckoutRequest) (GitAct, error) {
	return gitCheckoutAt(ctx, s.dir, req)
}

func (s *serviceScope) GitPush(ctx context.Context, req GitPushRequest) (GitPushResult, error) {
	return gitPushAt(ctx, s.dir, req)
}

func (s *serviceScope) GitTag(ctx context.Context, req GitTagRequest) (GitAct, error) {
	return gitTagAt(ctx, s.dir, req)
}

func (s *serviceScope) GitMerge(ctx context.Context, req GitMergeRequest) (GitAct, error) {
	return gitMergeAt(ctx, s.dir, req)
}

func (s *serviceScope) GitRevert(ctx context.Context, req GitRevertRequest) (GitAct, error) {
	return gitRevertAt(ctx, s.dir, req)
}

func (s *serviceScope) MaterializeRepositorySnapshot(
	ctx context.Context,
	req MaterializeRepositorySnapshotRequest,
) (MaterializedRepositorySnapshot, error) {
	req.Dir = s.dir
	return s.plane.MaterializeRepositorySnapshot(ctx, req)
}

func (s *serviceScope) PrepareRepositoryCheckout(
	ctx context.Context,
	req PrepareRepositoryCheckoutRequest,
) (PreparedRepositoryCheckout, error) {
	req.Dir = s.dir
	return s.plane.PrepareRepositoryCheckout(ctx, req)
}

func (s *serviceScope) ReleaseRepositorySnapshot(ctx context.Context, req ReleaseRepositorySnapshotRequest) error {
	req.Dir = s.dir
	return s.plane.ReleaseRepositorySnapshot(ctx, req)
}

// --- Lifecycle / checks / commands (bound service) ---

func (s *serviceScope) Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	if s.behavior != nil {
		response, err := s.behavior.Build(ctx, &runtimev0.BuildRequest{})
		if err != nil {
			return BuildResult{}, err
		}
		return BuildResult{
			Succeeded: response.GetStatus().GetState() == runtimev0.BuildStatus_SUCCESS,
			Output:    response.GetOutput(),
		}, nil
	}
	req.Service = s.name
	return s.plane.Build(ctx, req)
}

func (s *serviceScope) Test(ctx context.Context, req TestRequest) (CheckResult, error) {
	if s.behavior != nil && req.RuntimeContext == "" {
		request := &runtimev0.TestRequest{Suite: req.Suite}
		if req.Filter != "" {
			request.Filters = []string{req.Filter}
		}
		response, err := s.behavior.Test(ctx, request)
		if err != nil {
			return CheckResult{}, err
		}
		return CheckResult{
			Passed: orchestration.TestSucceeded(response),
			Output: orchestration.RenderTestReport(response),
		}, nil
	}
	req.Service = s.name
	return s.plane.Test(ctx, req)
}

func (s *serviceScope) Lint(ctx context.Context, req CheckRequest) (CheckResult, error) {
	if s.behavior != nil && req.RuntimeContext == "" {
		response, err := s.behavior.Lint(ctx, &runtimev0.LintRequest{})
		if err != nil {
			return CheckResult{}, err
		}
		result := CheckResult{
			Passed: response.GetStatus().GetState() == runtimev0.LintStatus_SUCCESS,
			Output: response.GetOutput(),
		}
		for _, diagnostic := range response.GetDiagnostics() {
			result.Details = append(result.Details, CheckFinding{
				File: diagnostic.GetFile(), Line: int(diagnostic.GetLine()),
				Severity: diagnostic.GetSeverity().String(), Message: diagnostic.GetMessage(),
			})
		}
		return result, nil
	}
	req.Service = s.name
	return s.plane.Lint(ctx, req)
}

func (s *serviceScope) Compile(ctx context.Context, req CheckRequest) (CheckResult, error) {
	req.Service = s.name
	return s.plane.Compile(ctx, req)
}

func (s *serviceScope) RunChecks(ctx context.Context, req CheckRequest) (CheckResult, error) {
	req.Service = s.name
	return s.plane.RunChecks(ctx, req)
}

func (s *serviceScope) ListCommands(ctx context.Context) ([]Command, error) {
	if s.behavior != nil {
		response, err := s.behavior.ListCommands(ctx, &agentv0.ListCommandsRequest{})
		if err != nil {
			return nil, err
		}
		commands := make([]Command, 0, len(response.GetCommands()))
		for _, command := range response.GetCommands() {
			commands = append(commands, Command{
				Name: command.GetName(), Description: command.GetDescription(),
				Usage: command.GetUsage(), Tags: command.GetTags(), Destructive: command.GetDestructive(),
			})
		}
		return commands, nil
	}
	return s.plane.ListCommands(ctx, s.name)
}

func (s *serviceScope) RunCommand(ctx context.Context, command string, args []string) (CommandResult, error) {
	if s.behavior != nil {
		response, err := s.behavior.RunCommand(ctx, &agentv0.RunPluginCommandRequest{Command: command, Args: args})
		if err != nil {
			return CommandResult{}, err
		}
		if !response.GetSuccess() {
			return CommandResult{ExitCode: 1, Output: response.GetError()}, nil
		}
		return CommandResult{Output: response.GetOutput()}, nil
	}
	return s.plane.RunCommand(ctx, RunCommandRequest{Service: s.name, Command: command, Args: args})
}

func (s *serviceScope) Stop(ctx context.Context) error {
	if s.behavior == nil {
		return s.plane.Stop(ctx, StopRequest{})
	}
	_, err := s.behavior.Stop(ctx, &runtimev0.StopRequest{})
	return err
}
