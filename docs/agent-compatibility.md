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
install a candidate for runtime discovery. Isolated agent CI seeds the newest
installed candidates without inferring compatibility from their versions.
