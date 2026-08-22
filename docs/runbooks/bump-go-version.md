# Runbook: Bump the Go version

Update the Go toolchain across the whole codefly workspace — `core`, `wool`, `cli`, and every
agent repo — in lockstep. Because these repos are linked through `go.work` + `replace`
directives, a version skew between them produces
`compile: version go1.26.x does not match go tool version ...` (a GOROOT/toolchain mismatch).
Bump them together.

## When to use

- A new Go minor/patch release you want to adopt.
- CI or a local build fails with a toolchain-mismatch error.

## Source of truth

`go.mod` is authoritative. It has **two** relevant lines:

```
go 1.26.3          # language version — the minimum the module compiles against
toolchain go1.26.6 # the toolchain the go command downloads/uses
```

Most CI reads the version *from* `go.mod` (`go-version-file: go.mod`), so those jobs follow
automatically. A few places pin a Go version **independently** and must be edited by hand — they
are listed below.

## Steps

### 1. Bump `go.mod` in every repo (lockstep)

For **each** repo in the workspace — `core`, `wool`, `cli`, and every agent repo — set the same
`go` and `toolchain` lines. From each repo root:

```bash
go mod edit -go=1.26.3 -toolchain=go1.26.6
go mod tidy
```

> Keep the `go` (language) version identical across repos. The `toolchain` line may be equal or
> newer than `go`, but keep it identical across repos too to avoid surprises.

### 2. Bump the manually-pinned spots in `cli` (these do NOT follow `go.mod`)

- **`.github/workflows/release.yaml`** — the GoReleaser cross-compile image:
  ```
  ghcr.io/goreleaser/goreleaser-cross:v1.26.4
  ```
  Bump the tag to the matching Go minor (`v1.26.x`). Release builds run **inside this image**, so
  it — not `go.mod` — determines the compiler that ships. CGO is required (tree-sitter), so this
  image cannot be swapped for plain `setup-go`.

- **`test/Dockerfile`** — `FROM golang:alpine`. Currently unpinned (floats to latest). If you
  pin it, pin to the new minor (`golang:1.26-alpine`).

- **`Makefile` / `.github/workflows/go.yml`** — the `golangci-lint` version (`GOLANGCI_LINT_VERSION`)
  is pinned and must support the new Go minor. Bump it if the new Go release needs a newer linter.

  **Leading-edge Go minors:** published golangci-lint *binaries* lag new Go releases by weeks — a
  binary built with the previous Go minor both refuses a module that targets the newer `go`
  directive ("used to build golangci-lint is lower than the targeted Go version") and cannot parse
  the newer stdlib. The CI lint job therefore builds golangci-lint **from source** with the
  module's Go (`install-mode: goinstall` in `go.yml`; `make lint` already does this via `go
  install`), so its type checker matches the toolchain. Revert to the default prebuilt-binary mode
  once a golangci-lint release compiled with the new Go minor is available.

### 3. Grep to catch anything new

Pinned versions creep in over time. Before finishing, sweep each repo:

```bash
# hard-coded Go versions in workflows, Dockerfiles, scripts
grep -rnE 'golang:1\.[0-9]+|go1\.[0-9]+|go-version:\s*1\.[0-9]+|goreleaser-cross:v1\.[0-9]+' \
  --include='*.yaml' --include='*.yml' --include='Dockerfile*' --include='*.sh' . \
  | grep -v vendor
```

`docs/design/*.md` contain `golang:1.22` in illustrative Dockerfiles — those are examples, not
build inputs; update only if you want the docs current.

### 4. Update the prose

- `AGENTS.md` — the "Language: Go 1.26" line.
- `docs/development.md` — the "Go 1.2x+" prerequisite.

### 5. Verify

```bash
go version                       # local toolchain
go build ./... && go test ./...  # in each repo
```

If you hit `version go1.26.x does not match go tool version`, your local `go` shim (e.g. mise/asdf)
disagrees with `GOROOT`. Invoke the explicit toolchain binary
(`$(go env GOROOT)/bin/go`) or align the shim; see the `go-toolchain-goroot-mismatch` note.

Push a branch and let CI (`go.yml`, `companions.yaml`) confirm the matrix builds green before
merging. Merge the companion repos' bumps first (or together) so `cli` never points at an
older-toolchain `core`/`wool`.

## Checklist

- [ ] `go.mod` (`go` + `toolchain`) bumped in **core, wool, cli, every agent repo**
- [ ] `go mod tidy` run in each
- [ ] `goreleaser-cross` image tag bumped in `release.yaml`
- [ ] `test/Dockerfile` reviewed (pin if applicable)
- [ ] `golangci-lint` version compatible
- [ ] grep sweep clean
- [ ] prose updated (`AGENTS.md`, `docs/development.md`)
- [ ] `go build`/`go test` green locally and in CI
