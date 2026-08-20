# Runbook: Cut a release

Publish an immutable, signed CLI release and update the Homebrew cask. Releases are built by
**GoReleaser** and triggered by pushing a semantic tag.

For how releases are consumed (install, `self update`, channels), see
[../cli-updates.md](../cli-updates.md). This runbook is the *producer* side.

## When to use

You want to ship a new CLI version to users (stable or beta).

## Prerequisites

- `main` (or the release branch) is green and holds exactly the commit you want to ship.
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

### 1. Pick the version

Use semver. Stable: `v1.4.0`. Prerelease/beta: `v1.4.0-beta.1` (GoReleaser marks it a
prerelease; users must opt in with `codefly self check-update --channel beta`).

### 2. Tag and push

```bash
git checkout main && git pull
git tag -a v1.4.0 -m "v1.4.0"
git push origin v1.4.0
```

Pushing a `v*.*.*` tag is the **only** trigger; there is no manual "release" button to click.
Concurrency is serialized per ref and not auto-cancelled.

### 3. Watch the workflow

```bash
gh run watch --workflow release.yaml
```

The workflow, in order: checks out the tag → sets up Go from `go.mod` → downloads Syft → loads
and verifies the signing key against `pkg/cliupdate/release-signing-cert.pem` → runs
`goreleaser release --clean` in the cross image → attests the archives/SBOMs → **verifies the
published release** (immutability, asset presence, signature) → verifies the Homebrew cask via
`.github/scripts/verify-homebrew-cask.sh`.

### 4. Verify as a consumer

```bash
gh release view v1.4.0
brew update && brew install --cask codefly-dev/cli/codefly   # or `brew upgrade --cask`
codefly version --json                                        # version/commit/buildDate match the tag
codefly self check-update                                     # sees the new stable
```

## If it fails

- **Signing-key mismatch** — the key fingerprint must equal the cert's public-key fingerprint.
  The workflow fails fast on this; fix the secret, don't re-tag.
- **Re-running a tag** — a release is immutable. To ship a fix, cut a **new** tag (e.g.
  `v1.4.1`); do not force-move a published tag.
- **Homebrew verification failed but the GitHub release exists** — the cask lives in a separate
  repo; check that repo's write token/permissions. The GitHub release can be valid while the cask
  step lags.

## Checklist

- [ ] `main` green, at the exact commit to ship
- [ ] Semantic tag chosen (prerelease suffix for beta)
- [ ] Tag pushed; `release.yaml` green end to end
- [ ] `codefly version --json` reports the tag
- [ ] `brew` install/upgrade works; `self check-update` sees it
