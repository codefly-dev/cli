package add

import (
	"github.com/codefly-dev/cli/cmd/common"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServiceDependencyCmd represents the run command
var ServiceDependencyCmd = &cobra.Command{
	Use:   "dependency",
	Short: "Add a service dependency",

	Run: func(cmd *cobra.Command, args []string) {
		if interactive {
			common.CLI().Oops("Interactive mode not implemented yet")
		}
		addServiceDependency()
	},
}

func addServiceDependency() {
	ctx, done := common.NewContext()
	defer done()
	defer agents.ClearAgents()

	w := wool.Get(ctx).In("cmd.add.serviceDependency")

	project := common.Project(ctx)
	w.Trace("project", wool.Field("project", project))
	app := common.Application(ctx)
	w.Trace("app", wool.Field("app", app))
	service := common.Service(ctx)
	w.Trace("service", wool.Field("service", service))

	//
	//if app.ExistsService(name) && !override {
	//	common.CLI().Oops("Service <{{.}}> already exists", name)
	//}
	//
	//w.Debug("input", wool.Field("agent", agent))
	//agent, err := configurations.ParseAgent(ctx, configurations.ServiceAgent, agentInput)
	//cli.ExitOnError(err, "Cannot parse agent")
	//
	//confirm := models.Confirm(golor.Sprintf("Confirm adding a service in your application <{{.Name}}>?", app), true)
	//if !confirm {
	//	cli.Header(2, "Received loud and clear!")
	//	cli.Exit()
	//}
	//
	//input := &actionsservice.AddService{
	//	Name:        name,
	//	Project:     project.Name,
	//	Application: app.Name,
	//	Agent:       agent.Proto(),
	//	Override:    override,
	//}
	//
	//addDescription := models.Confirm("Do you want to add a short description?", false)
	//if addDescription {
	//	input.Description = models.Input("Description", "Make some magic 🪄")
	//
	//}
	//
	//err = services.Add(ctx, input)
	//cli.ExitOnError(err, "Cannot add service")

}
