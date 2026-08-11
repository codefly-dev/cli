package control

import (
	"context"

	"github.com/codefly-dev/cli/pkg/gitops"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
)

// Plane is the complete control surface for a codefly workspace. It is the union
// of every operation the CLI, Mind Gateway, MCP server, and dashboard need,
// grouped into capability interfaces so adapters can depend on just the slice
// they expose. A single implementation (see impl.go, added incrementally as
// each group is lifted from the legacy surfaces) satisfies the whole thing.
//
// The grouping mirrors the frozen Action Catalog from the consolidation
// roadmap. Bold items in that catalog — Run and Deploy — are the capabilities
// that existed on NO prior surface and are introduced here (Phase 2).
type Plane interface {
	Introspector
	Lifecycle
	SourceEditor
	VCS
	DependencyManager
	CommandRunner
	TerminalController
	MutationAuthority
	ServiceInstallation
	GitOps

	// Service returns a handle scoped to one service — file/git operations root
	// at the service directory rather than the workspace, and lifecycle/command
	// operations target that service. This is what service-scoped adapters (the
	// Gateway, mind.yaml) delegate to. See scope.go.
	Service(ctx context.Context, name string) (ServiceScope, error)

	// Close releases terminals and any WorkspaceHost created by New/NewAt.
	// A plane created with NewWithHost leaves that externally owned host open.
	Close() error
}

type GitOps interface {
	RenderGitOps(ctx context.Context, req GitOpsRenderRequest) (gitops.RenderResult, error)
	PlanGitOpsPublish(ctx context.Context, req *gitops.PublishRequest) (gitops.PublishPlan, error)
	PlanGitOpsRollback(ctx context.Context, req *gitops.RollbackRequest) (gitops.RollbackPlan, error)
	ObserveGitOps(ctx context.Context, req *gitops.ObserveRequest) (gitops.ObserveResult, error)
}

// Introspector answers read-only questions about the workspace and any live
// run. Lifted from pkg/web's CLI service (GetWorkspaceInventory, GetActive,
// GetFlowStatus) and pkg/gateway (ListServices, GetProjectInfo).
type Introspector interface {
	// Inventory lists the workspace's modules, services, jobs, and agents.
	Inventory(ctx context.Context) (Inventory, error)
	// Modules lists the workspace's modules with descriptions.
	Modules(ctx context.Context) ([]ModuleInfo, error)
	// Services lists services with per-service detail (agent, version,
	// declared endpoints), optionally filtered to one module.
	Services(ctx context.Context, module string) ([]ServiceDetail, error)
	// DependencyGraph returns the startup-ordered dependency graph rooted at
	// service (or the whole workspace when service is empty).
	DependencyGraph(ctx context.Context, service string) (DependencyGraph, error)
	// FlowStatus reports the state of the currently running flow, if any.
	FlowStatus(ctx context.Context) (FlowStatus, error)
	// Addresses resolves the reachable endpoints for service.
	Addresses(ctx context.Context, service string) ([]Endpoint, error)
	// Configurations returns the service's own configuration plus the
	// configurations of its dependencies, resolved for the active flow. It
	// returns the authoritative core protobuf rather than a shadow DTO,
	// mirroring the per-service leaf contract documented in doc.go.
	Configurations(ctx context.Context, service string) ([]*basev0.Configuration, error)
	// Logs streams log lines for the active session until ctx is done; each
	// line is delivered to emit. follow keeps the stream open for new lines.
	Logs(ctx context.Context, opts LogOptions, emit func(LogLine) error) error
}

// Lifecycle drives services through build/run/test/deploy. Build/Test/Lint/
// Checks are lifted from pkg/mcp + pkg/gateway; Stop from pkg/web's StopFlow;
// Run and Deploy are NEW (Phase 2) — previously only reachable by shelling out
// to the `codefly` binary from an MCP tool.
type Lifecycle interface {
	Build(ctx context.Context, req BuildRequest) (BuildResult, error)
	Test(ctx context.Context, req TestRequest) (CheckResult, error)
	Lint(ctx context.Context, req CheckRequest) (CheckResult, error)
	Compile(ctx context.Context, req CheckRequest) (CheckResult, error)
	RunChecks(ctx context.Context, req CheckRequest) (CheckResult, error)
	// Run starts service and its dependency graph, returning once the flow is
	// started (or, when req.Wait is set, once it is healthy).
	Run(ctx context.Context, req RunRequest) (RunHandle, error)
	// Stop stops the active flow, preserving stateful containers unless
	// req.Destroy is set.
	Stop(ctx context.Context, req StopRequest) error
	// Deploy ships service/module to an environment. It MUST be authorized
	// through MutationAuthority (see PrepareMutation) before it will execute.
	Deploy(ctx context.Context, req DeployRequest) (DeployResult, error)
}

