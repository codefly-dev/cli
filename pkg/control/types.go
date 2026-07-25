package control

// This file defines transport-neutral workspace-orchestration types. Typed
// per-service leaf behavior uses the existing Codefly agent protobufs in
// pkg/engine; these Go types cover the higher-level operations that do not
// already have an authoritative plugin contract.

// --- Introspection ---

// Inventory is the bounded set of resources in a workspace.
type Inventory struct {
	Workspace   string
	Description string
	Modules     []string
	Services    []string
	Jobs        []string
	Agents      []string
}

// ModuleInfo is a module with its description (for listings).
type ModuleInfo struct {
	Name        string
	Description string
}

// ServiceDetail is a service with the metadata listings need — agent, version,
// and its declared endpoints. Distinct from Endpoint (a resolved, reachable
// address): these are the endpoints as declared in the service manifest.
type ServiceDetail struct {
	Module      string
	Name        string
	Description string
	Agent       string
	Version     string
	Endpoints   []DeclaredEndpoint
}

// DeclaredEndpoint is an endpoint as declared by a service (not yet resolved to
// a host:port).
type DeclaredEndpoint struct {
	Name       string
	API        string
	Visibility string
}

// DependencyGraph is a service's transitive dependencies in startup order.
type DependencyGraph struct {
	Root  string
	Order []string // topologically sorted; dependencies before dependents
}

// FlowState enumerates the lifecycle of a running flow.
type FlowState string

const (
	FlowIdle     FlowState = "idle"
	FlowStarting FlowState = "starting"
	FlowRunning  FlowState = "running"
	FlowStopped  FlowState = "stopped"
	FlowFailed   FlowState = "failed"
)

// FlowStatus reports the state of the active flow and its services.
type FlowStatus struct {
	State    FlowState
	Services []ServiceStatus
}

// ServiceStatus is one service's state within a flow.
type ServiceStatus struct {
	Name    string
	State   FlowState
	Healthy bool
}

// Endpoint is a resolved, reachable service endpoint.
type Endpoint struct {
	Service string
	Name    string
	Type    string // grpc | http | tcp | ...
	Host    string
	Port    int
}

// LogOptions selects which logs to stream.
type LogOptions struct {
	Service string // empty = all services in the active session
	Follow  bool
	Tail    int // last N lines before following; 0 = default
}

// LogLine is a single emitted log record.
type LogLine struct {
	Service string
	Text    string
}

// --- Lifecycle ---

// BuildRequest builds a service (or module when Module is set) image.
type BuildRequest struct {
	Service string
	Module  string
	Env     string
	Push    bool
}

// BuildResult is the outcome of a build.
type BuildResult struct {
	Image     string
	Succeeded bool
	Output    string
}

// CheckRequest is the common shape for lint/compile/checks over a target.
// Command applies only to RunChecks (the shell command to run in the service
// dir); Lint/Compile ignore it.
type CheckRequest struct {
	Service        string
	RuntimeContext string
	Command        string
}

// TestRequest runs a service's tests.
type TestRequest struct {
	Service        string
	RuntimeContext string
	Suite          string
	Filter         string
}

// CheckResult is the outcome of a test/lint/compile/checks invocation.
type CheckResult struct {
	Passed  bool
	Output  string
	Details []CheckFinding
}

// CheckFinding is one diagnostic from a check.
type CheckFinding struct {
	File     string
	Line     int
	Severity string
	Message  string
}

// RunRequest starts a service and its dependency graph.
type RunRequest struct {
	Service        string
	RuntimeContext string
	Wait           bool     // block until the flow is healthy
	Exclude        []string // dependencies to skip
	// ExcludeRoot starts the dependency graph without the origin service —
	// for an embedder that IS the origin, so it doesn't spawn a second copy
	// of itself.
	ExcludeRoot bool
	Headless    bool
}

// RunHandle identifies a started flow so it can be observed/stopped.
type RunHandle struct {
	FlowID string
}

// StopRequest stops the active flow.
type StopRequest struct {
	NameFilter []string
	Destroy    bool // also remove stateful containers
}

// DeployRequest ships a service or module to an environment. Must be authorized
// through MutationAuthority first.
type DeployRequest struct {
	Service string
	Module  string
	Env     string
	DryRun  bool
}

