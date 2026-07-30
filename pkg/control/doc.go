// Package control is Codefly's workspace-level control facade. It composes the
// transport-independent runtime behavior in pkg/engine with orchestration,
// filesystem, git, mutation-authority, and terminal capabilities.
//
// # Why this package exists
//
// Historically the same operations were implemented three times, once per entry
// point, with subtle drift and gaps between them:
//
//   - pkg/web   — the dashboard/CLI gRPC service (observe a live flow; stop it)
//   - pkg/mcp   — MCP tools over stdio (a mix of direct plugin-gRPC calls and
//     shelling out to the `codefly` binary)
//   - pkg/gateway — the Mind Gateway gRPC service (the richest surface: files,
//     git, build/test/lint, terminals — but no Run or Deploy)
//
// A long-lived adapter creates one engine.WorkspaceHost and binds a Plane to it.
// The host owns agent processes, active flows, source behavior, and tools;
// adapters translate their own wire protocol and keep only boundary-specific
// policy such as Gateway authentication and signed mutation permits.
//
//	cobra CLI ─┐
//	gateway  ──┼──▶ control.Plane ──▶ engine.WorkspaceHost
//	mcp      ──┤
//	agent loop ┘                         ├─ service-agent gRPC
//	                                    ├─ orchestration flows
//	                                    └─ in-process toolbox
//
// # Contract boundaries
//
// Per-service leaf behavior uses the existing typed Codefly agent protobufs:
// those messages are already the contract between Codefly and language agents,
// so a second shadow DTO would add translation without isolation. Workspace
// orchestration uses the small Go types in this package. MCP and the Mind
// Gateway remain adapters over those behaviors. A future deployable Codefly
// gRPC service can expose the stable subset it needs without making an
// unvalidated network schema the in-process engine API.
//
// RunRequest.Profile selects a typed run profile declared under run-profiles in
// workspace.codefly.yaml. Profiles exclude dependency services and workspace
// configuration dependencies from local run composition only. RunRequest.Exclude
// adds service exclusions to the selected profile; profile and explicit
// references are resolved and validated before any agent starts. Build, test,
// and deployment operations do not read or apply run profiles.
//
// # Layering
//
// control depends on pkg/engine and pkg/orchestration. engine depends downward
// on core's shared contracts but never on a transport adapter. Plugins must not
// import engine or control: they are the behavior providers being supervised.
//
// # Failure convention
//
// Two failure styles coexist, by design. A "check" that ran but did not pass
// (RunChecks, Lint, Compile, a plugin RunCommand) reports failure in its result
// (CheckResult.Passed=false / CommandResult.ExitCode!=0) with a nil error — the
// call succeeded, the checked thing failed. An operation that could not be
// carried out (bad input, workspace not found, transport failure) returns a Go
// error. Adapters should surface both.
//
// # Authority
//
// Destructive or outward actions (notably Deploy, and any file/git mutation)
// flow through the mutation-authority gate rather than executing on a bare
// call, mirroring the prepared-mutation pattern the Gateway already uses. See
// the MutationAuthority group.
//
// # Durable local services
//
// ServiceInstallation is the OS-native lifecycle for long-running embedded
// products. It writes a per-user LaunchAgent on macOS or systemd user unit on
// Linux and always leaves daemonization and crash restart to that supervisor.
// Its versioned materialized contract is distinct from a process-local RunHandle
// and never destroys product data or carries sensitive environment values.
package control