// ServiceInstallation manages durable foreground services with the current
// user's native OS supervisor. It is intentionally separate from Lifecycle:
// RunHandle describes one process-local workspace flow, while these operations
// remain authoritative across CLI processes, logout, and login.
type ServiceInstallation interface {
	InstallService(context.Context, InstallServiceRequest) (InstalledService, error)
	StartService(context.Context, ServiceRef) (InstalledServiceStatus, error)
	StopService(context.Context, ServiceRef) (InstalledServiceStatus, error)
	RestartService(context.Context, ServiceRef) (InstalledServiceStatus, error)
	UninstallService(context.Context, UninstallServiceRequest) error
	ServiceStatus(context.Context, ServiceRef) (InstalledServiceStatus, error)
}

// SourceEditor reads and mutates service source. Lifted from pkg/gateway (the
// only surface with full CRUD, smart edits, and Search).
type SourceEditor interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, content []byte) error
	CreateFile(ctx context.Context, path string, content []byte) error
	DeleteFile(ctx context.Context, path string) error
	MoveFile(ctx context.Context, from, to string) error
	ListFiles(ctx context.Context, dir string) ([]FileInfo, error)
	Search(ctx context.Context, req SearchRequest) ([]SearchHit, error)
	// ApplyEdit applies one or more edits; BatchApplyEdits is the multi-file
	// form. Both go through MutationAuthority when authority is configured.
	ApplyEdit(ctx context.Context, edit Edit) error
	BatchApplyEdits(ctx context.Context, edits []Edit) error
	// Fix runs a language plugin's safe auto-repairs over a target.
	Fix(ctx context.Context, req FixRequest) (FixResult, error)
}

// VCS exposes git status for the workspace repos. Lifted from pkg/gateway.
type VCS interface {
	GitStatus(ctx context.Context, dir string) (GitStatus, error)
	GitDiff(ctx context.Context, req GitDiffRequest) (string, error)
	ApplyPatch(ctx context.Context, req ApplyPatchRequest) (ApplyPatchResult, error)
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
}

// DependencyManager inspects and edits a service's declared dependencies.
type DependencyManager interface {
	ListDependencies(ctx context.Context, service string) ([]Dependency, error)
	AddDependency(ctx context.Context, service string, dep Dependency) error
	RemoveDependency(ctx context.Context, service string, dep Dependency) error
}

// CommandRunner lists and invokes plugin-owned commands for a service.
type CommandRunner interface {
	ListCommands(ctx context.Context, service string) ([]Command, error)
	RunCommand(ctx context.Context, req RunCommandRequest) (CommandResult, error)
}

// TerminalController manages PTY sessions. Present identically on pkg/web and
// pkg/gateway today; unified here.
type TerminalController interface {
	OpenTerminal(ctx context.Context, req OpenTerminalRequest) (TerminalID, error)
	// AttachTerminal streams terminal output to onOutput and writes input from
	// the returned writer until ctx is done.
	AttachTerminal(ctx context.Context, id TerminalID, onOutput func([]byte) error) (TerminalInput, error)
	ResizeTerminal(ctx context.Context, id TerminalID, cols, rows int) error
	CloseTerminal(ctx context.Context, id TerminalID) error
	ListTerminals(ctx context.Context) ([]TerminalID, error)
}

// MutationAuthority is the gate that destructive/outward actions pass through.
// It generalizes the Gateway's ConfigureMutationAuthority / PrepareMutation /
// ApplyPreparedMutation so an LLM-driven caller cannot Deploy or overwrite
// source on a bare RPC — a mutation must be prepared (and, per policy, approved)
// before it applies.
type MutationAuthority interface {
	ConfigureMutationAuthority(ctx context.Context, cfg AuthorityConfig) error
	PrepareMutation(ctx context.Context, m Mutation) (PreparedMutation, error)
	ApplyPreparedMutation(ctx context.Context, token PreparedMutation) (MutationResult, error)
}
