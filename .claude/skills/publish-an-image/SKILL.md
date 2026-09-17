---
name: publish-an-image
description: Publish a container image (companion, service runtime image, toolbox) to ghcr.io/codefly-dev and wire the guards that keep it pullable — package visibility, repo linking, digest pinning, anonymous-pull CI checks, provenance attestation. Use when adding an image-shipping repo or package, or when debugging why a pinned image reference 401s or 404s for someone not logged in locally, or why a companion or runtime image fails to pull in CI.
---

# Publish an image to the registry

**Full checklist:** [docs/runbooks/publish-an-image.md](../../../docs/runbooks/publish-an-image.md).

`ghcr.io/codefly-dev` is the single canonical registry. The `codeflydev` Docker Hub org is
retired — a repo still pinning `codeflydev/<name>:<tag>` has not migrated.

## What must not be skipped

- **Push from CI with `permissions: packages: write`, never from a laptop.** The manual path is
  the fallback for a one-off republish or a broken pipeline only.
- **A new package needs two one-time manual steps in the GitHub UI** (no API or CLI shortcut):
  set visibility to **public**, and **link it to its source repo**. An unlinked package gets no
  repo-scoped permissions, so its own CI cannot push to it. Skipping this is what produced 401s
  on a package that existed but was private and unlinked.
- **Pin by digest in every consumer, never by tag.** A tag is mutable; the same tag can point
  somewhere else tomorrow.
- **Guard both properties in the producing repo's CI**: the build reproduces the pinned digest,
  *and* the pinned reference is anonymously pullable. They are separate failures — a digest that
  reproduces but was never pushed (or landed in a private package) passes the first check and
  then 401s for every consumer. Use an empty `DOCKER_CONFIG` so the check runs with zero stored
  credentials.
- **Attest provenance** with `actions/attest-build-provenance` and `push-to-registry: true`.

`codefly companion verify` runs the anonymous check for companion images — wire the equivalent
into a service repo's CI rather than relying on someone remembering to run it.
