# Supported CLI / core / agent combinations

This page is the human-readable view of
[`pkg/conformance/matrix.json`](../pkg/conformance/matrix.json), the
machine-readable support matrix. The two cannot drift: `pkg/conformance`
fails if a row here disagrees with the matrix, names a CI job that does not
exist, or claims a status the repository cannot back.

Every row describes the CLI **built from this checkout** against the
`github.com/codefly-dev/core` release line pinned in `go.mod` — the matrix
records the line (`v0.3.25`), not the pseudo-version, so a core bump within a
line does not invalidate the claim. Version skew between a
released CLI and a released agent is not, by itself, a compatibility failure —
a row is about whether a real lifecycle was driven end to end.

## What the statuses mean

| Status | Meaning |
|---|---|
| `qualified` | A CI job drives this row on every change and lists it in `CODEFLY_CONFORMANCE_REQUIRED`, so its tests cannot skip themselves when the backend is missing. Every qualified row must declare a gate call site — without one nothing could hold it to the claim. |
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

`conformancetest.Gate(t, "<row>")` replaces the ad-hoc `t.Skip` that used to
guard each of these tests:

- The row is listed in `CODEFLY_CONFORMANCE_REQUIRED` → the test **runs**. A
  missing prerequisite, or a runner whose OS/architecture is not the row's,
  **fails**. It never skips.
- The row's own opt-in is set (`CODEFLY_GITOPS_K3D_QUALIFY=1`, …) → the test
  runs, and a missing prerequisite fails: you asked for that backend.
- Neither → the test skips, naming the row and how to enable it.

A row with no gate call site cannot be required at all (`Row.Requirable`), and
`Validate` refuses to call such a row qualified — so a job cannot claim a row
nothing enforces, and a row cannot claim support nothing can check.

### Prerequisites are a union; a caller may narrow

A row's `prerequisites` are the union over its tests, and no single test needs
all of them: only the solution-SDK fixture runs `buf`, and only the two ArgoCD
shapes need `kubectl` and `ssh-keygen`. A call site names the subset it uses —
`conformancetest.Gate(t, "linux-amd64-k3d-deploy", "docker", "k3d", "git")` —
and outside a CI claim only that subset must be present, so a partial local
toolchain still qualifies the tests it can run. A **claimed** row is always
held to every prerequisite it declares: narrowing is a local convenience, never
a way to call a row qualified on a runner missing a tool. Naming a prerequisite
the row does not declare is an error, so the two cannot drift apart.

### Receipts

When `CODEFLY_CONFORMANCE_RECEIPTS` names a directory, each admitted test drops
`<row>.<package>.<test>.json` there with the row, its status, the calling
package, the resolved prerequisite paths, the core pin, the phase duration and
the outcome. The package is part of the name because every package gating one
row writes into the same directory and two packages may hold a same-named test.
CI uploads the directory as an artifact and runs
[`scripts/verify-conformance-receipts.sh`](../scripts/verify-conformance-receipts.sh)
in an always-run step, which fails when a required row produced no receipt, or
produced one recording a failed run. That is what closes the last silent-pass
hole: a `go test -run` filter that matches nothing exits 0, but it leaves no
receipt — and checking the recorded outcome means the claim does not rest on
the test step's exit code.

## Qualifying a new row

1. Add the row to `pkg/conformance/matrix.json` as `not-yet-qualified`, with
   its blockers, prerequisites and gate packages.
2. Call `conformancetest.Gate(t, "<row>", …)` at the top of every test that is
   evidence for it, replacing any bespoke skip, naming the prerequisites that
   test actually uses.
3. Add the row to this table.
4. When a CI job provisions the backend, add the job to the row's `ci` list,
   name the row in that gate's `conformance_rows` in `go.yml`, drop the
   blockers, and flip the status to `qualified`. The tests in `pkg/conformance`
   fail if any of those four are missing — the `ci` entry is resolved against
   the workflow's real jobs and matrix gates, not matched as a substring.
