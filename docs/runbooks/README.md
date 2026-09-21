# Runbooks

Task-oriented, copy-pasteable procedures. If a doc explains *how something works*, it belongs
in `docs/` proper; if it lists *the steps to do a thing*, it belongs here.

The canonical entry point is [`../../AGENTS.md`](../../AGENTS.md); its **How-To Index** links to
each runbook below.

## Index by category

### Toolchain & dependencies
- [Bump the Go version](bump-go-version.md) — update Go across `go.mod`, CI, release images, and
  every companion repo (core, wool, agents) in lockstep.

### Shipping
- [Cut a release](cut-a-release.md) — tag → GoReleaser → signed archives, SBOMs, attestations,
  Homebrew cask.
- [Release affected agents](release-the-fleet.md) — publish only agents needing an implementation
  or protocol change; unchanged contracts require no fleet repinning.
- [Publish an image to the registry](publish-an-image.md) — `ghcr.io/codefly-dev` visibility,
  repo linking, digest pinning, anonymous-pull guards.

### Extending the CLI
- [Add a new command](add-a-command.md) — Cobra wiring, help/`explain`, docs, MCP exposure.
- [Rebuild the CLI and agents from local source](update-agents.md) — `codefly self build` and
  `--with-agents`.

## Adding a runbook

1. Create `docs/runbooks/<verb-noun>.md`. Lead with a one-line summary and a "when to use this".
2. Write **ordered, exact** steps — real paths, real flags, real commands. Show the verification
   step at the end.
3. Add it to the index above **and** to the How-To Index in [`AGENTS.md`](../../AGENTS.md).
4. Add `.claude/skills/<name>/SKILL.md` so the runbook triggers on its own, without `AGENTS.md`
   being read. Every runbook has one. Keep the body to *when this applies* and *what must not be
   skipped*, and point at the runbook for the steps — one copy of the steps, one thing to drift.
   The frontmatter `description` is all an agent sees before loading the skill, so write what it
   does **and** when to reach for it.
5. Note every place a value is pinned (grep for it) so the next person doesn't miss one.
