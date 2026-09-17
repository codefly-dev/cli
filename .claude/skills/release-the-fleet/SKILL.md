---
name: release-the-fleet
description: Bring every codefly agent, composed module, and downstream workspace onto one uniform core version and publish them in dependency order. Use after a core change the whole fleet must pick up, when re-pinning agents to a new core, or when a build fails because agents disagree on their core version. Covers core → CLI → agents → composed modules → workspaces. For a CLI-only release use cut-a-release.
---

# Release the agent fleet on a new core version

**Steps and the full ordering:** [docs/runbooks/release-the-fleet.md](../../../docs/runbooks/release-the-fleet.md).

## What must not be skipped

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
