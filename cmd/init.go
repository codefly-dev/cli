package cmd

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/models"
	"github.com/codefly-dev/core/actions/actions"
	actionworkspace "github.com/codefly-dev/core/actions/workspace"
	"github.com/codefly-dev/core/configurations"
	v1actions "github.com/codefly-dev/core/generated/go/actions/v1"
	basev1 "github.com/codefly-dev/core/generated/go/base/v1"
	"github.com/codefly-dev/core/wool"
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
	ctx := context.Background()

	provider := wool.New(ctx, configurations.CLI.AsResource())

	provider.WithLogger(common.CLI())
	defer provider.Done()

	ctx = provider.WithContext(ctx)

	// Check if codefly is already initialized
	isInitialized, err := configurations.IsInitialized(ctx)

	cli.ExitOnError(err, "cannot check if codefly is initialized")
	if isInitialized && !override {
		cli.Warning(`codefly is already initialized.
Use --override to reinitialize codefly.`)
		if !override {
			return
		}
	}

	cli.Header(1, "Welcome to Codefly 🪽!")

	cli.Header(2, "Let's start by creating an organization - don't worry you can change that easily later on.")

	orgName := models.Input("Organization name", "McFly.dev")
	orgDomain := models.Input("Organization domain", configurations.ToOrganizationDomain(orgName))

	org := &basev1.Organization{
		Name:   orgName,
		Domain: orgDomain,
	}

	action, err := actionworkspace.NewActionAddWorkspace(ctx, &v1actions.AddWorkspace{
		Organization: org,
		Name:         "default",
	})

	out, err := actions.Run(ctx, action)
	cli.ExitOnError(err, "cannot create default workspace")
	_, err = actions.As[configurations.Workspace](out)
	cli.ExitOnError(err, "cannot get default workspace")

	cli.Header(2, `✅ Created a default workspace configuration at ~/.codefly/codefly.yaml.`)
}

func init() {
	RootCmd.AddCommand(InitCmd)
}
