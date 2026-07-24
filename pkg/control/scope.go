package control

import (
	"context"

	codecore "github.com/codefly-dev/core/code"
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

	// Lifecycle / checks / commands (this service; any Service on the request is
	// overridden with the scoped one)
	Build(ctx context.Context, req BuildRequest) (BuildResult, error)
	Test(ctx context.Context, req TestRequest) (CheckResult, error)
	Lint(ctx context.Context, req CheckRequest) (CheckResult, error)
	Compile(ctx context.Context, req CheckRequest) (CheckResult, error)
	RunChecks(ctx context.Context, req CheckRequest) (CheckResult, error)
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
	return &serviceScope{plane: p, name: name, dir: service.Dir()}, nil
}

// ServiceScopeAt returns a scope whose file/git operations root at dir WITHOUT
// resolving a workspace — for a service-scoped adapter (the Gateway) that
// already knows the service directory and may run with no surrounding workspace
// (the codefly-in-Docker model). The lifecycle/command methods still resolve a
// workspace on use and will error when there is none, so an adapter in that mode
// should only call the source/VCS methods.
func ServiceScopeAt(name, dir string) ServiceScope {
	return &serviceScope{plane: New().(*planeImpl), name: name, dir: dir}
}

// serviceScope implements ServiceScope. File/git ops reuse the shared *With /
// *At helpers rooted at the service dir; lifecycle/commands delegate to the
// workspace-scoped plane methods with the bound service name.
type serviceScope struct {
	plane *planeImpl
	name  string
	dir   string
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

// --- Lifecycle / checks / commands (bound service) ---

func (s *serviceScope) Build(ctx context.Context, req BuildRequest) (BuildResult, error) {
	req.Service = s.name
	return s.plane.Build(ctx, req)
}

func (s *serviceScope) Test(ctx context.Context, req TestRequest) (CheckResult, error) {
	req.Service = s.name
	return s.plane.Test(ctx, req)
}

func (s *serviceScope) Lint(ctx context.Context, req CheckRequest) (CheckResult, error) {
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
	return s.plane.ListCommands(ctx, s.name)
}

func (s *serviceScope) RunCommand(ctx context.Context, command string, args []string) (CommandResult, error) {
	return s.plane.RunCommand(ctx, RunCommandRequest{Service: s.name, Command: command, Args: args})
}
