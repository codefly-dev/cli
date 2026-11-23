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
does not. A rule with no detect clause for the service's language applies as
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
			if machineReadable {
				encoder := json.NewEncoder(os.Stdout)
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(report); err != nil {
					return err
				}
			} else {
				Render(os.Stdout, report)
			}
			if report.Errors > 0 {
				return violations{count: report.Errors, machineReadable: machineReadable}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&machineReadable, "json", false, "Emit the report as JSON")
	return command
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
	case len(report.Findings) == 0 && len(report.Incomplete) > 0:
		fmt.Fprintf(out, "\n%s: nothing found, but not every source file could be inspected\n", report.Service)
	case len(report.Findings) == 0:
		fmt.Fprintf(out, "\n%s: no usage rule violated\n", report.Service)
	default:
		fmt.Fprintf(out, "\n%s: %d error(s), %d warning(s)\n", report.Service, report.Errors, report.Warnings)
	}
}
