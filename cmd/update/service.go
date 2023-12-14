package update

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Update an service",

	Run: func(cmd *cobra.Command, args []string) {
		ctx := shared.NewContext()
		service := common.Service(ctx)

		_, err := services.Load(ctx, service)
		shared.ExitOnError(err, "cannot load service")

	},
}

func init() {
	ServiceCmd.Flags().BoolVarP(&current, "current", "c", false, "update current application")
}
