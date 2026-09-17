---
name: add-a-command
description: Add a Cobra verb or subcommand to the codefly CLI and wire up registration, standalone help, the command reference, MCP exposure, and tests. Use when adding "codefly foo" or "codefly foo bar", when extending an existing command group, or when the CLI cannot express something a caller needs and the fix is a new capability rather than a workaround.
---

# Add a CLI command

**Steps, the Cobra pattern, and the closing checklist:**
[docs/runbooks/add-a-command.md](../../../docs/runbooks/add-a-command.md).
User-facing reference to update: [docs/commands.md](../../../docs/commands.md).

## What must not be skipped

- **`RunE`, never `Run`.** Return errors; the root owns exit codes. Get a context from
  `common.NewContext()`.
- **Help must stand alone without network access.** Fill `Short`, `Long`, and `Example`.
  `codefly explain <cmd>` reprints that static text, so it is the contract — an external
  `codefly-help` provider only augments it.
- **Parent groups reject unknown subcommands.** Wire new ones through
  `configureSubcommandValidation` / `rejectUnknownSubcommand` in `cmd/root.go` so
  `codefly foo bogus` errors cleanly.
- **Decide on MCP exposure explicitly** — register a tool in `pkg/mcp/` or say in the PR that
  you skipped it. Silence is not a decision.
- **Add it to `docs/commands.md`**, and to the Global Flags table if it introduces one.
- **Tests exercise real wiring** — no mocks. Cover registration and that help renders.

## Why this is often the right change

A gap in the CLI is a bug in the CLI. If a caller is scripting around a missing verb or
hand-writing environment the orchestration layer should resolve, the fix belongs here, not at
the call site.
