# Agent CI conformance & port isolation

Reference for how `codefly agents ci` validates an agent and why sequential
agent CI runs must not share a host port space. For the fleet-wide release
procedure that relies on this, see
[docs/runbooks/release-the-fleet.md](runbooks/release-the-fleet.md).

## Agent conformance CI

`codefly agents ci` validates a single agent by driving it through a real,
throwaway workspace instead of unit-testing agent internals:

1. A per-run temp root and `$CODEFLY_HOME` are created
   (`os.MkdirTemp("", "codefly-agent-ci-*")`), so the **filesystem** is
   isolated per run.
2. A workspace is generated with a fixed identity — workspace
   `agent-conformance`, module `app`, service `app/subject` — pinned to the
   agent under test (`cmd/agents/ci.go`, `runGeneratedServiceConformance`).
   Attach-source agents instead copy a shipped fixture workspace.
3. The gate runs `codefly ci run --all` inside that workspace
   (`agentConformanceGateArguments`), which executes the full plugin gate:
   `verify → sync-drift → lint → compile → test → audit → sbom → build`.

## Why CI runs must not share a port space

Host ports are allocated **deterministically from service identity**. Core's
`network.ToNamedPort` hashes `workspace-module-service-endpoint` into a port
(`core/network/port.go`). Because every agent's conformance workspace has the
**identical** `agent-conformance/app/subject` identity, every agent's CI hashes
to the **same** host port (observed: 38460).

Consequence: a single process leaked by one agent's CI (e.g. a Nix-launched
`redis-server` or `postgres`) holds that port and **blocks every agent that
runs afterwards**, producing failures that look like bugs in the *next* agent.

## The two levers

Both route through the flow into `RuntimeManager` in core:

- **`--temporary-ports`** (used automatically by agent conformance): asks the
  OS kernel for ephemeral ports (`RuntimeManager.WithTemporaryPorts` →
  `GetFreePort`). The kernel never re-hands a bound port, so a leak from a prior
  run cannot collide with a later run. This is the right tool for CI because
  the harness does not need to know endpoint names ahead of time.
- **`--override-port endpoint=port`** (repeatable): pins a specific endpoint to
  a chosen host port, keyed by `EndpointDestination`
  (`module/service/endpoint`, e.g. `app/subject/rest=45001`). An override wins
  over both the hash and temporary ports. Use it to anchor a known endpoint
  (external tooling, a reproducer) while the rest of the graph allocates
  normally. Two endpoints pinned to the same port is reported, not silently
  double-allocated.

Both flags are on `codefly ci run`; `codefly run service` also carries
`--temporary-ports` and `--override-port`.

> This stops a leak from *propagating* to the next agent. Reliably *reaping*
> the leaked processes themselves is a separate concern (codefly-dev/cli#429).
