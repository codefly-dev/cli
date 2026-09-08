# Roadmap: drop the Docker registry requirement from core and plugins

Status: proposed roadmap, not yet an implementation plan for any single issue

Scope: `codefly-dev/core`, `codefly-dev/cli`, and every "plugin" repo — services
(`service-*`), toolboxes (`toolbox-*`), modules (`module-*`) — anything with an
`agent.codefly.yaml` that builds or references a Docker image.

## Where this comes from

codefly-dev/core#406 and codefly-dev/cli#566 are the first, narrowly-scoped fix
for today's actual pain: five-plus repos each hardcode a registry literal for
the six companion images (`codeflydev/<name>` on Docker Hub, moving to
`ghcr.io/codefly-dev/<name>`), and that sprawl is what let `service-redis`'s
package go accidentally private and 401 in production. That fix is: one Go
constant, `resources.ImageRegistry`, that every consumer imports instead of
spelling out a registry string.

Core's half has landed — codefly-dev/core#407 (`80611ae`) adds
`resources.ImageRegistry = "ghcr.io/codefly-dev"` and
`resources.PublishedImage(name, tag) DockerImage`, and derives all six
companions through it. It is on core `main` but not yet in a released tag
(`v0.3.23` points at `e7b6566`, which predates it), so this repo still pins
`core v0.3.22` and `Companion.Tag()` (`cmd/companion/companion.go:160`) still
returns `codeflydev/<name>:<version>`. cli#566 is the consuming change.

A constant in a shared package is a real improvement — one place to change
instead of five — but it is still a compile-time literal. Changing the
registry (a new mirror, a self-hosted instance, an air-gapped deployment)
still means a new core release, a new cli release, and every plugin repo
re-pinning both. The question this roadmap answers: what would it take for no
codefly repo — core, cli, or any plugin — to hardcode a registry hostname at
all, the same way `toolbox-docker` already means the CLI doesn't need to know
Docker's own mechanics directly?

## The governing constraint: registry ownership leaves core

The operator's stated direction (recorded on the cross-agent blackboard by the
core#406 session, no issue filed yet) is that **the registry is to be
abstracted away from core entirely: core ships only the Dockerfile, and the
CLI owns registry resolution and push.**

That makes `resources.ImageRegistry` / `resources.PublishedImage` a deliberate
waypoint, not a foundation — the right thing to consume today, and a seam to
retire, not to build on. Every phase below is written to move *toward* that
end state, which means the resolver belongs in this repo (the CLI), not in
core. Concretely, for anyone picking up a phase:

- Do not add core-side registry machinery to support a CLI consumer. One
  constant and one call-site shape is the whole core surface, and it shrinks
  from there.
- Do not spread registry strings into new core packages.
- Core keeps declaring *build inputs* (Dockerfile, name, version) and *config
  schema*; the CLI resolves those into an image reference and pushes it.

The precedent for the split already exists: `EnvironmentRegistry` lives in
core as a two-field declarative type (`URL`, `Auth`) whose own doc comment
says "The CLI handles auth side-effects based on this value". Core declares,
the CLI acts.

## Two registry axes that already exist, separately

codefly already has two independent places where "which registry" is a
decision, and they're solved to different degrees:

1. **The workspace/environment registry** — where a *user's own* service
   images land when they run `codefly build` / `codefly deploy`. This is
   already data, not code: `env.Registry.URL` + `env.Registry.Auth` in the
   workspace config, resolved at build time by
   `pkg/builder/registry_auth.go` — in this repo, CLI-side, exactly where the
   constraint above says this logic belongs. Auth is pluggable per backend
   (`ecr`, `acr` fully implemented; `gar` and `ghcr` are still stubs that
   return "not yet wired" and print a manual `docker login` instead of
   running one).
2. **The codefly-owned artifact registry** — where *codefly's own* shared
   images live: the six companions, and each service/toolbox repo's runtime
   image. This is the one #406/#566 are consolidating from scattered literals
   into `resources.ImageRegistry`. Unlike axis 1, it has no config surface at
   all today — it is, and after #406/#566 remains, a fixed value baked into
   the binary, and it is resolved on the *core* side of the split.

The roadmap below is about giving axis 2 the same shape axis 1 already has
(config in, resolved reference out) and the same home, `pkg/builder` — and
then giving plugin repos a way to stop naming a registry even in their
config. What the two axes genuinely share is the auth helper
(`RegistryLogin`) and a package; they do not share a registry value, and must
not. Axis 1 pushes a *user's* images to a *user-owned* registry with that
user's cloud credentials; axis 2 pulls *codefly's* images, anonymously, from
a registry codefly owns. Co-location, not one resolver.

## Phase 0 — now (#406, #566, service-redis#34, service-object-storage#15, service-mssql#12)

One canonical registry (`ghcr.io/codefly-dev`), one Go constant
(`resources.ImageRegistry`), public packages, anonymous pulls, CI-only
pushes, digest pins in consumers. Land this as scoped — it fixes the
immediate incident class (private-package 401s, Docker Hub sprawl) with the
smallest possible blast radius, and four other sessions are already
coordinated against this exact plan. Do not reopen or expand it for the
phases below.

