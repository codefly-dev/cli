# Roadmap: drop the Docker registry requirement from core and plugins

Status: proposed roadmap, not yet an implementation plan for any single issue

Scope: `codefly-dev/core`, `codefly-dev/cli`, and every "plugin" repo — services
(`service-*`), toolboxes (`toolbox-*`), modules (`module-*`) — anything with an
`agent.codefly.yaml` that builds or references a Docker image.

## Where this comes from

codefly-dev/core#406 and codefly-dev/cli#566 (this repo) are landing the first,
narrowly-scoped fix for today's actual pain: five-plus repos each hardcode a
registry literal for the six companion images (`codeflydev/<name>` on Docker
Hub, moving to `ghcr.io/codefly-dev/<name>`), and that sprawl is what let
`service-redis`'s package go accidentally private and 401 in production. That
fix is: one Go constant, `resources.ImageRegistry`, that every consumer
imports instead of spelling out a registry string.

A constant in a shared package is a real improvement — one place to change
instead of five — but it is still a compile-time literal. Changing the
registry (a new mirror, a self-hosted instance, an air-gapped deployment)
still means a new core release, a new cli release, and every plugin repo
re-pinning both. The question this roadmap answers: what would it take for no
codefly repo — core, cli, or any plugin — to hardcode a registry hostname at
all, the same way `toolbox-docker` already means the CLI doesn't need to know
Docker's own mechanics directly?

## Two registry axes that already exist, separately

codefly already has two independent places where "which registry" is a
decision, and they're solved to different degrees:

1. **The workspace/environment registry** — where a *user's own* service
   images land when they run `codefly build` / `codefly deploy`. This is
   already data, not code: `env.Registry.URL` + `env.Registry.Auth` in the
   workspace config, resolved at build time by
   `pkg/builder/registry_auth.go`. Auth is pluggable per backend (`ecr`,
   `acr` fully implemented; `gar` and `ghcr` are still stubs that print a
   manual `docker login` command instead of running one).
2. **The codefly-owned artifact registry** — where *codefly's own* shared
   images live: the six companions, and each service/toolbox repo's runtime
   image. This is the one #406/#566 are consolidating from scattered literals
   into `resources.ImageRegistry`. Unlike axis 1, it has no config surface at
   all today — it is, and after #406/#566 remains, a fixed value baked into
   the binary.

The roadmap below is about bringing axis 2 up to where axis 1 already is
(config, not code), and then giving plugin repos a way to stop naming a
registry even in their config.

## Phase 0 — now (#406, #566, service-redis#34, service-object-storage#15, service-mssql#12)

One canonical registry (`ghcr.io/codefly-dev`), one Go constant
(`resources.ImageRegistry`), public packages, anonymous pulls, CI-only
pushes, digest pins in consumers. Land this as scoped — it fixes the
immediate incident class (private-package 401s, Docker Hub sprawl) with the
smallest possible blast radius, and four other sessions are already
coordinated against this exact plan. Do not reopen or expand it for the
phases below.

## Phase 1 — make the constant a resolved default, not a literal

Generalize `resources.PublishedImage(name, tag)` so `ImageRegistry` becomes
the *default* of a resolution order instead of the only value:

1. Explicit workspace/cell config (a new `env.ArtifactRegistry`-shaped field,
   parallel to the existing `env.Registry` for user builds).
2. `CODEFLY_ARTIFACT_REGISTRY` environment override, for CI and one-off
   overrides without touching workspace config.
3. `resources.ImageRegistry` (`ghcr.io/codefly-dev`) as the compiled-in
   default when neither is set.

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

Once resolution is centralized in core (Phase 1), audit every plugin repo
for a registry literal outside that resolver: Dockerfile `FROM` lines,
`info.codefly.yaml` / `runtime-image.json` image references, and Go
`DockerImage{}` literals. Each of these today either names `ghcr.io/...`
directly or will right after #406/#566 land. Replace them with "name + tag
(or digest)" only, resolved through the same core entry point a companion
uses.

`toolbox-docker` is the natural pilot: it already exists to keep the CLI from
needing local Docker knowledge directly, so it's the right place (or
`runners/dockerrun` in core, if the abstraction belongs at that layer
instead) to become the single chokepoint that turns "name + tag" into
"pull/push against the resolved registry" for every other plugin. Once one
real plugin is fully off literal registries, extend the pattern rather than
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
but no registry-shaped concept anywhere in a plugin repo's own source.

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
