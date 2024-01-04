package sync

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/services/factory"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceCmd represents the run command
var ServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Sync a service",

	Run: func(cmd *cobra.Command, args []string) {
		err := syncService()
		cli.ExitOnError(err, "cannot sync service")

	},
}

// Sync service works a lot like the flow run manager because it is using dependencies
func syncService() error {
	ctx, done := common.NewContext()
	defer done()
	defer agents.ClearAgents()

	w := wool.Get(ctx).In("cmd.sync.Service")

	project := common.Project(ctx)
	w.Trace("project", wool.Field("project", project.Name))
	app := common.Application(ctx)
	w.Trace("app", wool.Field("app", app.Name))
	service := common.Service(ctx)
	w.Trace("service", wool.Field("service", service.Name))

	f, err := factory.NewFlow(ctx, project, service)
	if err != nil {
		return w.Wrap(err)
	}
	f.InitOnly(initOnly)
	return f.StartSync(ctx)
}

func init() {
	ServiceCmd.Flags().BoolVar(&initOnly, "init-only", false, "Only run the init phase")
}