// DeployResult is the outcome of a deploy.
type DeployResult struct {
	Succeeded bool
	Rendered  string // manifests when DryRun/render-only
	Output    string
}

// --- Source ---

// FileInfo describes a workspace file.
type FileInfo struct {
	Path  string
	IsDir bool
	Size  int64
}

// SearchRequest searches workspace source.
type SearchRequest struct {
	Query string
	Path  string // scope; empty = whole workspace
	Regex bool
}

// SearchHit is one search match.
type SearchHit struct {
	Path string
	Line int
	Text string
}

// Edit is a single edit to a file.
type Edit struct {
	Path    string
	OldText string
	NewText string
	// Whole, when set, replaces the entire file with NewText (OldText ignored).
	Whole bool
}

// FixRequest runs plugin-owned auto-repairs.
type FixRequest struct {
	Service    string
	File       string
	DryRun     bool
	Aggressive bool
}

// FixResult is the outcome of a fix.
type FixResult struct {
	Changed []string
	Output  string
}

// --- VCS ---

// GitStatus is a repo's working-tree state.
type GitStatus struct {
	Branch  string
	Dirty   bool
	Ahead   int
	Behind  int
	Changed []string        // paths with any change (convenience)
	Files   []GitFileStatus // per-file detail
}

// GitFileStatus is one changed file's porcelain status.
type GitFileStatus struct {
	Path   string
	Code   string // raw porcelain v1 XY code, e.g. " M", "??", "A "
	Staged bool
}

// GitDiffRequest selects a diff.
type GitDiffRequest struct {
	Dir    string
	Staged bool
	Paths  []string
}

// GitLogRequest selects commits.
type GitLogRequest struct {
	Dir   string
	Limit int
}

// GitCommit is one commit.
type GitCommit struct {
	SHA       string
	ShortHash string
	Author    string
	Message   string
	Date      string
}

// GitCommitRequest creates a commit.
type GitCommitRequest struct {
	Dir     string
	Message string
	Paths   []string // empty = all staged
}

// --- Dependencies ---

// Dependency is a language package a service depends on (a Go module, npm
// package, …) — resolved through the service's Code plugin, not a codefly
// service reference.
type Dependency struct {
	Name    string
	Version string
	Direct  bool // directly imported (vs transitive); set on list results
}

// --- Commands ---

// Command is a plugin-owned command a service exposes.
type Command struct {
	Name        string
	Description string
	Usage       string
	Tags        []string
	Destructive bool
}

// RunCommandRequest invokes a plugin command.
type RunCommandRequest struct {
	Service string
	Command string
	Args    []string
}

// CommandResult is the outcome of a plugin command.
type CommandResult struct {
	ExitCode int
	Output   string
}

// --- Terminal ---

// TerminalID identifies a PTY session.
type TerminalID string

// OpenTerminalRequest opens a shell scoped to a workspace resource.
type OpenTerminalRequest struct {
	Service string
	Module  string
	Shell   string
}

// TerminalInput writes bytes to a terminal session.
type TerminalInput interface {
	Write(p []byte) (int, error)
	Close() error
}

// --- Mutation authority ---

// AuthorityMode controls how mutations are gated.
type AuthorityMode string

const (
	// AuthorityOpen applies mutations immediately (trusted local caller).
	AuthorityOpen AuthorityMode = "open"
	// AuthorityPrepared requires PrepareMutation + ApplyPreparedMutation.
	AuthorityPrepared AuthorityMode = "prepared"
)

// AuthorityConfig configures the mutation gate.
type AuthorityConfig struct {
	Mode AuthorityMode
}

// MutationKind classifies what a mutation touches.
type MutationKind string

const (
	MutationFile   MutationKind = "file"
	MutationDeploy MutationKind = "deploy"
)

// Mutation is a proposed change to be prepared before it applies.
type Mutation struct {
	Kind    MutationKind
	Summary string
	// Payload carries the kind-specific request (e.g. an Edit or DeployRequest).
	Payload any
}

// PreparedMutation is an opaque, fenced token returned by PrepareMutation and
// consumed by ApplyPreparedMutation.
type PreparedMutation struct {
	Token string
}
