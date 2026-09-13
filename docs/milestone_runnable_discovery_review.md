# Milestone — reviewed Runnable discovery

PR #639 adds strict declaration listing and inspection, Runnable-agent
installation/metadata commands and MCP discovery. It references issue #638;
that lifecycle milestone remains open.

The review corrections make MCP discover agents pinned by Runnable declarations
before installation, reject unknown module filters and check tool registration
errors. Registry traversal no longer copies the large registration value.
Regression tests use real on-disk workspaces and the MCP control path.

Validation passes for the complete Go test suite and targeted race checks of
Runnable discovery and container recovery. The uncapped linter reports zero new
issues against the PR base, using `--new-from-rev` rather than comparing counts
with the repository's existing lint backlog.

The stale Dependabot test is updated to the catch-all grouping policy already
merged in #635. The actual dependency policy is unchanged. This removes the
unrelated baseline test failure that had blocked the coverage job.

## Core and agent compatibility

The CLI core pin crosses the v2 container-recovery marker change. A regression
downloads the real released Go agent 0.0.47 into a private cache, verifies its
embedded core v0.3.27, starts it and reads gRPC metadata. Under the new CLI marker
it supplies no scope acknowledgement; Docker/free initialization is rejected
before any runtime Init RPC. This test runs under ordinary `go test ./...`.

Source merge is permitted; coordinated CLI/fleet release remains gated by #640.
The release runbook records the order and the limits of the tested pairing.
Native/Nix runtime exemptions do not establish compatibility for old agents
which use Docker internally. Those agents must also be rebuilt and qualified
before releasing the new CLI/fleet combination.

## Next

Core #472 must define Runnable loading over gRPC, transfer of native launch/build
evidence and the precise invocation/completion framing. Python's implementation
is a proposal for that contract; Go currently provides bindings only.

After core lands the handoff, CLI #638 adds create/build and local supervision.
The acceptance path must use Orchestration registration, activation and durable
invocations, then repeat with actual invocation Jobs in disposable k3d. Discovery
and agent metadata alone do not satisfy that milestone.
