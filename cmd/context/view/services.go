package _view

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/templates"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ServicesCmd represents the run command
var ServicesCmd = &cobra.Command{
	Use:   "services",
	Short: "View services",
	Run: func(cmd *cobra.Command, args []string) {
		viewServices()
	},
}

func viewServices() {
	// Workspace
	ctx := context.Background()

	provider := wool.New(ctx, configurations.CLI.AsResource())

	provider.WithLogger(common.CLI())
	defer provider.Done()

	ctx = provider.Inject(ctx)
	app := common.Application(ctx)
	if len(app.Services) == 0 {
		cli.Header(2, "No services found")
		cli.Exit()
	}
	cli.Header(1, "Services in application <{{.Name}}>", app)

	active := app.ActiveService(ctx)

	if active == nil {
		cli.Header(2, "No active service")
		cli.Header(2, "Services:")
		for _, other := range app.Services {
			cli.Header(2, "<{{.Name}}>", other)
		}
		return
	}

	cli.Header(2, "Active: <{{.Other.Active}}>", templates.New().With("Active", active))

	var others []string
	for _, other := range app.Services {
		if shared.PointerEqual(active, other.Name) {
			continue
		}
		others = append(others, other.Name)
	}
	if len(others) == 0 {
		return
	}
	cli.Header(2, "Others:")
	for _, other := range others {
		cli.Header(2, "<{{.Other.Name}}>", templates.New().With("Name", other))
	}
}
