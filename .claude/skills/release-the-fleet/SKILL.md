---
name: release-the-fleet
description: Publish affected agents and consumers when a verified agent implementation fix or changed wire protocol requires it. Do not use merely because Core or CLI released or linked Core versions differ; unchanged contracts require no fleet repinning. Covers dependency-ordered publication. For a CLI-only release use cut-a-release.
---

# Release affected agents

**Steps and the full ordering:** [docs/runbooks/release-the-fleet.md](../../../docs/runbooks/release-the-fleet.md).

## What must not be skipped

- **Check the running contract first.** Different linked Core or artifact versions
  are not incompatibility. Only affected agents need an implementation update;
  do not turn an ordinary Core release into a fleet rebuild.
- **Publish changed dependencies before their consumers.** This is not a mandatory
  Core/CLI/fleet/ancestor release chain. Skip components that need no implementation change.
  No release or merge is authorized merely by invoking this skill.
- **Verify the CLI tooling needed to publish.** Agent publish runs `codefly ci run`;
  use a CLI with the required port-isolation behavior, and rebuild only when needed. See
  [docs/agent-ci-port-isolation.md](../../../docs/agent-ci-port-isolation.md).
- **Update only affected agents:** `codefly agent deps --dir /path/to/affected-agent --pin vX.Y.Z`
  moves that agent's `go.mod`, `base/*`, and factory locks together. Do not use a blanket
  `--all` update to infer compatibility from matching build dependencies.
- **If core touched `companions/*`,** wait for `companions-publish.yml` and then
  `codefly companion verify`. A companion bump with no pushed, publicly pullable image breaks
  every Codefly-native build at the companion pull — not just this release.

## Before you start

Each repo must be clean, on `main`, and synced. `codefly publish` aborts untouched if pre-flight
or CI fails; let it.
