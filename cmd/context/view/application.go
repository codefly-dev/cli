package _view

import (
	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/cli/display"
	"github.com/codefly-dev/core/shared"
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
	ctx := shared.NewContext()
	project := common.Project(ctx)

	active := project.ActiveApplication()
	var others []string
	for _, other := range project.Applications {
		if shared.PointerEqual(active, other.Name) {
			continue
		}
		others = append(others, other.Name)
	}
	shared.ExitIf(len(others) == 0, "No applications found")
	cli.Header(1, "Applications in project <{{.Project.Name}}>", display.New().WithProject(project))
	cli.Header(2, "Active: <{{.Active}}>", display.New().With("Active", active))
	if len(others) == 0 {
		return
	}
	for _, other := range others {
		cli.Header(2, "<{{.Other}}>", display.New().With("Other", other))
	}
}
