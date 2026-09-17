---
name: cut-a-release
description: Ship a new codefly CLI version to users — tag, GoReleaser build, signed archives, SBOMs, attestations, and the Homebrew cask. Use when asked to cut, publish, or ship a CLI release (stable or beta), to bump the CLI version, or when a release workflow failed and you need to know what it was supposed to produce. Not for releasing agents or core — that is release-the-fleet.
---

# Cut a CLI release

**Steps:** [docs/runbooks/cut-a-release.md](../../../docs/runbooks/cut-a-release.md).
Consumer side (install, `self update`, channels): [docs/cli-updates.md](../../../docs/cli-updates.md).

## What must not be skipped

- **`codefly publish` is the release.** It bumps the manifest, commits, tags, and pushes in the
  right order behind pre-flight gates. Do not hand-run the pieces — a gap in `publish` is a bug
  in `publish`, not a reason to tag by hand.
- **Dry-run first.** `codefly publish --dry-run` shows the version it would cut and changes
  nothing.
- **`main` must be green and hold exactly the commit you want to ship.** The tag is immutable
  once pushed.
- **Signing is not optional.** The public half of `CODEFLY_RELEASE_SIGNING_KEY` must match
  `pkg/cliupdate/release-signing-cert.pem`, or `self update` rejects the release for everyone.

## When it fails

Read the workflow logs before retrying. Re-tagging over a failed release is a history rewrite —
cut the next patch instead.
