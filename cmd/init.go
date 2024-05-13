package cmd

import (
	"context"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// InitCmd represents the install command
var InitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize codefly",
}

func init() {
	_, err := resources.Init(context.Background())
	cli.ExitOnError(err, "Cannot initialize codefly")
}
