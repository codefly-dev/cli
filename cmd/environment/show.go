package environment

import (
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var showJSON bool

var showCmd = &cobra.Command{
	Use:          "show <env>",
	Short:        "Print the resolved environment as YAML (or --json)",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("load workspace: %w", err)
		}
		env := workspace.FindEnvironment(args[0])
		if env == nil {
			return fmt.Errorf("environment %q is not declared in %s", args[0], resources.WorkspaceConfigurationName)
		}

		var out []byte
		if showJSON {
			out, err = json.MarshalIndent(env, "", "  ")
		} else {
			out, err = yaml.Marshal(env)
		}
		if err != nil {
			return err
		}
		fmt.Print(string(out))
		if showJSON {
			fmt.Println()
		}
		return nil
	},
}

func init() {
	showCmd.Flags().BoolVar(&showJSON, "json", false, "Print the environment as JSON instead of YAML")
}
