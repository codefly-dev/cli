package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/spf13/cobra"
)

func init() {
	RecoverCmd.AddCommand(newRecoverContainerCommand(dockerrun.PreviewContainerStop, dockerrun.StopContainer))
}

// Core owns receipt validation, exact-object comparison, stop and readback.
// This command selects one operator-supplied ID; it infers no service ownership.
func newRecoverContainerCommand(
	preview func(context.Context, string) (*dockerrun.ContainerStopExpectation, error),
	stopContainer func(context.Context, dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error),
) *cobra.Command {
	var previewOnly bool
	var expectationPath string
	command := &cobra.Command{
		Use:   "container ID",
		Short: "Preview or stop one explicitly selected container, retaining its data",
		Long: `Select one container by its full lowercase 64-hex Docker ID.
Use --preview to inspect it without effects and print Core's JSON receipt.
Review and save the complete receipt, then use --expect FILE with the same ID.
Core compares the identity, creation time, name, labels and mounts before stopping
that ID only. AutoRemove containers refuse. No name lookup, parent-service claim,
label rewrite, container removal, volume removal or restart is performed.
Success requires the same container retained and stopped with unchanged mounts
and labels. This does not prove stored data contents or qualify producer reuse.
An error may follow a stop; inspect the reported result before any retry.`,
		Example: `  codefly recover container <full-container-id> --preview > container-stop.json
  codefly recover container <full-container-id> --expect container-stop.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if cmd.Flags().Changed("expect") && expectationPath == "" {
				return errors.New("--expect requires a receipt file")
			}
			if previewOnly == (expectationPath != "") {
				return errors.New("select exactly one of --preview or --expect FILE")
			}
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			if previewOnly {
				expected, err := preview(ctx, id)
				if err != nil {
					return err
				}
				if expected == nil || expected.ID != id {
					return errors.New("core returned no matching container stop preview")
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(expected)
			}
			file, err := os.Open(expectationPath)
			if err != nil {
				return fmt.Errorf("read container stop receipt: %w", err)
			}
			defer file.Close()
			data, err := io.ReadAll(io.LimitReader(file, dockerrun.MaxContainerStopExpectationBytes+1))
			if err != nil {
				return fmt.Errorf("read container stop receipt: %w", err)
			}
			if len(data) > dockerrun.MaxContainerStopExpectationBytes {
				return fmt.Errorf("container stop receipt exceeds %d bytes", dockerrun.MaxContainerStopExpectationBytes)
			}
			expected, err := dockerrun.ParseContainerStopExpectation(data)
			if err != nil {
				return fmt.Errorf("invalid container stop receipt: %w", err)
			}
			if expected.ID != id {
				return errors.New("receipt ID does not match the explicitly selected container ID")
			}
			result, stopErr := stopContainer(ctx, expected)
			var reportErr error
			if result != nil {
				reportErr = json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			if stopErr != nil {
				return errors.Join(stopErr, reportErr)
			}
			if result == nil || !result.Stopped || !result.Retained || !result.MountsUnchanged {
				return errors.Join(errors.New("container stop incomplete: stopped, retained and unchanged mounts were not all confirmed"), reportErr)
			}
			return reportErr
		},
	}
	command.Flags().BoolVar(&previewOnly, "preview", false, "Inspect one full container ID and print its receipt without stopping it")
	command.Flags().StringVar(&expectationPath, "expect", "", "Stop only the exact container matching this reviewed Core JSON receipt")
	return command
}
