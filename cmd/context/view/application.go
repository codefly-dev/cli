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

// ApplicationsCmd represents the run command
var ApplicationsCmd = &cobra.Command{
	Use:   "applications",
	Short: "View applications",
	Run: func(cmd *cobra.Command, args []string) {
		viewApplications()
	},
}

func viewApplications() {
	// Workspace
	ctx := context.Background()

	provider := wool.New(ctx, configurations.CLI.AsResource())

	provider.WithLogger(common.CLI())
	defer provider.Done()

	ctx = provider.Inject(ctx)
	project := common.Project(ctx)

	active := project.ActiveApplication()
	var others []string
	for _, other := range project.Applications {
		if shared.PointerEqual(active, other.Name) {
			continue
		}
		others = append(others, other.Name)
	}
	if len(others) == 0 {
		cli.Header(2, "No applications found")
		cli.Exit()
	}
	cli.Header(1, "Applications in project <{{.Project.Name}}>", templates.New().WithProject(project))
	cli.Header(2, "Active: <{{.Active}}>", templates.New().With("Active", active))
	if len(others) == 0 {
		return
	}
	for _, other := range others {
		cli.Header(2, "<{{.Other}}>", templates.New().With("Other", other))
	}
}
