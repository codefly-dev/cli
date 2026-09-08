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

The roadmap below is about converging axis 2 onto axis 1 — same home
(`pkg/builder`), same shape (config in, resolved reference out) — and then
giving plugin repos a way to stop naming a registry even in their config.

## Phase 0 — now (#406, #566, service-redis#34, service-object-storage#15, service-mssql#12)

One canonical registry (`ghcr.io/codefly-dev`), one Go constant
(`resources.ImageRegistry`), public packages, anonymous pulls, CI-only
pushes, digest pins in consumers. Land this as scoped — it fixes the
immediate incident class (private-package 401s, Docker Hub sprawl) with the
smallest possible blast radius, and four other sessions are already
coordinated against this exact plan. Do not reopen or expand it for the
phases below.

Note for whoever picks up cli#566: `DockerImage.Repository` is now populated,
so anything reading `img.Name` alone gets a registry-less reference — use
`FullName()` / `String()`.

## Phase 1 — the CLI resolves the registry, not core

Move the decision from a core constant to a CLI resolver. The natural home is
`pkg/builder`, next to `registry_auth.go`, so both axes resolve and
authenticate through one package. `resources.ImageRegistry` becomes the
last-resort default of a resolution order rather than the only value:

1. Explicit workspace/cell config (a new `env.ArtifactRegistry`-shaped field
   declared in core alongside the existing `env.Registry`, read and acted on
   by the CLI — declaration in core, resolution in the CLI, per the
   constraint above).
2. `CODEFLY_ARTIFACT_REGISTRY` environment override, for CI and one-off
   overrides without touching workspace config.
3. A compiled-in default when neither is set — initially
   `resources.ImageRegistry`, which lets this phase land without a core
   release; once no CLI path reads the core constant directly, the default
   moves into the CLI and the core constant is deleted rather than
   generalized. That deletion is the actual "registry out of core" step.

Consumers then call the CLI resolver instead of `resources.PublishedImage`:
`Companion.Tag()` (`cmd/companion/companion.go:160`) and the push path
(`cmd/companion/push.go`) are the first two call sites.

This is additive to axis 1's existing auth plumbing: finish `RegistryLogin`'s
`gar` and `ghcr` cases in `pkg/builder/registry_auth.go` (today both return
"not yet wired") so authenticating against a resolved artifact registry is a
real login call, not a printed instruction, regardless of which axis is
asking.

Payoff: a self-hosted or air-gapped deployment points every companion and
runtime-image pull at its own mirror by setting one value — no core or cli
release required. This is also the natural place to plug in the per-cell
ACR mirroring that core#406 already references (images published once to
ghcr, mirrored by digest into each cell's private registry) — mirroring
becomes a resolver policy instead of a special case bolted onto individual
consumers.

## Phase 2 — plugin repos stop naming a registry at all

Once resolution is centralized in the CLI (Phase 1), audit every plugin repo
for a registry literal outside that resolver: Dockerfile `FROM` lines,
`info.codefly.yaml` / `runtime-image.json` image references, and Go
`DockerImage{}` literals. Each of these today either names `ghcr.io/...`
directly or will right after #406/#566 land. Replace them with "name + tag
(or digest)" only, resolved through the same CLI entry point a companion
uses.

`toolbox-docker` is the natural pilot: it already exists to keep the CLI from
needing local Docker knowledge directly, so it's the right place to become
the single chokepoint that turns "name + tag" into "pull/push against the
resolved registry" for every other plugin. (`runners/dockerrun` in core is
the other candidate layer, but siting the chokepoint there would push
registry knowledge back into core — so prefer the toolbox, or the
`pkg/builder` resolver, unless Phase 1 proves otherwise.) Once one real
plugin is fully off literal registries, extend the pattern rather than
redesigning it.

Exit test for this phase: `grep -rn 'ghcr.io\|codeflydev' --include=*.go
--include=Dockerfile --include=*.yaml` across every plugin repo returns
nothing outside the resolver itself and its tests.

## Phase 3 (stretch) — transparent mirroring by default

With Phase 1 and 2 in place, every pull already goes through one resolver.
Extend it to transparently substitute a cell-local mirror keyed by digest, so
a digest-pinned consumer (`runtime-image.json`, `DockerImage{Digest: ...}`)
never has to know or care that the origin registry changed underneath it.
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
