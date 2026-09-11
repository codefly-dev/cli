# Runbook: Release the whole agent fleet on a new core version

Bring every codefly agent onto a single uniform core version and publish it.
Use this after a core change that the fleet must pick up (the endpoint of an
epic like codefly-dev/cli#435).

## When to use

- Core released a new version and the agents, composed modules, and downstream
  workspaces must all move onto it together.

## Order (release after dependencies, never before)

Agents before the modules that pin them; modules before the workspaces that
compose them.

1. **Core** — merge the core change, then in the core checkout on a clean,
   synced `main`:
   ```bash
   codefly publish patch        # bumps version/info.codefly.yaml, tags, pushes
   ```
   If this change touched any `companions/*/info.codefly.yaml`, wait for
   `companions-publish.yml` to finish pushing the bumped tags, then run
   `codefly companion verify` before moving on — a companion version bump
   that isn't backed by a pushed, publicly pullable image breaks every
   Codefly-native build at the companion pull, not just this release.
2. **CLI** — pin to the new core and release:
   ```bash
   GOWORK=off go get github.com/codefly-dev/core@vX.Y.Z && go mod tidy
   # commit, merge the CLI PR, then:
   codefly publish patch        # in pkg/cli mode
   codefly self build           # install the new binary — it carries the CI port-isolation fix
   ```
   The rebuilt binary matters: agent publish runs `codefly ci run`, and the
   [port-isolation](../agent-ci-port-isolation.md) fix is what keeps sequential
   agent releases from colliding on one host port.
3. **Agents** — re-pin every agent and publish each:
   ```bash
   codefly agent deps --pin vX.Y.Z --all   # pins go.mod + base/* + factory locks (cli#434)
   # commit each repo, then per agent repo (clean, on main, synced):
   codefly publish patch                   # runs release CI + creates the GitHub release
   ```
   `codefly publish` works for every agent kind — service, module, toolbox,
   provider (cli#433). It aborts untouched if pre-flight or CI fails.
4. **Composed modules** — only after the agents they pin are published:
   ```bash
   cd module-saas-starter
   codefly update workspace     # refresh the pinned agent versions
   codefly publish patch
   ```
5. **Downstream base-sync workspaces** — move the base ref once the starter
   publishes (e.g. `obin-ai/lodestar`):
   ```bash
   codefly sync module          # reconcile the immutable base + overlay
   ```

## Gotchas

- **`codefly publish` pre-flight is strict**: clean tree, on `main`, in sync
  with `origin/main`, tag not already present. It never force-pushes. Resolve
  any divergence by hand — a repo on a feature branch or with a dirty tree is
  skipped/aborted, not forced.
- **A partial `--pin` used to pass silently** (cli#434). `--pin` now updates
  every lock the agent owns (root `go.mod`, `base/*`, factory templates); a
  stale template lock fails the pin instead of reporting success.
- **Sequential agent CI shares a deterministic port** unless isolated — see
  [agent-ci-port-isolation.md](../agent-ci-port-isolation.md). Use a CLI built
  after that fix landed.

## Checklist

- [ ] Core tagged and fetchable (`git ls-remote --tags <core> vX.Y.Z`)
- [ ] If companion versions changed: `companions-publish.yml` finished and
      `codefly companion verify` passes
- [ ] CLI pinned to the core tag, released, and reinstalled (`codefly version`)
- [ ] Every agent re-pinned and published (service / module / toolbox / provider)
- [ ] `module-saas-starter` refreshed and published after its agents
- [ ] Downstream base-sync refs moved

## Tagged Core rollout checkpoint (2026-09-11)

The CLI now consumes Core `v0.3.27`, tagged at
`9267a219f28478d090c90b69b5b99900c52f8ab9` by the
[version-tag workflow](https://github.com/codefly-dev/core/actions/runs/34624729792)
after [Core CI passed](https://github.com/codefly-dev/core/actions/runs/34623653367).
The tag contains merged Core #458, including Buildx forwarding and pre-build
capability negotiation, and resolves through Go modules. The conformance
matrix already records the matching `v0.3.27` release line.

The source-workspace catalog consumes Go agent
[`v0.0.46`](https://github.com/codefly-dev/service-go/releases/tag/v0.0.46),
whose tag resolves to `504360afdfc196f59a36369f052ffa5f4806db26` and consumes
Core `v0.3.26`. All four Linux/macOS amd64/arm64 release archives match the
published SHA-256 checksums. The existing `codefly agent promote-source`
gate downloaded the exact release into an isolated cache and passed a real
Go source test over gRPC before generating the catalog update. GitHub reports `isImmutable: false` for this
release; checksum verification does not establish GitHub-enforced immutability.

The [tagged dependency rollout](https://github.com/codefly-dev/cli/issues/625)
still requires:

1. Publish and verify Next.js `v0.0.151` from merged
   [Next.js #116](https://github.com/codefly-dev/service-nextjs/pull/116),
   tracked by [Next.js #113](https://github.com/codefly-dev/service-nextjs/issues/113).
   Until its release assets exist and pass source-workspace qualification,
   the catalog retains Next.js `0.0.141`.
2. After the final catalog pins pass CLI checks and the PR merges, follow
   the CLI release process above and record the published CLI version.
   The CLI manifest and latest published release remain `0.1.145`; this
   checkpoint does not publish a CLI release.