## Phase 1 — the CLI resolves the registry, not core

Move the decision from a core constant to a CLI resolver. The natural home is
`pkg/builder`, next to `registry_auth.go`, which is where axis 1 already
keeps its registry state. `resources.ImageRegistry` becomes the last-resort
default of a resolution order rather than the only value.

**Precedence must match the order axis 1 already documents** at
`cmd/build/service.go:92-101` — explicit imperative override wins over
declarative config, which wins over the compiled default:

1. An explicit `--artifact-registry` flag (the analogue of axis 1's `--org`,
   whose own comment reads "explicit override; wins everything").
2. `CODEFLY_ARTIFACT_REGISTRY` environment override, for CI and one-off
   overrides without touching workspace config.
3. Workspace/cell config (a new `env.ArtifactRegistry`-shaped field declared
   in core alongside the existing `env.Registry`, read and acted on by the
   CLI — declaration in core, resolution in the CLI, per the constraint
   above).
4. A compiled-in default when none are set — initially
   `resources.ImageRegistry`; once no CLI path reads the core constant
   directly, the default moves into the CLI and the core constant is retired
   (see the deprecation note below). That retirement is the actual "registry
   out of core" step.

Getting this order backwards is not cosmetic: an air-gapped workspace is
exactly the deployment that sets config, so ranking config above the
override would make the flag and env var permanently dead in the one
environment the payoff paragraph below advertises them for.

Step 4 needs a released core — `resources.ImageRegistry` does not exist in
`v0.3.22`, which this repo pins (see "Where this comes from"). No *additional*
core release is required beyond the one Phase 0 already needs for cli#566,
but Phase 1 cannot start before that release is cut.

Consumers then call the CLI resolver instead of `resources.PublishedImage`:
`Companion.Tag()` (`cmd/companion/companion.go:160`) and the push path
(`cmd/companion/build.go:426`, reached via `cmd/companion/push.go`) are the
first two call sites.

**Do not resolve axis 2 through axis 1's existing state.** `pkg/builder`
holds `var repository` (`builder.go:11`), a mutable package-level global that
`cmd/build/service.go:105`, `cmd/build/module.go:52` and
`pkg/gitops/orchestrate.go:381` all write via `SetRepository`. Co-locating
the two axes in one package is fine; sharing that variable is not — a
`codefly build` that points it at the user's ECR would silently redirect the
next companion pull there. Axis 2's resolved value must be its own,
independently derived.

**The override tiers are an image-substitution surface, and must ship with
their guardrail in this phase, not Phase 3.** Companions are addressed by
mutable tag, not digest — `Companion.Tag()` returns `name:version` — and a
companion executes build logic rather than merely supplying data
(`cmd/generate/proto.go:49` runs buf *inside* the proto companion). Anything
that can set an environment variable in CI could therefore redirect every
companion pull to an attacker-controlled host. Phase 1 must land, together
with the resolver: digest pinning for companion references (the same
treatment Phase 0 already gives runtime images), and verification that a
resolved reference's digest matches the pin before the image is used. A
resolver that accepts an arbitrary host over an unpinned tag is a
supply-chain regression against Phase 0, not a neutral refactor.

This is additive to axis 1's existing auth plumbing: finish `RegistryLogin`'s
`gar` and `ghcr` cases in `pkg/builder/registry_auth.go` (today both return
"not yet wired") so authenticating against a resolved artifact registry is a
real login call, not a printed instruction, regardless of which axis is
asking.

Payoff: a self-hosted or air-gapped deployment points every companion and
runtime-image pull at its own mirror by setting one value — no further core
or cli release required once Phase 1 has shipped. This is also the natural
place to plug in the per-cell
ACR mirroring that core#406 already references (images published once to
ghcr, mirrored by digest into each cell's private registry) — mirroring
becomes a resolver policy instead of a special case bolted onto individual
consumers.

## Phase 2 — plugin repos stop naming a registry at all

Once resolution is centralized in the CLI (Phase 1), audit every plugin repo
for a registry literal outside that resolver: Dockerfile `FROM` lines,
`info.codefly.yaml` / `runtime-image.json` image references, and Go
`DockerImage{}` literals.

**Only codefly-published images are in scope.** The audit has to separate two
kinds of literal that look identical to a grep:

- *Images codefly publishes* — companions, agent runtime images, each
  service/toolbox repo's own image. These name `ghcr.io/codefly-dev/...` or
  `codeflydev/...` today and are what Phase 2 replaces with "name + tag (or
  digest)" only, resolved through the same CLI entry point a companion uses.
- *Images codefly consumes from upstream* — `FROM python:3.12-slim` and
  `FROM alpine:3.19` in `service-mssql`, `ghcr.io/goreleaser/goreleaser-cross`
  in this repo's `.github/workflows/release.yaml`. These **must stay
  literal**. They live in registries codefly does not own and cannot resolve
  through its own artifact registry; routing them through it would break the
  build. Note that `pkg/cliupdate/release_contract_test.go:110` asserts the
  goreleaser reference is present, so "fixing" it reddens a contract test
  that exists to pin it.

