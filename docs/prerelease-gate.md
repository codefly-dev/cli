# The prerelease gate

**A prerelease version must never reach a repository's default branch, so a tag cut from it can
never contain one.** `codefly ci prerelease` is the check that enforces it, and this page is
why it exists, what it refuses, and how each repository wires it in.

The rule was a convention before it was a gate. It was written down in a composition's release
document and violated anyway — not through carelessness, but because the person releasing a
module has no reason to read a downstream consumer's release document.

Nothing on this page names a repository that uses the CLI, and nothing in the implementation
does either. The gate is told what to judge by the repository in front of it.

## What it cost, measured

In one release round, six repositories were released and five module tags cut. Two of those tags
turned out to contain prerelease agent pins — five service pins across three agents:

| agent | pinned at | released as |
|---|---|---|
| `codefly.dev/go-grpc` | `0.1.48-dev.e87db5e08865` | v0.1.50 |
| `codefly.dev/nextjs` | `0.0.159-dev.1ed5001fd8b4` | v0.0.162 |
| `codefly.dev/postgres` | `0.0.139-dev.9785d12f3700` | v0.0.141 |

Every one of those agents had already been released and verified, so the pins were stale as well
as unreleased.

The consequence was not local. A composition downstream of those modules requires dropping every
`agent-overrides` DEV PIN before it tags, on the stated basis that *"a module released after the
agent fix pins the released agent itself"*. Because those modules did not, the composition could
not drop its overrides, and the fix was two more module release rounds. Each pin cost one line at
the pull request that wrote it.

Every one of those pins carried a comment saying to move it before tagging. A comment is not a
gate.

## What it refuses

Detection is the **shape** of the version, never the literal `dev`:

- a semver prerelease component — `0.1.48-dev.e87db5e08865`, `0.2.0-rc.1`, `1.0.0-alpha`;
- a Go pseudo-version — `v0.0.0-20260930123456-abcdef123456`,
  `v0.5.11-0.20260927230309-0c1b5db823d5` — which is the same defect spelled differently.

Anything that is not an exact semantic version names no build and is left alone: `latest` is the
agent sentinel, `^0.0.1` and `~1.2` are the range constraints a library dependency carries.
Semver build metadata (`1.2.3+build.5`) is not a prerelease either.

### Where it looks

Only in files **git tracks**. That is the point rather than an optimisation: the rule is about
what reaches the default branch, and what reaches it is what git tracks. It is also why the local
dev loop is free — `codefly.local.yaml` is gitignored, so it is outside the gate by construction
and needs no exception written into it.

| source | what is read | refused? |
|---|---|---|
| every `*.codefly.yaml` | every `version:` key, at any depth — a service's `agent.version`, a workspace's `modules[].version` and `solutions[].version`, a module's or library's own version | yes |
| `workspace.codefly.yaml` | the top-level `agent-overrides` block, whose values are versions under an agent identity | only unlabelled, or under `--release` |
| `go.mod` | requires under an owner this repository itself publishes under | reported; refused with `--go-modules` |

The YAML scan walks for the `version:` **key** rather than reading a known schema, so a new
versioned field is covered the day it is added instead of the day somebody notices the gate
missed it.

### Which owners count as first-party

Derived from the repository, never carried by the CLI: the owner prefix of every `go.mod`'s own
`module` path.

```text
module github.com/acme/widgets/services/api/code    ->    github.com/acme/
```

A run prints the owners it derived, and `--format json` reports them as `first_party_owners`. A
repository whose siblings are published under an owner it does not itself publish under names
them with `--first-party`.

This replaced a hardcoded list of two GitHub organisations that shipped in v0.1.172. That was
wrong in kind and not only in content: a generic tool cannot name the products that happen to use
it, such a list is stale the moment somebody adds an organisation, and every repository that was
not one of those two silently got a narrower check than the ones the gate was written against.

`testdata/` trees are skipped — a fixture's job can be to carry a bad pin, and this repository
tracks 138 `*.codefly.yaml` and `go.mod` files under `testdata` against one real `go.mod`.
`--include-testdata` reads them.

## The one exception, and the two scopes

`agent-overrides` is the sanctioned prerelease carrier. `codefly publish dev` publishes an agent
build for iteration and `codefly update workspace --agent-override <publisher>/<name>=<version>`
writes it into the workspace; a composition's release document describes that loop and requires
the override be gone before it tags. A blanket "no `-` anywhere" would break it.

So the gate has two scopes:

| scope | command | `agent-overrides` | everything else |
|---|---|---|---|
| default branch | `codefly ci prerelease` | a prerelease is allowed **with a label** — a comment naming the issue it stands in for | refused |
| release | `codefly ci prerelease --release` | refused, however well labelled | refused |

