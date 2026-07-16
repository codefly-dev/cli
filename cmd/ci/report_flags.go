package ci

import "github.com/spf13/cobra"

var (
	ciReportFormat string
	ciReportOutput string
)

func bindReportFlags(command *cobra.Command) {
	command.Flags().StringVar(&ciReportFormat, "format", "text", "Final CI report format: text or json")
	command.Flags().StringVar(&ciReportOutput, "output", ".codefly/ci", "Directory for Codefly CI reports and artifacts (relative to the workspace root)")
}
