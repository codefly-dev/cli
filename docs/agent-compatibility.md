# Runtime agent compatibility

Core and the CLI know protocols and operation requirements, not which named
agents or releases implement them. Startup validates the wire handshake;
discovery checks `AgentInformation.contract`; each operation checks the
capabilities it needs before work. Missing or incompatible declarations fail.
Do not compare an agent's linked Core version with the host, special-case an
agent identity, or maintain a release-to-compatibility table. A Core/CLI release
with unchanged protocols does not require rebuilding or repinning agents.

Artifact selection is separate. A resource may select `publisher/name:version`
for reproducibility, or `publisher/name:latest` (also the omitted-version
default). Neither selection proves compatibility. An explicitly selected
incompatible artifact fails; it must never be replaced silently.

## Source selection

Without an explicit selection, the CLI enumerates installed service agents,
inspects the newest executable of each publisher/name through authenticated
gRPC, and checks its runtime contract. Candidates must advertise source
packaging support and every language declared by the source root's structural
manifest. The manifest identifies languages, never agent names. Exactly one
candidate is required. Unknown roots, no matches and ambiguous matches require
an explicit selection. Uninspectable processes fail discovery rather than
silently making another candidate appear unique. Incompatible declarations are
excluded, but no older version is substituted for that identity.

Use `codefly test source --agent publisher/name[:version]` or an agent
repository's `source.agent` declaration to select explicitly. `agent versions`
reports available artifacts, not compatibility. `agent promote-source` no
longer edits a CLI-owned roster; qualify the chosen artifact with `test source`.
`agent verify-platform <os_arch> <publisher/name>...` verifies explicitly
selected release assets; it does not claim their protocols are compatible.
CI fixture selections and historical qualification records are evidence, not
production admission policy.

`update agent` inspects the resolved candidate before changing the service's
agent version. Failed discovery or an incompatible protocol leaves both the
manifest and in-memory selection unchanged. This is protocol admission, not
proof of operation support, functional readiness or deployment approval.
The command edits that service's declaration; it is not the product-owned
nested replacement workflow tracked in [CLI #753](https://github.com/codefly-dev/cli/issues/753).

Runnable loading checks the live protocol before `Builder.Load`, then requires
Builder support. Solution executors check the live declaration before exposing
a Package/Render client. Artifact verification remains a separate prerequisite;
the protocol check does not authorize an unverified executable.

See [independent-upgrade coverage](independent-upgrade-coverage.md) for what
agent admission delivers and what still needs Core/CLI integration and evidence.

The gateway accepts literal `plugin: publisher/name[:version]` in `mind.yaml`.
It no longer translates names such as `generic-node` to another agent identity.
For polyglot checkouts, `source_agents` maps canonical repository-relative unit
paths to literal selections. A missing entry uses runtime discovery; an
explicit entry is never replaced if incompatible. Commands in a test formula
do not override these selections. Topology language is the optional
`config.language` declaration, not a guess from an agent name.

## Self-hosted packaging

A source agent that packages itself owns its bootstrap command in
`agent.codefly.yaml`:

```yaml
source:
  directory: .
  agent: self
  bootstrap: [./bootstrap-agent]
```

The command executes in `source.directory`. It writes the executable to
`CODEFLY_AGENT_OUTPUT` for `CODEFLY_AGENT_OS` and `CODEFLY_AGENT_ARCH`.
The CLI validates nonempty regular output and installs it into the isolated
qualification home under the manifest's own canonical version. The resulting
process must pass the same runtime protocol checks before packaging. No
agent-name exception or compiled predecessor release exists.

Existing self-hosted repositories must declare this bootstrap to build from an
empty home; merely naming a particular language agent no longer activates a
CLI-owned compiler command. Other repositories declare their source agent or
install a candidate for runtime discovery. Agent CI resolves against the original
home before isolation and retains the exact selected version in a private home
for both source validation and packaging, including older installed versions.
A self-hosted candidate is bootstrapped once, before source validation; it does
not need an intermediate release. Neither path replaces installed agent files.

## What an agent must report

The CLI does not infer an outcome an agent did not state. Two reports are
required rather than reconstructed, and a missing one is refused rather than
read favourably.

**A test run states its own verdict.** `TestResponse.result.state` must be set
to an explicit outcome — `PASSED`, `FAILED`, `ERRORED` or `TIMED_OUT`. Only
`PASSED` is success. `UNKNOWN`, or no `result` at all, is refused by the
orchestrator, reported as a failure by the gateway, and kept as `UNKNOWN`
through code-unit aggregation, where it outranks `FAILED` so one unit without
a verdict decides the run.

The CLI used to fall back to the deprecated `status` field and then to "no
failures were counted". Both are gone: zero counted failures is also what a
run that never executed reports, so an agent that crashed before reporting, or
whose harness output it could not parse, was deploying. An agent that sets only
the deprecated fields now reads as having reported nothing.

**A Kubernetes deployment output states the security property.** For the
restricted portable profile, `KubernetesManifestValidation.restricted` must be
true, static validation must have passed, server-side validation must have
passed or not run, and the violation list must be present and empty. The
contract version must match the one this CLI pins.

`restricted` is the security property; `promotable` names a delivery decision
and the CLI no longer reads it anywhere, including in the render inventory it
persists — that record carries `restricted`, so a render produced by an older
CLI shows a one-line change in `render-inventory.json` on its next render. An
agent emitting only `promotable` is refused at the builder gate and, if it
reached one, as persisted evidence.

The CLI requests only two profiles: the restricted portable profile, and the
ephemeral local-apply profile for a direct apply into a local cluster. A
response naming any other profile is refused, and the local-apply profile is
the one documented exemption from restricted-rendering evidence, because it
produces no promotable artifact and nothing persists it.

