package add

import (
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	runnableops "github.com/codefly-dev/cli/pkg/runnable"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

var runnableAgent, runnableHandler, runnableModule string
var runnableJSON bool

var RunnableCmd = &cobra.Command{
	Use:   "runnable <name>",
	Short: "Create a typed Runnable through its language agent",
	Long: `Create a Runnable declaration and scaffold its handler through the selected
agent over gRPC. Both the pinned agent and handler path are explicit. Edit the
generated declaration's input/output contract and handler before building.

Creation is unattended. It fails if the name already exists or the agent lacks
the Runnable Builder lifecycle. It does not install or invoke work.`,
	Example: "  codefly add runnable word-count --agent=python:0.0.1 --handler=handler.py\n  codefly add runnable word-count --module=backend --agent=python:0.0.1 --handler=handler.py --json",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		var module *resources.Module
		if runnableModule != "" {
			module, err = workspace.LoadModuleFromName(ctx, runnableModule)
		} else {
			module, err = common.LoadModule(ctx)
		}
		if err != nil {
			return err
		}
		agent, err := resources.ParseAgent(ctx, resources.RunnableAgent, runnableAgent)
		if err != nil {
			return err
		}
		r, err := runnableops.Create(ctx, workspace, module, args[0], agent, runnableHandler, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		if runnableJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"name": r.Name, "module": module.Name, "directory": r.Dir(), "agent": r.Agent.Identifier()})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created Runnable %s/%s at %s\nEdit its contract and handler, then run: codefly build runnable %s/%s\n", module.Name, r.Name, r.Dir(), module.Name, r.Name)
		return err
	},
}

func init() {
	RunnableCmd.Flags().StringVar(&runnableAgent, "agent", "", "Pinned Runnable agent (publisher/name:version)")
	RunnableCmd.Flags().StringVar(&runnableHandler, "handler", "", "Handler path relative to the Runnable directory")
	RunnableCmd.Flags().StringVar(&runnableModule, "module", "", "Owning module (defaults to the current module)")
	RunnableCmd.Flags().BoolVar(&runnableJSON, "json", false, "Emit the created resource as JSON")
	if err := RunnableCmd.MarkFlagRequired("agent"); err != nil {
		panic(err)
	}
	if err := RunnableCmd.MarkFlagRequired("handler"); err != nil {
		panic(err)
	}
}
