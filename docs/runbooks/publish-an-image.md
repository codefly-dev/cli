# Runbook: Publish an image to the registry

Checklist for any repo that ships a container image (an agent — service,
toolbox, module, provider — or a companion). `ghcr.io/codefly-dev` is the
single canonical registry for everything codefly publishes, but **no repo
spells that out**: `resources.ImageRegistry` in core is the one place it is
written down, and the CLI derives every reference from it. A registry move
is a change there, not in every repo's CI.

## When to use

- You're adding a runtime image to an agent repo, or a new image-shipping
  repo.
- You're debugging why a pinned image reference 401s or 404s for someone who
  isn't logged in locally.

## Checklist for an agent repo

1. **Check a `Dockerfile` into the repo root.** That is the entire opt-in.
   The build context is the repo root; the version is the one already in
   `agent.codefly.yaml`. A repo with no root `Dockerfile` publishes no image
   and needs nothing else here.

2. **Run `codefly publish`.** For any agent kind, when a root `Dockerfile` is
   present, publish:
   - builds it for `linux/amd64` and `linux/arm64` **before** it commits,
     tags, or pushes anything — a Dockerfile that doesn't build aborts the
     release with the working tree untouched;
   - tags it `<resources.ImageRegistry>/<kind>-<name>:<version>` (e.g.
     `ghcr.io/codefly-dev/service-redis:0.0.86`) via
     `resources.PublishedImage`;
   - pushes the multi-platform manifest once the Git tag is live;
   - re-inspects the pushed reference with an **empty `DOCKER_CONFIG`** and
     fails the publish if it isn't anonymously pullable.

   Publish refuses up front, during its pre-flight gates, if the host has no
   working `docker buildx` — so `codefly publish all` rejects an incapable
   host before any repo ships.

3. **Do not put any of this in the agent repo's CI.** No
   `docker/login-action`, no `buildx --push`, no registry hostname literal,
   no hand-computed digest. CI should keep doing what only CI can do —
   build the image locally, smoke-test it, scan it — and leave
   build/tag/push/verify against the canonical registry to `codefly
   publish`.

4. **On the first publish of a new package**, two manual, one-time steps in
   the GitHub UI (there is no API/CLI shortcut for either):
   - Set the package's visibility to **public**.
   - Link the package to its source repo (org packages settings → the
     package → "Package settings" → "Connect repository"). An unlinked
     package doesn't get repo-scoped permissions and doesn't show up in the
     repo's UI.

   Skipping this is what bit `service-redis` with 401s: the package existed
   but was private and unlinked. Publish's anonymous check now fails loudly
   in that state instead of reporting a successful push, and prints the
   settings URL to fix it.

5. **Push credentials are the operator's.** `codefly publish` pushes with
   whatever the local daemon is logged into; `docker login ghcr.io -u <user>
   -p $(gh auth token)` is what a `denied` push is telling you to run. The
   anonymous re-check afterwards is precisely what stops those credentials
   from masking a package nobody else can pull.

## Companions

Companions live in `core/companions/<name>/` and pin their version in
`info.codefly.yaml` rather than `agent.codefly.yaml`, so they have their own
commands with the same guarantees:

```bash
codefly companion publish <name>   # build + push at the pinned version
codefly companion verify           # anonymous check over every pinned tag
```

`companion verify` is the inverse of `companion publish`: the set it checks
is the set publish produces. Wire it into CI so a version bump that
references an unpublished tag fails fast instead of at the companion pull.

## Consuming a published image

**Pin by digest, never by tag.** A tag is mutable — the same tag can point
at a different image tomorrow. Pin `runtime-image.json` (or the Go
`resources.DockerImage{Digest: ...}` value) to the digest, so a consumer's
build is reproducible regardless of what the tag currently resolves to.

A consumer repo can guard its own pin with the same credential-free check
publish runs:

```bash
ref=$(jq -er '.name + "@" + .digest' runtime-image.json)
mkdir -p "$DOCKER_CONFIG"   # point at an empty dir first
echo '{}' > "$DOCKER_CONFIG/config.json"
docker manifest inspect "$ref"   # must succeed with zero stored credentials
```

A digest that reproduces locally but was never pushed (or was pushed into a
still-private package) passes a reproducibility check and then 401s for
every downstream consumer — the anonymous check is what catches that gap.

## Known gaps

- **Images built from a Dockerfile that isn't at the repo root** are not
  covered: the opt-in is the root `Dockerfile` only. `service-mssql`'s
  `mssql-alembic` image (`migrations/Dockerfile.alembic`) is the one such
  case today.
- **Provenance attestation** (`actions/attest-build-provenance` with
  `push-to-registry: true`) still has to run in CI against the pushed
  digest; publish does not attest.
