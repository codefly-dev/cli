package environment

import (
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var showJSON bool

var showCmd = &cobra.Command{
	Use:          "show <env>",
	Short:        "Print the resolved environment as YAML (or --json)",
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return fmt.Errorf("load workspace: %w", err)
		}
		env, err := environments.Select(workspace, args[0])
		if err != nil {
			return err
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
		cmd.Print(string(out))
		if showJSON {
			cmd.Println()
		}
		return nil
	},
}

func init() {
	showCmd.Flags().BoolVar(&showJSON, "json", false, "Print the environment as JSON instead of YAML")
}
