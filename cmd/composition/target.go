package composition

import (
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

func newTargetCommand(workspace *string) *cobra.Command {
	var expected string
	command := &cobra.Command{
		Use:   "inspect-local-target ENVIRONMENT",
		Short: "Read the live local Kubernetes target identity without mutation",
		Long: "Read an explicitly declared local k3d environment's cluster and namespace identities. " +
			"An optional independently retained identity is checked against the live target. " +
			"This does not qualify inputs, reserve approval, fence effects or deploy anything.",
		Example: "codefly composition --workspace . inspect-local-target local",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			current, err := resources.LoadWorkspaceFromDir(cmd.Context(), *workspace)
			if err != nil {
				return err
			}
			env := current.FindEnvironment(args[0])
			if env == nil {
				return fmt.Errorf("environment %q is not declared in %s", args[0], resources.WorkspaceConfigurationName)
			}
			var inspected *deployments.KubernetesTargetInspection
			if cmd.Flags().Changed("expected-identity") {
				inspected, err = deployments.RecheckLocalKubernetesTarget(cmd.Context(), env, expected)
			} else {
				inspected, err = deployments.InspectLocalKubernetesTarget(cmd.Context(), env)
			}
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(inspected)
		},
	}
	command.Flags().StringVar(&expected, "expected-identity", "", "Independently retained target binding digest; mismatch refuses the check")
	return command
}
