// Package control is the single control plane for codefly — the one place that
// knows how to run, build, deploy, test, inspect, and mutate a workspace.
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
// control.Plane collapses those into ONE implementation. Every entry point
// becomes a thin ADAPTER that translates its own wire protocol to and from this
// package's types and calls the Plane. No business logic lives in an adapter.
//
//	cobra CLI ─┐
//	gateway  ──┼──▶ control.Plane ──▶ orchestration · plugin gRPC · git · fs
//	mcp      ──┤
//	web      ──┘
//
// # DTO boundary
//
// The Plane speaks its OWN request/response types (this package), never a
// specific transport's generated protobufs. Adapters own the translation. This
// keeps the core independent of any one protocol and lets the interface be
// reviewed and tested on its own terms. It is also what will let pkg/control
// (with pkg/orchestration) later extract into a standalone codefly-dev/engine
// module that both the CLI and a future `codefly server` import as peers.
//
// # Layering
//
// control depends downward on core (shared types) and sideways on
// pkg/orchestration and the plugin gRPC clients. It must NOT be imported by
// plugins/agents — they are the things being controlled, so they sit below it.
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
package control
