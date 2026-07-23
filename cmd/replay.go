package cmd

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/actions/actions"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// ReplayCmd represents the replay command
var ReplayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Re-run operations recorded in a Codefly action track",
	RunE: func(cmd *cobra.Command, args []string) error {
		if track == "" {
			return fmt.Errorf("you must provide a track to replay")
		}
		return replayCodefly(track)
	},
}

func replayCodefly(track string) error {
	ctx, done := common.NewContext()
	defer done()

	actionTracker, err := actions.NewActionTracker(ctx, resources.CodeflyDir(), track)
	if err != nil {
		return fmt.Errorf("cannot create action tracker: %w", err)
	}
	actionTracker.Replay = true

	// Optionally override the directory
	actionTracker.WithDir(dir)

	steps, err := actionTracker.GetActions(ctx)
	if err != nil {
		return fmt.Errorf("cannot get actions: %w", err)
	}
	for _, action := range steps {
		fmt.Println("Running action equivalent to:", action.Command())
		// TODO: Need to update the action space from the output
		_, err := actions.Run(ctx, action, nil)
		if err != nil {
			return fmt.Errorf("cannot run action: %w", err)
		}
	}
	return nil
}

var track string
var dir string

func init() {
	ReplayCmd.Flags().StringVar(&track, "track", "", "Replay codefly tracks")
	ReplayCmd.Flags().StringVar(&dir, "dir", "", "Replay codefly tracks")
}
