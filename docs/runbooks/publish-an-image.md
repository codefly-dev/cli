# Runbook: Publish an image to the registry

Checklist for any repo that ships a container image (a companion, a service
runtime image, a toolbox). `ghcr.io/codefly-dev` is the single canonical
registry for everything codefly publishes — packages are public, pulled
anonymously, pushed from release workflows with a token, and pinned by
digest downstream.

## When to use

- You're adding a new image-shipping repo or package.
- You're debugging why a pinned image reference 401s or 404s for someone who
  isn't logged in locally.

## Checklist

1. **Push from CI with a token that has `packages: write`, never from a
   laptop.** In a GitHub Actions workflow that's the job's own
   `GITHUB_TOKEN` with `permissions: packages: write`; `codefly companion
   publish` (or the equivalent push step in a service repo) is the manual
   fallback only, for a one-off republish or a broken pipeline.

2. **On the first push of a new package**, two manual, one-time steps in the
   GitHub UI (there is no API/CLI shortcut for either):
   - Set the package's visibility to **public**.
   - Link the package to its source repo (org packages settings → the
     package → "Package settings" → "Connect repository"). An unlinked
     package doesn't get repo-scoped permissions, so its own CI can't push
     to it, and it doesn't show up in the repo's UI.

   Skipping this is what bit `service-redis` with 401s: the package existed
   but was private and unlinked, so an anonymous pull (and CI's own push)
   both failed.

3. **Pin the image by digest in every consumer, never by tag.** A tag is
   mutable — the same tag can point at a different image tomorrow. Pin
   `runtime-image.json` (or the Go `resources.DockerImage{Digest: ...}`
   value) to the digest, not the tag, so a consumer's build is reproducible
   regardless of what the tag currently resolves to.

4. **Add a CI guard in the producing repo** that checks two things on every
   change: the build reproduces the pinned digest, and the pinned reference
   is anonymously pullable. `service-redis/.github/workflows/ci.yml`'s
   "Verify runtime image digest is reproducible" and "Verify runtime image
   is publicly pullable" steps are the reference implementation — copy
   them. The anonymous-pull check matters even when the digest reproduces
   cleanly: a digest that reproduces but was never pushed to the registry
   (or was pushed into a still-private package) passes the reproducibility
   check and then 401s for every downstream consumer, and nothing in a
   same-repo CI run would otherwise catch that gap. Pattern:

   ```bash
   ref=$(jq -er '.name + "@" + .digest' runtime-image.json)
   mkdir -p "$DOCKER_CONFIG"   # point at an empty dir first
   echo '{}' > "$DOCKER_CONFIG/config.json"
   docker manifest inspect "$ref"   # must succeed with zero stored credentials
   ```

   `codefly companion verify` runs the same anonymous check for companion
   images; wire the equivalent into a service repo's own CI rather than
   relying on a human to remember to run it.

5. **Attest provenance.** Use `actions/attest-build-provenance` with
   `push-to-registry: true` in the workflow that pushes the image, so the
   published digest carries a verifiable attestation of the build that
   produced it.

## Deprecation note

The `codeflydev` Docker Hub org is retired now that companions and this CLI
publish and consume `ghcr.io/codefly-dev` exclusively (codefly-dev/core#406,
codefly-dev/cli#566). Any repo still pinning a `codeflydev/<name>:<tag>`
reference hasn't migrated yet — point it at the `ghcr.io/codefly-dev`
equivalent.
