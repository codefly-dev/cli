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

The candidate source-workspace pins are Go `0.0.47` and Next.js `0.0.152`,
implemented in [Go #61](https://github.com/codefly-dev/service-go/pull/61)
and [Next.js #118](https://github.com/codefly-dev/service-nextjs/pull/118).
Both consume Core `v0.3.27` and explicitly implement `BuildCapabilities`.
Go's legacy executor honors Buildx selection through Core; Next.js requires
recipe output for selected-builder requests and rejects legacy execution
before preparing build files. Merely upgrading the embedded transport does
not advertise support.

The CLI regression starts both real agent binaries and checks the outgoing
Build request, scoped cache policy, selected builder, exact output directory,
and verified recipe response. It also proves Docker execution belongs to the
CLI. Clearing the request's output directory makes the regression fail.
Local validation uses binaries built from the companion changes in an
isolated `CODEFLY_HOME`; it is not evidence of published release assets.

The [tagged dependency rollout](https://github.com/codefly-dev/cli/issues/625)
still requires:

1. Merge the checked companion PRs through the agent release process, then
   publish and checksum-verify Go `v0.0.47` and Next.js `v0.0.152`. The prior
   Go `v0.0.46` release and unpublished Next.js `v0.0.151` candidate cannot
   negotiate the Buildx preflight and are insufficient for this rollout.
2. Qualify both published source plugins and rerun the real-agent CLI
   regression with an empty agent cache. Keep the CLI PR in draft until
   these pins resolve to verified release artifacts.
3. After final validation and merge, follow the CLI release process above
   and record the published CLI version. The CLI manifest remains
   `0.1.145`; this checkpoint does not publish a CLI release.