Phase 3's mirroring is the only mechanism that can legitimately relocate an
upstream base image, and then only by digest with the original as the source
of truth. Phase 2 does not touch them.

The chokepoint has to be a component that actually moves images. Today that
is the CLI's own `docker` invocations: `cmd/companion/build.go:426`
(`docker push`) and `cmd/companion/verify.go:124` (`docker manifest inspect`)
for axis 2, and `pkg/orchestration/builder.go:276` for axis 1. Routing those
through the Phase 1 resolver in `pkg/builder` is the pilot — one real
consumer fully off literal registries — after which the pattern extends
rather than gets redesigned.

`toolbox-docker` is **not** a candidate, despite the name. It is a read-only
introspection plugin for agents (`list_containers`, `list_images`,
`inspect_container`); its README states mutating operations are deliberately
omitted, and its declared sandbox grants only `/var/run/docker.sock` while
denying network by default. It cannot pull or push, and giving it that
ability would mean discarding the sandbox posture it was built around.
`runners/dockerrun` in core is likewise wrong for this, because siting the
chokepoint there pushes registry knowledge back into core against the
governing constraint.

Exit test for this phase, run in each plugin repo:

```
grep -rn 'ghcr\.io/codefly-dev\|codeflydev/' \
  --include='*.go' --include='*.yaml' --include='*.yml' \
  --include='*.json' --include='Dockerfile*' .
```

It must return nothing outside the resolver itself and its tests. Three
details are load-bearing, each of which the obvious version of this command
gets wrong:

- `--include='*.json'` — without it the command cannot see
  `runtime-image.json`, which is both named above and the exact file that
  carried the `service-redis` 401. A test that is blind to the incident's own
  file certifies a pass on the regression it exists to catch.
- `--include='Dockerfile*'` — `--include` matches the basename exactly, so a
  bare `Dockerfile` pattern misses `service-mssql`'s
  `migrations/Dockerfile.alembic` and `templates/builder/Dockerfile.tmpl`.
- Matching the codefly-owned namespaces (`ghcr.io/codefly-dev`,
  `codeflydev/`) rather than a bare `ghcr.io` — a bare host pattern flags
  every upstream image quoted above and turns the exit test into a source of
  false positives that engineers learn to ignore.

### Retiring the core constant

`ImageRegistry` and `PublishedImage` are exported from
`github.com/codefly-dev/core/resources`, a module every plugin repo imports,
so "delete it" is a breaking change to a shared API and needs a sequence
rather than a commit:

1. Core's own six companion definitions stop deriving their image through
   `PublishedImage` — core keeps declaring the Dockerfile, the name and the
   version, and stops naming a registry. This is the step that has no
   worked-out shape yet, and whoever picks up Phase 1 should design it first,
   because everything below is blocked on it.
2. Mark both symbols `Deprecated:` with a pointer to the CLI resolver, and
   cut a core release carrying the deprecation.
3. Migrate the remaining consumers (this repo first, then each plugin repo)
   off the symbols.
4. Delete them in a later core release, once no consumer references them.

Skipping straight to deletion breaks every plugin repo that has not yet
migrated, and does so at `go build` time on a transitive dependency bump —
the failure lands in repos whose owners did nothing.

## Phase 3 (stretch) — transparent mirroring by default

With Phase 1 and 2 in place, every pull already goes through one resolver.
Extend it to transparently substitute a cell-local mirror keyed by digest, so
a digest-pinned consumer (`runtime-image.json`, `DockerImage{Digest: ...}`)
never has to know or care that the origin registry changed underneath it.

This is also the only phase that may touch the upstream base images Phase 2
deliberately leaves alone (`python:3.12-slim`, `alpine:3.19`,
`ghcr.io/goreleaser/goreleaser-cross`). Mirroring one is legitimate because
the substitution is keyed by digest and the upstream reference stays the
source of truth; rewriting it to a codefly-owned name, as Phase 2 does for
codefly's own images, is not — codefly does not publish those images and
cannot stand behind their contents. An air-gapped deployment is the case that
needs this, and it needs the pull redirected, not the identity rewritten.

This is the actual end state of "codefly services don't know about the
registry": not just "one constant" (Phase 0) or "one config value" (Phase 1),
but no registry-shaped concept anywhere in core or in a plugin repo's own
source.

## Non-goals

- Do not block or delay #406, #566, or the three service PRs already
  coordinated against Phase 0 — they solve the real incident today with a
  well-understood, small change.
- This is not a general infrastructure/mirroring product (no Terraform-style
  resource graph). The scope is exactly the images codefly itself publishes
  and the images plugin repos declare.
- No decision here about *which* mirror backend Phase 1/3 should support
  first beyond what already exists (ACR, referenced in core#406); that's an
  implementation-time call for whoever picks up Phase 1.
