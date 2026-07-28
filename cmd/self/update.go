package self

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/cli/pkg/cliupdate"
	"github.com/codefly-dev/core/releaseupdate"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type updateService interface {
	Installation() cliupdate.Installation
	Check(context.Context, releaseupdate.Channel, bool) (cliupdate.CheckResult, error)
	StageAndApply(context.Context, cliupdate.CheckResult) (releaseupdate.ApplyResult, error)
}

type machineReadableUpdateError struct {
	error
}

func (machineReadableUpdateError) MachineReadable() bool {
	return true
}

var CheckUpdateCmd = newCheckUpdateCommand()

func newCheckUpdateCommand() *cobra.Command {
	var channelName string
	var jsonOutput bool
	command := &cobra.Command{
		Use:          "check-update",
		Short:        "Check the authenticated Codefly release feed for an update",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(command *cobra.Command, _ []string) error {
			channel, err := parseChannel(channelName)
			if err != nil {
				return writeCheckError(command, jsonOutput, err)
			}
			service, err := cliupdate.NewService()
			if err != nil {
				return writeCheckError(command, jsonOutput, err)
			}
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			result, err := service.Check(ctx, channel, false)
			if err != nil {
				return writeCheckError(command, jsonOutput, err)
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(result)
			}
			renderCheckResult(command.OutOrStdout(), command.ErrOrStderr(), result)
			return nil
		},
	}
	command.Flags().StringVar(&channelName, "channel", string(releaseupdate.ChannelStable), "Release channel: stable or beta")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print a machine-readable update status")
	return command
}

var UpdateCmd = newUpdateCommand()

func newUpdateCommand() *cobra.Command {
	var channelName string
	var yes bool
	var allowDowngrade bool
	command := &cobra.Command{
		Use:   "update",
		Short: "Install a verified Codefly release when this binary is directly owned",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			channel, err := parseChannel(channelName)
			if err != nil {
				return err
			}
			service, err := cliupdate.NewService()
			if err != nil {
				return err
			}
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			return runUpdate(ctx, command.OutOrStdout(), command.ErrOrStderr(), service, channel, yes, allowDowngrade)
		},
	}
	command.Flags().StringVar(&channelName, "channel", string(releaseupdate.ChannelStable), "Release channel: stable or beta")
	command.Flags().BoolVar(&yes, "yes", false, "Install without an interactive confirmation")
	command.Flags().BoolVar(&allowDowngrade, "allow-downgrade", false, "Allow the selected channel release to be older than the running version")
	return command
}

func runUpdate(
	ctx context.Context,
	output io.Writer,
	errorOutput io.Writer,
	service updateService,
	channel releaseupdate.Channel,
	yes bool,
	allowDowngrade bool,
) error {
	installation := service.Installation()
	if installation.Kind != cliupdate.InstallKindDirect {
		fmt.Fprintf(output, "Codefly installation kind: %s\n%s\n", installation.Kind, installation.Action())
		return nil
	}
	result, err := service.Check(ctx, channel, allowDowngrade)
	if err != nil {
		return err
	}
	if result.Warning != "" {
		fmt.Fprintln(errorOutput, result.Warning)
	}
	if !result.Available {
		fmt.Fprintf(output, "Codefly %s is current on the %s channel.\n", displayVersion(result.Current), result.Channel)
		return nil
	}
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("interactive confirmation requires a terminal; pass --yes to install")
		}
		confirmed, err := models.ConfirmE(ctx,
			fmt.Sprintf("Install Codefly v%s over %s?", result.Latest, installation.ResolvedPath), false)
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Fprintln(output, "Update cancelled.")
			return nil
		}
	}

	applyResult, err := service.StageAndApply(ctx, result)
	if applyResult.Applied {
		fmt.Fprintf(output, "Updated Codefly from %s to v%s.\n", displayVersion(result.Current), result.Latest)
	}
	if err != nil {
		if applyResult.Applied && errors.Is(err, releaseupdate.ErrApplyCleanup) {
			fmt.Fprintln(errorOutput, "The verified update was installed, but cleanup of the previous executable needs attention.")
		}
		return err
	}
	return nil
}

func renderCheckResult(output, errorOutput io.Writer, result cliupdate.CheckResult) {
	if result.Warning != "" {
		fmt.Fprintln(errorOutput, result.Warning)
	}
	if result.Current == "development" {
		fmt.Fprintf(output, "This is a development Codefly build. %s\n", result.Action)
		return
	}
	if result.Available {
		fmt.Fprintf(output, "Codefly v%s is available (current %s, %s channel, %s install).\n%s\n",
			result.Latest, displayVersion(result.Current), result.Channel, result.InstallKind, result.Action)
		return
	}
	fmt.Fprintf(output, "Codefly %s is current on the %s channel (%s install).\n",
		displayVersion(result.Current), result.Channel, result.InstallKind)
}

func parseChannel(value string) (releaseupdate.Channel, error) {
	channel := releaseupdate.Channel(strings.ToLower(strings.TrimSpace(value)))
	switch channel {
	case releaseupdate.ChannelStable, releaseupdate.ChannelBeta:
		return channel, nil
	default:
		return "", fmt.Errorf("unsupported release channel %q (use stable or beta)", value)
	}
}

func writeCheckError(command *cobra.Command, jsonOutput bool, err error) error {
	if !jsonOutput {
		return err
	}
	payload := struct {
		SchemaVersion int    `json:"schema_version"`
		Error         string `json:"error"`
	}{
		SchemaVersion: 1,
		Error:         err.Error(),
	}
	if encodeErr := json.NewEncoder(command.OutOrStdout()).Encode(payload); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return machineReadableUpdateError{error: err}
}

func displayVersion(version string) string {
	if version == "" || version == "development" || strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}
