# Runbooks

Task-oriented, copy-pasteable procedures. If a doc explains *how something works*, it belongs
in `docs/` proper; if it lists *the steps to do a thing*, it belongs here.

The canonical entry point is [`../../AGENTS.md`](../../AGENTS.md); its **How-To Index** links to
each runbook below.

> **These files are embedded in the CLI** by [`runbooks.go`](runbooks.go), so they're reachable
> at runtime without a repo checkout:
> - `codefly help <topic>` (shell) — e.g. `codefly help bump-go-version`.
> - the `how_to` MCP tool (`codefly mcp`) — omit `topic` to list, pass it to fetch.
>
> `runbooks.go` keeps these `.md` files as the single source of truth (no duplication); editing a
> runbook updates both surfaces on the next build. The `README.md` index is excluded from the
> topic list.

## Index by category

### Toolchain & dependencies
- [Bump the Go version](bump-go-version.md) — update Go across `go.mod`, CI, release images, and
  every companion repo (core, wool, agents) in lockstep.

### Shipping
- [Cut a release](cut-a-release.md) — tag → GoReleaser → signed archives, SBOMs, attestations,
  Homebrew cask.

### Extending the CLI
- [Add a new command](add-a-command.md) — Cobra wiring, help/`explain`, docs, MCP exposure.
- [Rebuild the CLI and agents from local source](update-agents.md) — `codefly self build` and
  `--with-agents`.

## Adding a runbook

1. Create `docs/runbooks/<verb-noun>.md`. Lead with a one-line summary and a "when to use this".
2. Write **ordered, exact** steps — real paths, real flags, real commands. Show the verification
   step at the end.
3. Add it to the index above **and** to the How-To Index in [`AGENTS.md`](../../AGENTS.md).
4. Note every place a value is pinned (grep for it) so the next person doesn't miss one.