The release scope is what guarantees the property that actually broke. It is the enforcement of
the "drop every dev override" step, which a release document can ask for but cannot enforce.

A label is a comment on the entry carrying an issue reference (`codefly-dev/service-go#117`, or
a bare `#117`). The condition for removal is prose no gate can check; the issue reference is the
half that can be, and the half that tells the next reader where to look. The convention already
writes both:

```yaml
agent-overrides:
    # DEV PIN, labelled: codefly-dev/service-go#117 — the `go` agent ran
    # `go mod download` inside the container, where no credential exists for a
    # private module. Removed before anything here is tagged (docs/release.md).
    codefly.dev/go: 0.0.63-dev.bd71dd90cc10
```

### Why not push the dev loop into `codefly.local.yaml` instead

That was the stricter alternative, and it is the wrong trade for two reasons.

A dev agent build has to reach a **shared environment**, not just a laptop: a composition deploys
dev renders to staging, and a staging render reads committed config. `codefly.local.yaml` is
machine-local and gitignored, so moving the override there removes the ability to dogfood a dev
agent anywhere but one developer's machine.

And `codefly.local.yaml` carries only `resolve:` — module locations. It has no agent-override
carrier, so the stricter option is a Core change plus a CLI change plus a migration of a
documented workflow, for a property the release scope above already guarantees.

A dev loop that genuinely does not need to be committed should still use `codefly.local.yaml`.
The gate never looks there.

### Why `go.mod` is reported rather than refused

First-party Go pseudo-versions are pervasive in trees whose agent pins are perfectly clean. Of
four module releases verified to have correct agent pins, three carried them — **27, 9 and 3**
respectively, and the fourth none.

Refusing them by default would fail three of those four, and many arrive as
`// indirect` entries that no change in the repository can move — they shift only when the
dependency that requires them releases. A gate that fails every repository in the fleet on the day
it lands is a gate that gets switched off in a week, so they are surfaced in the report and a
repository tightens at its own pace with `--go-modules`.

## Wiring it into a repository's CI

The check needs no workspace, no agent and no network: it reads committed text, so it runs in a
fresh clone in milliseconds.

```yaml
  prerelease:
    name: prerelease
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - name: Install codefly
        run: |   # however this repository already installs the CLI
          curl -fsSL https://raw.githubusercontent.com/codefly-dev/cli/main/install.sh | sh
      - name: No prerelease version reaches main
        run: codefly ci prerelease
```

Make it a **required** check on the default branch: a gate that a pull request can merge past is
the convention again. Add `--release` to whatever cuts the tag, so a release cannot be built over
a labelled dev override:

```bash
codefly ci prerelease --release    # before tagging
```

`--format json` emits a schema-versioned report (`schema_version`, `scope`, `status`,
`files_scanned`, `blocking[]`, `allowed[]`), each finding carrying its file, line, key, version,
kind, carrier, why it was refused and the remedy.

This repository holds itself to the rule through `TestThisRepositoryPassesItsOwnGate` in
`pkg/prerelease`, which rides the test gates already required on every pull request rather than
adding a job that would need a branch-protection change before it could bite.

Agents reach the same check over MCP as `check_prerelease_versions`, so an agent that writes a pin
can check its own edit before a human reviews it.

## Reading a failure

```text
5 prerelease versions must not reach the default branch:

  config (5) — a prerelease in committed configuration reaches the default branch, and any tag cut from it
      module/services/accounts/service.codefly.yaml:12
          agent.version = 0.1.48-dev.e87db5e08865   (prerelease-tag)
      module/services/frontend/service.codefly.yaml:8
          agent.version = 0.0.159-dev.1ed5001fd8b4   (prerelease-tag)
      ...
      -> pin a released version — `codefly agent versions <publisher>/<name>` lists what is published, `codefly update workspace` moves the pin
      -> or, for a dev agent build this composition has to run, move it to `agent-overrides` in workspace.codefly.yaml with a comment naming the issue it stands in for
      -> a dev loop that does not have to be committed belongs in codefly.local.yaml, which is gitignored and outside this gate
```

Findings are grouped by the remedy they share and the remedy is stated once per group, because the
locations are the part that differs. A passing run still lists what it permitted — an allowed
prerelease is one somebody has to remove.

## Related

- [`codefly publish dev`](commands.md#codefly-publish-dev) — how a dev build is published and
  consumed
- [`codefly update`](commands.md#codefly-update) — `--agent-override` writes the committed
  override, `codefly update service --agent-version` writes a service pin
- [`codefly doctor workspace`](commands.md#codefly-doctor-workspace) — lists every override in
  force (`agent_override_active`) and every service running a dev build (`agent_dev_build`)
- The fleet operations page that says which command owns which operation. This is its enforcement
  half: the doc tells you, the gate stops you.
