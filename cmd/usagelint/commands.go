package usagelint

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

// ServiceCmd checks a service against the usage contracts of what it consumes.
var ServiceCmd = newServiceCommand()

func newServiceCommand() *cobra.Command {
	var machineReadable bool
	command := &cobra.Command{
		Use:   "usage [module/]service",
		Short: "Check a service against the usage rules of the agents it consumes",
		Long: `Check a service against the usage contracts of what it consumes.

The service's own agent and the agent of every service it depends on may
publish a usage contract: the rules for code that consumes the services it
manages, each with a reason and what to do instead. This command gathers
those contracts, asks the service's language plugin for its semantic index,
and reports every place a rule's detect clause matches.

A finding of severity error fails the command. A warning is reported and
does not. The command also fails when the plugin left source unread, a file
it could not inspect or a kind of place its analyzer does not project, and a
rule could have matched there: the partial report is printed, and it is not
a pass. A rule with no detect clause for the service's language applies as
text only and cannot be reported here.

What is not found: a call made through an import alias, or of a name imported
on its own. The plugin records calls as written, so those are missed rather
than guessed.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			cli.Init()
			if machineReadable {
				// Stdout is the report; narration goes to stderr.
				defer cli.ProtectResultStream()()
			}
			defer services.ClearAgents()
			workspace, module, service, err := common.LoadRequiredE(ctx, args)
			if err != nil {
				return err
			}
			report, err := Run(ctx, workspace, module, service)
			if err != nil {
				return err
			}
			return finish(os.Stdout, report, machineReadable)
		},
	}
	command.Flags().BoolVar(&machineReadable, "json", false, "Emit the report as JSON")
	return command
}

// finish writes the report and decides the command's status. The report is
// always written first, a partial one included, so a failure still shows
// what was found. The command then fails on a violation of severity error,
// and on a check that could not vouch for the service: an index that left
// source unread, beside a rule that could have matched in it, passes nothing.
func finish(out io.Writer, report *Report, machineReadable bool) error {
	if machineReadable {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		Render(out, report)
	}
	switch {
	case report.Errors > 0:
		return violations{count: report.Errors, machineReadable: machineReadable}
	case report.Unverified():
		return unverified{service: report.Service, gaps: len(report.Incomplete), machineReadable: machineReadable}
	}
	return nil
}

// unverified is the failure of a check that could not read everything a rule
// might have matched in.
type unverified struct {
	service         string
	gaps            int
	machineReadable bool
}

func (u unverified) Error() string {
	if u.machineReadable {
		return ""
	}
	return fmt.Sprintf("%s could not be checked completely (%d gap(s) above); this is not a pass", u.service, u.gaps)
}

// violations is the failure of a check that found errors. With --json the
// report already said everything, so the error carries no second message.
type violations struct {
	count           int
	machineReadable bool
}

func (v violations) Error() string {
	if v.machineReadable {
		return ""
	}
	return fmt.Sprintf("%d usage rule violation(s)", v.count)
}

// Render writes a report for a person, or for an agent reading a terminal:
// each finding carries the rule's reason and what to do instead, because the
// message is the instruction.
func Render(out io.Writer, report *Report) {
	if len(report.Owners) == 0 {
		fmt.Fprintf(out, "%s: no agent it consumes publishes a usage contract\n", report.Service)
		return
	}
	for _, owner := range report.Owners {
		fmt.Fprintf(out, "%s (%s): %d rule(s), %d checkable here\n", owner.Owner, owner.Through, owner.Rules, owner.Detectable)
	}
	for _, line := range report.Incomplete {
		fmt.Fprintf(out, "incomplete: %s\n", line)
	}
	for i := range report.Findings {
		finding := &report.Findings[i]
		where := finding.Path
		if finding.Line > 0 {
			where = fmt.Sprintf("%s:%d:%d", finding.Path, finding.Line, finding.Column)
		}
		fmt.Fprintf(out, "\n%s: %s %s %s: %s\n", where, finding.Severity, finding.RuleID, finding.Kind, finding.Symbol)
		fmt.Fprintf(out, "  %s (%s)\n", finding.Title, finding.Owner)
		fmt.Fprintf(out, "  why: %s\n", finding.Rationale)
		fmt.Fprintf(out, "  do:  %s\n", finding.Do)
		if finding.Docs != "" {
			fmt.Fprintf(out, "  see: %s in %s\n", finding.Docs, finding.Owner)
		}
	}
	switch {
	case len(report.Incomplete) > 0:
		fmt.Fprintf(out, "\n%s: %d error(s), %d warning(s) in what was read; not everything could be read\n", report.Service, report.Errors, report.Warnings)
	case len(report.Findings) == 0:
		fmt.Fprintf(out, "\n%s: no usage rule violated\n", report.Service)
	default:
		fmt.Fprintf(out, "\n%s: %d error(s), %d warning(s)\n", report.Service, report.Errors, report.Warnings)
	}
}

// GuidanceCmd writes the usage rules a service consumes into its AGENTS.md.
var GuidanceCmd = newGuidanceCommand()

func newGuidanceCommand() *cobra.Command {
	var check, machineReadable bool
	command := &cobra.Command{
		Use:   "usage [module/]service",
		Short: "Write the usage rules of the agents a service consumes into its AGENTS.md",
		Long: `Write the usage rules of what a service consumes into its AGENTS.md.

The service's own agent and the agent of every service it depends on may
publish a usage contract. This command renders their rules, one line each,
into a block of the service's AGENTS.md, between two markers. The block is
regenerated on every run and never edited by hand; everything outside the
markers is left as it is. When nothing the service consumes publishes a
contract, the block is removed, and no file is created for it.

A coding agent reads AGENTS.md without being asked, so this is how it learns
a rule exists before it writes the code. The reason behind a rule and what to
do instead are printed by 'codefly lint usage' where the rule is broken.

With --check nothing is written and the command fails when the file is not
current: the drift gate for CI.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			cli.Init()
			if machineReadable {
				defer cli.ProtectResultStream()()
			}
			defer services.ClearAgents()
			workspace, module, service, err := common.LoadRequiredE(ctx, args)
			if err != nil {
				return err
			}
			result, err := Sync(ctx, workspace, module, service, check)
			if err != nil {
				return err
			}
			if machineReadable {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(result); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(os.Stdout, describe(result, check))
			}
			if check && result.Changed {
				return stale{path: result.Path, machineReadable: machineReadable}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&check, "check", false, "Write nothing and fail when the file is not current")
	command.Flags().BoolVar(&machineReadable, "json", false, "Emit the result as JSON")
	return command
}

func describe(result *Guidance, check bool) string {
	switch {
	case !result.Changed && result.Owners == 0:
		return fmt.Sprintf("%s: no agent it consumes publishes a usage contract; nothing to write", result.Service)
	case !result.Changed:
		return fmt.Sprintf("%s: %s is current (%d owner(s))", result.Service, result.Path, result.Owners)
	case check:
		return fmt.Sprintf("%s: %s is not current; run `codefly sync usage`", result.Service, result.Path)
	}
	return fmt.Sprintf("%s: wrote the usage rules of %d owner(s) to %s", result.Service, result.Owners, result.Path)
}

// stale is the failure of --check on a file that is not current.
type stale struct {
	path            string
	machineReadable bool
}

func (s stale) Error() string {
	if s.machineReadable {
		return ""
	}
	return fmt.Sprintf("%s is not current with the usage rules of what the service consumes", s.path)
}
