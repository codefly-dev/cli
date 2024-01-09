package _view

import (
	"context"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

// ApplicationsCmd represents the run command
var ApplicationsCmd = &cobra.Command{
	Use:   "applications",
	Short: "View applications",
	Run: func(cmd *cobra.Command, args []string) {
		ViewApplications(nil)
	},
}

type Details func(ctx context.Context, app *configurations.Application)

func ViewApplications(detail Details) {
	// Workspace
	ctx := context.Background()

	provider := wool.New(ctx, configurations.CLI.AsResource())

	provider.WithLogger(cli.GetLogger())
	defer provider.Done()

	ctx = provider.Inject(ctx)
	project := common.Project(ctx)

	active, err := project.LoadActiveApplication(ctx)
	cli.ExitOnError(err, "cannot load active application")
	var others []string
	for _, other := range project.Applications {
		if active.Name == other.Name {
			continue
		}
		others = append(others, other.Name)
	}
	cli.Header(1, "Applications in project <%s>", project.Name)
	cli.Header(2, "Active: <%s>", active.Name)
	if detail != nil {
		detail(ctx, active)
	}
	if len(others) == 0 {
		return
	}
	cli.Header(2, "Others:")
	for _, other := range others {
		cli.Header(2, "<%s>", other)
		if detail == nil {
			continue
		}
		app, err := project.LoadApplicationFromName(ctx, other)
		cli.ExitOnError(err, "cannot load application")
		detail(ctx, app)
	}
}
