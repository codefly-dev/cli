# Runbook: Cut a release

Publish an immutable, signed CLI release and update the Homebrew cask. Releases are built by
**GoReleaser** and triggered by pushing a semantic tag.

For how releases are consumed (install, `self update`, channels), see
[../cli-updates.md](../cli-updates.md). This runbook is the *producer* side.

## When to use

You want to ship a new CLI version to users (stable or beta).

## Prerequisites

- `main` is green and holds exactly the commit you want to ship.
- A `codefly` binary new enough to have `publish` (it bumps the manifest for you).
- You can push tags to `codefly-dev/cli`.
- Release secrets are configured in the repo: `CODEFLY_RELEASE_SIGNING_KEY` (its public half must
  match `pkg/cliupdate/release-signing-cert.pem`).

## What a release produces

`.github/workflows/release.yaml` runs GoReleaser inside
`ghcr.io/goreleaser/goreleaser-cross` (CGO on — tree-sitter needs the platform compilers) and
publishes, per tag:

- 4 archives: `codefly_<version>_{darwin,linux}_{amd64,arm64}.tar.gz`
- `checksums.txt` + `checksums.txt.sig` (release-key signature)
- one Syft SBOM per archive
- GitHub artifact attestations
- a Homebrew **cask** in the separate Homebrew tap repo

The version, commit, and UTC build date are injected via ldflags into `pkg/cliupdate` and
reported by `codefly version --json`. macOS binaries are Developer ID signed and notarized.

## Steps

### 1. Publish

`codefly publish` is the release. It bumps the manifest, commits, tags, and pushes — in the
right order, behind pre-flight gates — so there is no step to forget:

```bash
codefly publish --dry-run   # show the version it would cut; changes nothing
codefly publish             # patch bump; or: codefly publish minor|major
codefly publish beta        # next beta: 1.4.0 → 1.4.1-beta.1, then beta.2
```

It auto-detects this repo from `pkg/cli/info.yaml` and refuses to do anything unless the
working tree is clean, you are on `main`, it exactly matches `origin/main`, and the target tag
does not already exist. It atomically pushes the release commit and tag: a failure before that
push restores the manifest and removes the local release commit/tag; after a successful push,
the immutable release commit and tag remain even if the release workflow later fails. Neither
`main` nor the tag is ever force-pushed.

It also reconciles the manifest against the tags actually on origin: if the manifest has
drifted behind (as it had at `0.1.145` while tags ran to `v0.1.150`), it bumps from the
latest tag rather than colliding with an existing one.

**Do not hand-roll `git tag`.** The workflow's first gate requires the tag to equal `v` +
`pkg/cli/info.yaml`'s version, and a hand-cut tag skips the bump: the gate rejects it about a
second in, nothing is published, and the version number is burned. Tags `v0.1.146` through
`v0.1.150` were all lost that way.

### 2. Watch the workflow

```bash
gh run watch --workflow release.yaml
```

The workflow, in order: checks out the tag → sets up Go from `go.mod` → downloads Syft →
**qualifies the clean tag** (tag == `v` + `pkg/cli/info.yaml`, tag points at this commit, tree
clean, snapshot build produces all 4 archives) → loads and verifies the signing key against
`pkg/cliupdate/release-signing-cert.pem` → runs `goreleaser release --clean` in the cross image
→ attests the archives/SBOMs → **verifies the published release** (immutability, asset
presence, signature) → verifies the Homebrew cask via `.github/scripts/verify-homebrew-cask.sh`.

Pushing a `v*.*.*` tag is the **only** trigger; there is no manual "release" button to click.
Concurrency is serialized per ref and not auto-cancelled.

### 3. Verify as a consumer

```bash
gh release view v1.4.0
brew update && brew install --cask codefly-dev/cli/codefly   # or `brew upgrade --cask`
codefly version --json                                        # version/commit/buildDate match the tag
codefly self check-update                                     # sees the new stable
```

## If it fails

- **`Qualify the clean tag` fails in seconds** — the tag does not match `pkg/cli/info.yaml`,
  i.e. the tag was cut by hand instead of by `codefly publish`. Nothing was published, so
  there is no release to supersede; run `codefly publish` and let it cut the next tag.
- **Signing-key mismatch** — the key fingerprint must equal the cert's public-key fingerprint.
  The workflow fails fast on this; fix the secret, don't re-tag.
- **Re-running a tag** — a release is immutable. To ship a fix, cut a **new** tag (e.g.
  `v1.4.1`); do not force-move a published tag.
- **Homebrew verification failed but the GitHub release exists** — the cask lives in a separate
  repo; check that repo's write token/permissions. The GitHub release can be valid while the cask
  step lags.

## Checklist

- [ ] `main` green, at the exact commit to ship
- [ ] `codefly publish --dry-run` shows the version you expect
- [ ] `codefly publish` ran clean (manifest bumped, `release:` commit and tag pushed)
- [ ] `release.yaml` green end to end
- [ ] `codefly version --json` reports the tag
- [ ] `brew` install/upgrade works; `self check-update` sees it
