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

The source-workspace pins are Go `0.0.47` and Next.js `0.0.152`.
Both consume Core `v0.3.27` and explicitly implement `BuildCapabilities`.
Go's legacy executor honors Buildx selection through Core; Next.js produces
recipes and rejects requests without an output directory before preparing
build files. Merely upgrading the embedded transport does not advertise
support.

Next.js [`v0.0.152`](https://github.com/codefly-dev/service-nextjs/releases/tag/v0.0.152)
was published at `7e150913857b2ed2734cb3d576eed36781fde974` through
[GoReleaser](https://github.com/codefly-dev/service-nextjs/actions/runs/34637795678).
All four Linux/macOS amd64/arm64 archives match the published SHA-256
manifest. Source qualification, the recipe regression, and the polyglot
gateway regression under race detection pass using downloaded artifacts in
empty caches.

Go [`v0.0.47`](https://github.com/codefly-dev/service-go/releases/tag/v0.0.47)
was published at `a49e3f72ad3a8ebfccfc24d39571ba82570944c6`, the merge of
[Go #62](https://github.com/codefly-dev/service-go/pull/62). Both main-branch
CI gates passed before tagging, and
[GoReleaser](https://github.com/codefly-dev/service-go/actions/runs/34642039756)
passed. All four Linux/macOS amd64/arm64 archives match the published
SHA-256 manifest; Go source qualification passes with the downloaded release.

The verified checksum-manifest SHA-256 values are:

- Go `v0.0.47`: `6698cee42f9f8d6e6d4a5d24240e0e70de32d66d030bf01bcc5ac1ae66aef759`.
- Next.js `v0.0.152`: `b8c2d981dac1c836a569ace8c956544676b8ae70cccbf8b27152f47fdfeef9e1`.

GitHub reports `isImmutable: false` for both releases. These recorded hashes
identify the verified artifacts; checksum verification does not establish
GitHub-enforced release immutability.

The CLI regression starts both real agent binaries and checks the outgoing
Build request, scoped cache policy, selected builder, exact output directory,
and verified recipe response. It also proves Docker execution belongs to the
CLI. Clearing the request's output directory makes the regression fail.
The full CLI suite and the affected orchestration/gateway race tests pass
with the published agents downloaded into empty `CODEFLY_HOME` caches.
Preinstalled development binaries are not used as release evidence.

The [tagged dependency rollout](https://github.com/codefly-dev/cli/issues/625)
now consumes Core `v0.3.27`, Go `0.0.47`, and Next.js `0.0.152`.
After final CLI CI validation and merge, follow the CLI release process above
and record the published CLI version. The CLI manifest remains `0.1.145`;
this checkpoint does not publish a CLI release.
