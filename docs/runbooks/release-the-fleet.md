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


## Runnable core pin and v2 marker: source merge versus release

CLI #639 may merge as a source increment. It does not publish a CLI version or
establish compatibility with the currently released agent fleet. CLI #640 remains
the gate for a coordinated release:

1. Build and qualify every Docker-creating agent against core `6a40c4bf28ac` or
   later, including agents which use containers internally from a native/Nix mode.
   [The rollout inventory](../container-recovery-rollout.md) is the list of record:
   it classifies every agent repository by how it reaches a container and says
   which ones must be rebuilt.
2. Publish those compatible agents before or together with the CLI release, then
   update workspace pins. An old CLI with a new-core agent is also unsupported for
   container creation; upgrade the pair together.
3. Test the actual runtime pairings before declaring them supported. Do not infer
   wire compatibility from the Go module release line alone.

`TestContainerRecoveryRejectsAReleasedLegacyAgent` downloads the real Go agent
`0.0.47` into a private cache, checks its embedded core `v0.3.27`, starts it through
core's process manager, and reads its gRPC metadata under the new CLI's v2 marker.
The agent returns no acknowledgement. The CLI rejects Docker/free initialization
before issuing any runtime Init RPC. This test runs in the ordinary Go suite.

The existing guard exempts native and Nix runtime contexts. It is not proof that
legacy agents using Docker internally are compatible, and source merge does not
remove the release gate for those paths. No Core tag carries the marker at all —
`v0.3.27` is the newest tag and recovery landed after it — so every currently
published agent is pre-marker, and the guard rejects one on the default `free`
runtime context, not only under Docker. Core's Docker runner is the marker
consumer; core's agent interceptor exposes its acknowledgement in gRPC headers.
The CLI sets the marker at run preparation and validates the acknowledgement.

### Native and Nix selections project ownership too

`initRunService` decided whether to prepare container recovery from the same
predicate that decides whether to sweep, and the sweep is deliberately off for
`native` and `nix` so those selections never require a Docker daemon. Those are
different questions. A `companion` agent — one that reaches Docker only through
a Core package — creates containers even on a run where nothing selects a
Docker-capable context, and with no marker to inherit it created them carrying
no `codefly.recovery-scope` label at all, which no later sweep can match.
Projection is now unconditional; the sweep keeps the selectable-context
predicate, so a Local/Nix run still never contacts a daemon.

[The rollout inventory](../container-recovery-rollout.md) stays the list of
record for which agents this affects and what remains unqualified.

`.github/workflows/container-recovery-native.yml` qualifies the path on every
change: it rebuilds every row that reaches a container through a Core companion
— `service-go`, `service-go-grpc`, `service-rust`, `service-nextjs` and
`service-python-fastapi` — against the Core this checkout pins and requires the
projected identity back verbatim in the `codefly-container-recovery-scope`
header, retaining each log as an artifact. The matrix cannot quietly shrink
back to one row: `pkg/conformance` fails if it and the inventory disagree about
which rows reach a container that way, or if one repository is listed twice.

Each rebuilt binary is held to the repository its row names, read from the
binary's own build information. The agent identity a row carries only selects
the cache path the binary is installed at, and an agent answers the
acknowledgement the same wherever it sits, so without that check a row passes
while qualifying a different agent's build entirely. Separately, a weekly
scheduled job reports any pinned ref whose default branch has moved past it:
nothing else refreshes those pins, and a gate qualifying source the fleet no
longer publishes reports success for work nobody did.

Observed on 2026-09-13, rebuilt on `b3470f0096cd` and run against the real
agent processes: all five echo the identity exactly — `go` 0.0.48, `go-grpc`
0.1.36, `rust` 0.0.35, `nextjs` 0.0.152, `python-fastapi` 0.0.98. Building
`service-go-grpc` on the pre-marker Core it is published against (`v0.3.27`)
instead is rejected on the embedded-Core assertion, before the header is read
at all — so the qualification refuses a published-generation binary rather than
reporting an empty acknowledgement for it. That empty acknowledgement was
recorded separately, for `service-go` at its own pin `177cb87e85ee`, and is
what every published agent returns today.

**A native run holding a container-pinned service now fails against the
published fleet.** `preferences.codefly.yaml` overrides the launch context per
service, so `--runtime-context=native` with `by-service: {postgres: container}`
projects a marker and brings that runner under `Runner.Init`'s acknowledgement
guard for the first time. Every published agent answers with none, so the run
stops with "did not acknowledge this run's container recovery scope" rather
than silently creating unlabeled containers. That is the intended outcome and
the reason the rebuild gates the release — but it is a new, loud failure on a
path that used to be silent, so expect it as soon as this ships.

This closes the CLI-side half of the native `companion` qualification.

### `codefly generate` is qualified against a real daemon

`generate` is the one command that reaches Docker without spawning an agent:
`generate proto`, `contracts` and `client` build their containers in the CLI
process itself. No fleet rebuild can make those recoverable — there is no agent
in the path — and no unit test can prove they are, because the labels and the
sweeps that read them exist only against a real daemon.

`TestAnInterruptedGenerateLeavesARecoverableContainer` runs in `go.yml`'s
`control-integration` gate. It re-execs an interrupted generate — a process that
builds its container and dies before the defer that would shut it down — and
then requires that the leftover carries `codefly.recovery-scope` and the durable
namespace, that a later run's *exact-scope* sweep walks past it once that run
renamed the naming scope, and that the disposable sweep collects it. Observed
2026-09-13 against Docker 29.4.0.

A host that can prove no durable identity **fails** this proof rather than
skipping it. The disposable sweep is keyed on that namespace and does nothing
without one, so a skip would leave the gate green having checked the labels and
neither sweep — which is the silent pass the proof exists to close. Expect it
on a runner with no usable machine identity, such as a container-based job.

Expect the exact-scope sweep to miss these containers: that is the documented
behavior, not a defect. A run is free to choose a different naming scope
(`--naming-scope`, a non-local `--env`, the invocation id `--temporary-ports`
generates), and the exact-scope hash includes it. What collects the leftover is
the disposable sweep, keyed on the naming-scope-independent namespace, and it
reaches these containers only because `generate` marks them ephemeral.

This is the only container-recovery dimension the CLI can qualify on its own.
It does not lift the release gate: the remaining rebuild, qualification,
publishing and matrix items are carried in
[cli#662](https://github.com/codefly-dev/cli/issues/662).

## Upgrading the generic Go packager

The canonical `codefly.dev/go` service agent can qualify its own successor even
when the published predecessor has an older cross compiler. Agent qualification
builds one native seed from the candidate's standalone source into a private,
temporary plugin home, using the candidate's exact version. That seed serves
`Builder.Package` for the ordinary native and Linux artifacts and their SBOMs.
Source tests, audits, conformance, drift checks and required release platforms
remain mandatory. No candidate binary is stored under a released predecessor's
identity, and the temporary seed is never a published artifact.

Other agents continue to use their explicit source agent or the CLI compatibility
roster. After publishing a packager, qualify its adoption through the normal
source-agent promotion flow before releasing the dependent fleet.

Fresh generated and copied conformance workspaces record an empty Git baseline
and their initial source snapshot before invoking the workspace gate. This gives
the same integrity checks explicit change bounds and proof of newly introduced
manifest ownership; it does not exempt conformance from integrity verification.
