package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/runners/base"
	"github.com/spf13/cobra"
)

var RecoverCmd = newRecoverCommand(base.PreviewProcessGroupRecovery, base.RecoverProcessGroup)

// Only selection and reporting live here. Core owns receipt decoding,
// authentication, generation checks, signal delivery and conditional removal.
func newRecoverCommand(
	preview func(context.Context, int) (*base.ProcessGroupRecoveryExpectation, error),
	recoverGroup func(context.Context, base.ProcessGroupRecoveryExpectation) (*base.ProcessGroupRecoveryResult, error),
) *cobra.Command {
	parent := &cobra.Command{
		Use: "recover", Short: "Recover an explicitly selected resource or process",
	}
	var previewOnly bool
	var expectationPath string
	group := &cobra.Command{
		Use:   "process-group PGID",
		Short: "Preview or recover one authenticated orphan process-group generation",
		Long: `Select one retained process group explicitly, independently of workspace-scoped stop.
Use --preview to authenticate it without signaling and print Core's JSON receipt.
Review and save the complete receipt, then use --expect FILE with the same PGID.
Core revalidates the registration, absent original owner and process births before
effects. Changed, missing or ambiguous evidence refuses recovery. Workspace
membership is not inferred. Containers, caches and other groups are not selected.
Recovery succeeds only when the group is dead and its exact record was removed.
An error can report partial termination; inspect the result before any retry.`,
		Example: `  codefly recover process-group 12345 --preview > recovery.json
  codefly recover process-group 12345 --expect recovery.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pgid, err := strconv.Atoi(args[0])
			if err != nil || pgid <= 1 {
				return errors.New("PGID must be an integer greater than 1")
			}
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
				expected, previewErr := preview(ctx, pgid)
				if previewErr != nil {
					return previewErr
				}
				if expected == nil || expected.PGID != pgid {
					return errors.New("core returned no matching recovery preview")
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(expected)
			}
			file, err := os.Open(expectationPath)
			if err != nil {
				return fmt.Errorf("read recovery receipt: %w", err)
			}
			defer file.Close()
			// Bound allocation for operator input; Core enforces its own format
			// size limit and is the sole decoder of the expectation.
			data, err := io.ReadAll(io.LimitReader(file, base.MaxProcessGroupRecoveryExpectationBytes+1))
			if err != nil {
				return fmt.Errorf("read recovery receipt: %w", err)
			}
			if len(data) > base.MaxProcessGroupRecoveryExpectationBytes {
				return fmt.Errorf("recovery receipt exceeds %d bytes", base.MaxProcessGroupRecoveryExpectationBytes)
			}
			expected, err := base.ParseProcessGroupRecoveryExpectation(data)
			if err != nil {
				return fmt.Errorf("invalid recovery receipt: %w", err)
			}
			if expected.PGID != pgid {
				return errors.New("receipt PGID does not match the explicitly selected PGID")
			}
			result, recoveryErr := recoverGroup(ctx, expected)
			var reportErr error
			if result != nil {
				reportErr = json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			if recoveryErr != nil {
				return errors.Join(recoveryErr, reportErr)
			}
			if result == nil || !result.GroupDead || !result.RecordRemoved {
				return errors.Join(errors.New("recovery incomplete: group death and exact record removal were not both confirmed"), reportErr)
			}
			return reportErr
		},
	}
	group.Flags().BoolVar(&previewOnly, "preview", false, "Authenticate one generation and print its receipt without signaling")
	group.Flags().StringVar(&expectationPath, "expect", "", "Recover only the generation in this reviewed Core JSON receipt")
	parent.AddCommand(group)
	return parent
}
