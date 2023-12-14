package cmd

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/actions/actions"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ReplayCmd represents the replay command
var ReplayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Replay",
	Run: func(cmd *cobra.Command, args []string) {
		if track == "" {
			cli.Error("You must provide a track to replay")
			os.Exit(0)
		}
		replayCodefly(track)
	},
}

func replayCodefly(track string) {
	ctx := shared.NewContext()
	logger := shared.GetLogger(ctx).With("Replay")
	tracker := actions.NewActionTracker(track)
	steps, err := tracker.GetActions(ctx)
	shared.UnexpectedExitOnError(err, "cannot get actions")
	logger.DebugMe("Replaying #%d steps", len(steps))
	for _, action := range steps {
		fmt.Println("Running action equivalent to:", action.Command())
		_, err := actions.Run(ctx, action)
		shared.UnexpectedExitOnError(err, "cannot run action")
	}

}

var track string

func init() {
	ReplayCmd.Flags().StringVar(&track, "track", "", "Replayialize codefly with demo project")
	ReplayCmd.Flags().BoolVar(&override, "override", false, "Override existing configuration")
	RootCmd.AddCommand(ReplayCmd)
}
