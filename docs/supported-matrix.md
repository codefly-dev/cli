# Supported CLI / core / agent combinations

This page is the human-readable view of
[`pkg/conformance/matrix.json`](../pkg/conformance/matrix.json), the
machine-readable support matrix. The two cannot drift: `pkg/conformance`
fails if a row here disagrees with the matrix, names a CI job that does not
exist, or claims a status the repository cannot back.

Every row describes the CLI **built from this checkout** against
`github.com/codefly-dev/core` as pinned in `go.mod`. Version skew between a
released CLI and a released agent is not, by itself, a compatibility failure —
a row is about whether a real lifecycle was driven end to end.

## What the statuses mean

| Status | Meaning |
|---|---|
| `qualified` | A CI job drives this row on every change. If the row has a gate call site, that job also lists it in `CODEFLY_CONFORMANCE_REQUIRED`, so its tests cannot skip themselves when the backend is missing. |
| `not-yet-qualified` | Real tests and a real gate exist, but nothing proves the row outside a developer machine. **This is not a support claim.** Each row lists what blocks it. |
| `unsupported` | Not shipped and not tested. |

## Rows

| Row | Status | Platform | Backend | Prerequisites | Evidence |
|---|---|---|---|---|---|
| `linux-amd64-source` | qualified | linux/amd64 | none | — | `go.yml` — `coverage`, `race`, `lint` |
| `linux-amd64-native-npm` | qualified | linux/amd64 | native | `npm` | `go.yml` — `coverage`, `race` |
| `linux-amd64-nix-run` | qualified | linux/amd64 | nix | `nix` | `go.yml` — `control-integration` |
| `linux-amd64-docker-generate` | not-yet-qualified | linux/amd64 | docker | `docker`, `buf` | `CODEFLY_GENERATE_QUALIFY=1`, developer machine only |
| `linux-amd64-docker-mcp-run` | not-yet-qualified | linux/amd64 | docker | `docker` | `CODEFLY_MCP_RUN_QUALIFY=1`, developer machine only |
| `linux-amd64-network-mcp-agent` | not-yet-qualified | linux/amd64 | native | — | `CODEFLY_MCP_AGENT_QUALIFY=1`, developer machine only |
| `linux-amd64-k3d-deploy` | not-yet-qualified | linux/amd64 | k3d | `docker`, `k3d`, `kubectl`, `ssh-keygen`, `git` | `CODEFLY_GITOPS_K3D_QUALIFY=1`, developer machine only |
| `darwin-arm64-native-user-service` | not-yet-qualified | darwin/arm64 | native | `launchctl` | `CODEFLY_SERVICE_INTEGRATION=1`, developer machine only |
| `windows-amd64-native-run` | unsupported | windows/amd64 | native | — | none |

### Qualification gaps

The matrix carries the blocker for every `not-yet-qualified` row. The two that
matter most for a support claim:

- **No deployment row is qualified.** `linux-amd64-k3d-deploy` is the only row
  that reaches a cluster, and no CI job provisions k3d. It also creates its
  cluster at k3d's default k3s image; a qualified cluster row must pin an
  explicit Kubernetes version, which `Validate` enforces.
- **No macOS row is qualified.** CI has no macOS runner, so every released
  darwin binary ships without lifecycle evidence.

## How the gate works

`conformance.Gate(t, "<row>")` replaces the ad-hoc `t.Skip` that used to guard
each of these tests:

- The row is listed in `CODEFLY_CONFORMANCE_REQUIRED` → the test **runs**. A
  missing prerequisite, or a runner whose OS/architecture is not the row's,
  **fails**. It never skips.
- The row's own opt-in is set (`CODEFLY_GITOPS_K3D_QUALIFY=1`, …) → the test
  runs, and a missing prerequisite fails: you asked for that backend.
- Neither → the test skips, naming the row and how to enable it.

A row with no gate call site cannot be required at all
(`Row.Requirable`), so a job cannot claim a row nothing enforces.

### Receipts

When `CODEFLY_CONFORMANCE_RECEIPTS` names a directory, each admitted test drops
`<row>.<test>.json` there with the row, its status, the resolved prerequisite
paths, the core pin, and the phase duration. CI uploads the directory as an
artifact and runs
[`scripts/verify-conformance-receipts.sh`](../scripts/verify-conformance-receipts.sh)
in an always-run step, which fails when a required row produced no receipt.
That is what closes the last silent-pass hole: a `go test -run` filter that
matches nothing exits 0, but it leaves no receipt.

## Qualifying a new row

1. Add the row to `pkg/conformance/matrix.json` as `not-yet-qualified`, with
   its blockers, prerequisites and gate packages.
2. Call `conformance.Gate(t, "<row>")` at the top of every test that is
   evidence for it, replacing any bespoke skip.
3. Add the row to this table.
4. When a CI job provisions the backend, add the job to the row's `ci` list,
   set `CODEFLY_CONFORMANCE_REQUIRED` on its test step, drop the blockers, and
   flip the status to `qualified`. The tests in `pkg/conformance` fail if any
   of those four are missing.
