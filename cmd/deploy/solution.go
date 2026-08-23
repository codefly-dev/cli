package deploy

import (
	"fmt"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/spf13/cobra"
)

// SolutionCmd packages a codefly:solution into an OCI artifact and renders its
// manifests into the owned gitops tree, driving the solution executor through
// the same promotable render pipeline services and modules use. The rendered
// tree is published and reconciled with the existing `deploy gitops` commands
// (keyed on the solution name), so a solution reaches an ArgoCD-synced namespace
// without a separate transport.
var SolutionCmd = &cobra.Command{
	Use:   "solution [name]",
	Short: "Package and render a solution into the gitops tree",
	Args:  cobra.ExactArgs(1),
	RunE: func(_ *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		cli.Init()
		cli.RegisterCleanup(services.ClearAgents)

		name := args[0]
		if solutionAgent == "" {
			return fmt.Errorf("--agent is required: the codefly:solution executor that packages and renders %s", name)
		}
		if solutionSource == "" {
			return fmt.Errorf("--source is required: the solution source directory to package")
		}
		if solutionReference == "" {
			return fmt.Errorf("--reference is required: the target OCI reference to push the package to")
		}

		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		env, err := orchestration.SelectEnvironment(workspace, envInput)
		if err != nil {
			return err
		}
		agent, err := resources.ParseAgent(ctx, resources.SolutionAgent, solutionAgent)
		if err != nil {
			return fmt.Errorf("cannot parse solution agent %q: %w", solutionAgent, err)
		}

		result, err := gitops.RenderSolution(ctx, &gitops.SolutionRenderRequest{
			Workspace:   workspace,
			Environment: env,
			Agent:       agent,
			Name:        name,
			Source:      solutionSource,
			Reference:   solutionReference,
			AppProject:  appProject,
			Values:      solutionValues,
		})
		if err != nil {
			return fmt.Errorf("cannot render solution: %w", err)
		}
		cli.Info("Rendered %s", result.Path)
		cli.Info("Digest %s", result.Inventory.Digest)
		printSizingReport(result.Sizing)
		cli.Header(1, "Solution render done!")
		return nil
	},
}

var (
	solutionAgent     string
	solutionSource    string
	solutionReference string
	solutionValues    map[string]string
)

func init() {
	SolutionCmd.Flags().StringVar(&envInput, "env", "local", "Environment to deploy the solution")
	SolutionCmd.Flags().StringVar(&appProject, "app-project", "", "AppProject contract used to validate cluster-scoped rendered resources")
	SolutionCmd.Flags().StringVar(&solutionAgent, "agent", "", "codefly:solution executor identity (publisher:name:version)")
	SolutionCmd.Flags().StringVar(&solutionSource, "source", "", "Solution source directory to package as an OCI artifact")
	SolutionCmd.Flags().StringVar(&solutionReference, "reference", "", "Target OCI reference to push the packaged solution to")
	SolutionCmd.Flags().StringToStringVar(&solutionValues, "values", nil, "Values passed to the solution executor's render (key=value)")
}
