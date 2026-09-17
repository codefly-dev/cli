---
name: bump-go-version
description: Update the Go toolchain across core, wool, cli, and every agent repo in lockstep, including the spots that do not follow go.mod (GoReleaser cross image, Dockerfiles, pinned CI setup-go steps). Use when adopting a new Go release, when editing the go or toolchain line in go.mod, or when a build fails with "compile - version go1.X does not match go tool version" or another GOROOT/toolchain mismatch.
---

# Bump the Go version

**Steps, and the full list of manually pinned spots:**
[docs/runbooks/bump-go-version.md](../../../docs/runbooks/bump-go-version.md).

## What must not be skipped

- **`go.mod` is the source of truth**, and it has *two* lines — `go` (language version) and
  `toolchain`. Set both.
- **Lockstep across repos.** `core`, `wool`, `cli`, and every agent are linked through `go.work`
  and `replace` directives; a skew between any two produces the toolchain-mismatch error. Bumping
  only the repo you are in reproduces the bug you are fixing.
- **A few places pin Go independently of `go.mod`** and will not follow it. The runbook lists
  them; grep for the old version before you call it done, since a missed pin fails only in the
  release job.

## Verifying

`go build ./...` passing locally proves nothing about the pins that only CI reads. Say so in the
PR if you could not exercise the release path.
