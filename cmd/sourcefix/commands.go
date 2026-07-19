package sourcefix

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/sourceworkspace"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

type commandFlags struct {
	files      []string
	paths      []string
	mode       string
	aggressive bool
	dryRun     bool
	check      bool
	json       bool
	dir        string
}

var ServiceCmd = newServiceCommand()
var SourceCmd = newSourceCommand()

func newServiceCommand() *cobra.Command {
	flags := &commandFlags{}
	command := &cobra.Command{
		Use:   "service [module/]service",
		Short: "Safely repair service source through its language plugin",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			cli.Init()
			defer services.ClearAgents()
			workspace, module, service, err := common.LoadRequiredE(ctx, args)
			if err != nil {
				return err
			}
			return runCommand(ctx, workspace, module, service, flags)
		},
	}
	bindFlags(command, flags)
	return command
}

func newSourceCommand() *cobra.Command {
	flags := &commandFlags{}
	command := &cobra.Command{
		Use:   "source",
		Short: "Safely repair an arbitrary source checkout through its detected plugin",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			cli.Init()
			defer services.ClearAgents()
			dir := flags.dir
			if dir == "" {
				var err error
				dir, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			prepared, err := sourceworkspace.Prepare(ctx, dir)
			if err != nil {
				return err
			}
			defer prepared.Close()
			return runCommand(ctx, prepared.Workspace, prepared.Module, prepared.Service, flags)
		},
	}
	bindFlags(command, flags)
	command.Flags().StringVar(&flags.dir, "dir", "", "Source checkout (default: current directory)")
	return command
}

func bindFlags(command *cobra.Command, flags *commandFlags) {
	command.Flags().StringSliceVarP(&flags.files, "file", "f", nil, "Source-root-relative file to repair (repeatable)")
	command.Flags().StringSliceVarP(&flags.paths, "path", "p", nil, "Directory scope to repair recursively (repeatable)")
	command.Flags().StringVar(&flags.mode, "mode", "safe", "Fix mode: safe or aggressive")
	command.Flags().BoolVar(&flags.aggressive, "aggressive", false, "Enable explicitly aggressive language fixes")
	command.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Preview changes and evidence without writing")
	command.Flags().BoolVar(&flags.check, "check", false, "Preview without writing and fail if changes are needed")
	command.Flags().BoolVar(&flags.json, "json", false, "Emit machine-readable JSON evidence")
}

func runCommand(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, flags *commandFlags) error {
	mode, err := parseMode(flags.mode, flags.aggressive)
	if err != nil {
		return err
	}
	report, err := Run(ctx, workspace, module, service, Options{
		Files: flags.files, Paths: flags.paths, Mode: mode, DryRun: flags.dryRun || flags.check,
	})
	if err != nil {
		return err
	}
	if flags.json {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
	} else {
		renderReport(report)
	}
	if flags.check && report.Changed > 0 {
		return checkFailure{count: report.Changed, machineReadable: flags.json}
	}
	return nil
}

func parseMode(value string, aggressive bool) (basev0.FixMode, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if aggressive {
		if value != "" && value != "safe" && value != "aggressive" {
			return 0, fmt.Errorf("--aggressive conflicts with --mode=%s", value)
		}
		return basev0.FixMode_FIX_MODE_AGGRESSIVE, nil
	}
	switch value {
	case "", "safe":
		return basev0.FixMode_FIX_MODE_SAFE, nil
	case "aggressive":
		return basev0.FixMode_FIX_MODE_AGGRESSIVE, nil
	default:
		return 0, fmt.Errorf("unknown fix mode %q (use safe or aggressive)", value)
	}
}

func renderReport(report *Report) {
	state := "applied"
	if report.DryRun {
		state = "previewed"
	}
	fmt.Printf("Source fixes %s for %s: %d changed, %d written\n", state, report.Service, report.Changed, report.Written)
	for _, result := range report.Results {
		if !result.Changed {
			fmt.Printf("  = %s unchanged\n", result.File)
			continue
		}
		fmt.Printf("  ✓ %s  %s → %s  [%s]\n", result.File, shortHash(result.BeforeSHA256), shortHash(result.AfterSHA256), strings.Join(result.Actions, ", "))
		if result.Output != "" {
			fmt.Printf("    %s\n", strings.ReplaceAll(strings.TrimSpace(result.Output), "\n", "\n    "))
		}
	}
}

func shortHash(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

type checkFailure struct {
	count           int
	machineReadable bool
}

func (e checkFailure) Error() string         { return fmt.Sprintf("%d source file(s) need fixes", e.count) }
func (e checkFailure) MachineReadable() bool { return e.machineReadable }
