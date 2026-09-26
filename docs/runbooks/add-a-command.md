# Runbook: Add a new command

Add a Cobra command (or subcommand) to the CLI and wire up help, docs, and — where relevant —
MCP exposure. See [../commands.md](../commands.md) for the narrative command guide and
[../cli-reference.md](../cli-reference.md) for the generated list of every command and flag.

## When to use

You're adding a new verb (`codefly foo`) or a new subcommand under an existing verb
(`codefly foo bar`).

## The pattern

Commands live in `cmd/`. A **top-level** command is a `*cobra.Command` variable in
`cmd/<name>.go`, registered in `cmd/root.go`. A command **with subcommands** gets its own
`cmd/<name>/` package; the parent adds each child in its `init()`.

Example — the `run` command (`cmd/run.go`):

```go
package cmd

import (
	"github.com/codefly-dev/cli/cmd/run"
	"github.com/spf13/cobra"
)

var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "Start a service or job in its local workspace context",
}

func init() {
	RunCmd.AddCommand(run.ServiceCmd)   // from package cmd/run
	RunCmd.AddCommand(run.JobCmd)
	RunCmd.AddCommand(run.CommandCmd)
}
```

A leaf command (`cmd/run/service.go`) supplies the behavior:

```go
package run

var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Start a service locally with its dependency graph",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runServiceCommand,
}

func runServiceCommand(cmd *cobra.Command, args []string) (returnErr error) {
	ctx, done := common.NewContext()   // standard context/cleanup helper
	defer done()
	// ...
}
```

## Steps

### 1. Create the command

- **New verb:** add `cmd/<name>.go` with a `var <Name>Cmd = &cobra.Command{Use: "<name>", Short: ...}`.
- **New subcommand:** add `cmd/<parent>/<name>.go` in the parent's package with a
  `var <Name>Cmd`, and register it in the parent's `init()` via `<Parent>Cmd.AddCommand(<Name>Cmd)`.

Use `RunE` (not `Run`) and return errors — the root handles exit codes. Get a context via
`common.NewContext()`.

### 2. Register a top-level command

Add it in `cmd/root.go`'s `init()`:

```go
RootCmd.AddCommand(FooCmd)
```

Group commands that only exist to hold subcommands should reject unknown/missing subcommands —
`cmd/root.go` provides `configureSubcommandValidation` / `rejectUnknownSubcommand`; wire new
parent commands through the same path so `codefly foo bogus` errors cleanly.

### 3. Write help text that stands alone

Every command must give complete, useful `--help` **without network access** (see
[../commands.md](../commands.md)). Fill in `Short`, `Long`, and `Example`. `codefly explain
<cmd>` reprints this static help and optionally augments it via an external `codefly-help`
provider — so the static text is the contract; keep it accurate.

### 4. Regenerate the command reference, and write up the verb

Two documents, two jobs.

[../cli-reference.md](../cli-reference.md) is generated from the command tree and lists every
command and flag. Regenerate it — `go test ./cmd -run TestCLIReferenceIsCurrent -update-reference`
— and commit the result. `TestCLIReferenceIsCurrent` fails otherwise, naming your command: that is
the guard, and it is why a new verb can no longer ship undocumented. Its text is the `Short`,
`Long` and flag usage from step 3, so the reference and `--help` cannot disagree.

[../commands.md](../commands.md) is the narrative guide, and is still written by hand: add the
command under the right category with what it is *for* and the chain it takes part in — the thing
generated text cannot say. If it introduces a global flag, add it to the Global Flags table.

### 5. Consider MCP exposure

Per the project rule, when you add a CLI capability, decide whether AI agents should reach it too.
If yes, register a corresponding tool in `pkg/mcp/` (`tools.go` / `service_tools.go` / …). See
[../mcp-server.md](../mcp-server.md).

One family is already decided: no `codefly deploy` verb is exposed, because they act on real
clusters and secret stores with the operator's own credentials, and an MCP tool is called by an
agent rather than typed by that operator. `TestNoDeployVerbIsExposedAsATool` in `pkg/mcp` holds
that line. Changing it is a deliberate edit to that test, not an oversight.

### 6. Test

Command tests live next to the command (e.g. `cmd/run/service_test.go`, `cmd/root_test.go`).
Follow the no-mock rule — exercise real wiring. Add a test that the command is registered and its
help renders.

### 7. Verify

```bash
go build ./cmd/codefly && ./codefly <name> --help
./codefly explain <name>
go test ./cmd/...
```

## Checklist

- [ ] Command/subcommand file created; `RunE` returns errors
- [ ] Registered in `cmd/root.go` (top-level) or the parent's `init()`
- [ ] Parent groups reject unknown subcommands
- [ ] `Short`/`Long`/`Example` complete and network-free
- [ ] `docs/cli-reference.md` regenerated and committed (`go test ./cmd -run TestCLIReferenceIsCurrent -update-reference`)
- [ ] Written up in `docs/commands.md` under the right category
- [ ] MCP tool added or consciously skipped (no `deploy` verb is exposed — see step 5)
- [ ] Tests pass; `--help` and `explain` render
