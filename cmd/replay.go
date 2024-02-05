package cmd

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/actions/actions"
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
	ctx, done := common.NewContext()
	defer done()

	actionTracker, err := actions.NewActionTracker(ctx, track)
	cli.ExitOnError(err, "cannot create action tracker")
	actionTracker.Replay = true

	// Optionally override the directory
	actionTracker.WithDir(dir)

	steps, err := actionTracker.GetActions(ctx)
	cli.ExitOnError(err, "cannot get actions")
	for _, action := range steps {
		fmt.Println("Running action equivalent to:", action.Command())
		_, err := actions.Run(ctx, action)
		cli.ExitOnError(err, "cannot run action")
	}

}

var track string
var dir string

func init() {
	ReplayCmd.Flags().StringVar(&track, "track", "", "Replay codefly tracks")
	ReplayCmd.Flags().StringVar(&dir, "dir", "", "Replay codefly tracks")
	RootCmd.AddCommand(ReplayCmd)
}
