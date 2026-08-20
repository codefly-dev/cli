package cmd

import (
	"github.com/codefly-dev/cli/docs/runbooks"
	"github.com/spf13/cobra"
)

// init exposes each docs/runbooks entry as a Cobra additional help topic, so
// `codefly help <topic>` prints the runbook and the topics appear under
// "Additional help topics" in `codefly help`. A command with neither Run nor
// subcommands is treated by Cobra as a help topic, so configureSubcommandValidation
// (which only rewrites parent commands that have subcommands) leaves these alone.
func init() {
	for _, r := range runbooks.List() {
		full, err := runbooks.Get(r.Name)
		if err != nil {
			continue
		}
		RootCmd.AddCommand(&cobra.Command{
			Use:   r.Name,
			Short: r.Summary,
			Long:  full.Content,
		})
	}
}
