---
name: release-the-fleet
description: Publish affected agents and consumers when a verified agent implementation fix or changed wire protocol requires it. Do not use merely because Core or CLI released or linked Core versions differ; unchanged contracts require no fleet repinning. Covers dependency-ordered publication. For a CLI-only release use cut-a-release.
---

# Release the agent fleet on a new core version

**Steps and the full ordering:** [docs/runbooks/release-the-fleet.md](../../../docs/runbooks/release-the-fleet.md).

## What must not be skipped

- **Check the running contract first.** Different linked Core or artifact versions
  are not incompatibility. Only affected agents need an implementation update;
  do not turn an ordinary Core release into a fleet rebuild.
- **Release after dependencies, never before.** Core → CLI → agents → the modules that pin
  them → the workspaces that compose them. Publishing a module before its agents means the
  module pins a version nobody can pull.
- **Rebuild the CLI before publishing agents** (`codefly self build`). Agent publish runs
  `codefly ci run`, and sequential agent releases collide on one host port without the
  port-isolation fix — see
  [docs/agent-ci-port-isolation.md](../../../docs/agent-ci-port-isolation.md).
- **Re-pin with `codefly agent deps --pin vX.Y.Z --all`.** It moves `go.mod`, `base/*`, and the
  factory locks together. Editing `go.mod` by hand leaves the other two behind.
- **If core touched `companions/*`,** wait for `companions-publish.yml` and then
  `codefly companion verify`. A companion bump with no pushed, publicly pullable image breaks
  every Codefly-native build at the companion pull — not just this release.

## Before you start

Each repo must be clean, on `main`, and synced. `codefly publish` aborts untouched if pre-flight
or CI fails; let it.
