package cmd

import (
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionworkspace "github.com/codefly-dev/core/actions/workspace"
	"github.com/codefly-dev/core/configurations"
	v1actions "github.com/codefly-dev/core/proto/v1/go/actions"
	v1base "github.com/codefly-dev/core/proto/v1/go/base"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// InitCmd represents the init command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "Init codefly global developer workspace",
	Run: func(cmd *cobra.Command, args []string) {
		initCodefly()
	},
}

func initCodefly() {
	ctx := shared.NewContext()
	// Check if codefly is already initialized
	if configurations.IsInitialized(ctx) && !override {
		cli.Warning(`codefly is already initialized.
Use --override to reinitialize codefly.`)
		if !override {
			return
		}
	}
	cli.Header(1, "Welcome to Codefly Demo 🪽!")

	cli.Header(2, "Let's start by creating an organization - don't worry you can change that easily later on.")

	orgName := models.Input("Organization name", "McFly.dev")
	orgDomain := models.Input("Organization domain", configurations.ToOrganizationDomain(orgName))

	org := &v1base.Organization{
		Name:   orgName,
		Domain: orgDomain,
	}

	action, err := actionworkspace.NewActionAddWorkspace(ctx, &v1actions.AddWorkspace{
		Organization: org,
		Name:         "default",
	})

	out, err := actions.Run(ctx, action)
	shared.UnexpectedExitOnError(err, "cannot create default workspace")
	_, err = actions.As[configurations.Workspace](out)
	shared.UnexpectedExitOnError(err, "cannot get default workspace")

	cli.Header(2, `✅ Created a default workspace configuration at ~/.codefly/codefly.yaml.`)

}

func init() {
	InitCmd.Flags().BoolVar(&override, "override", false, "Override existing configuration")
	RootCmd.AddCommand(InitCmd)
}
