package sync

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/partial"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/golor"
	"github.com/spf13/cobra"
)

// PartialCmd represents the run command
var PartialCmd = &cobra.Command{
	Use:   "partial",
	Short: "Sync a partial",

	Run: func(cmd *cobra.Command, args []string) {

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()

		// errs := make(chan error)

		if len(args) == 0 {
			fmt.Println("Please provide a partial name")
			os.Exit(0)
		}
		name := args[0]

		project := common.ProjectConfiguration(current)

		conf, err := project.GetPartial(name)
		if err != nil {
			fmt.Printf("Cannot find partial <%s> in project <%s>\n", name, project.Name)
			os.Exit(1)
		}

		part, err := partial.NewPartial(project, conf)
		shared.ExitOnError(err, "<%s>", conf.Name)

		if initOnly {
			return
		}
		err = part.Configure(ctx)
		shared.UnexpectedExitOnError(err, "cannot configure partial")

		err = part.Sync(ctx)
		shared.UnexpectedExitOnError(err, "cannot sync partial")

		golor.Println(`#(blue,bold)[Syncing partial done]`)
	},
}

func init() {
	PartialCmd.Flags().BoolVar(&current, "current", false, "Run the current partial")
	PartialCmd.Flags().BoolVar(&initOnly, "init-only", false, "Only initialize the partial")
}
