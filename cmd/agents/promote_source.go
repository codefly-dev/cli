package agents

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Kept as a migration diagnostic for scripts invoking the removed pin workflow.
var PromoteSourceCmd = &cobra.Command{
	Use:                "promote-source",
	Short:              "Report the replacement for static source-agent promotion",
	Deprecated:         "source agents are checked at runtime; use test source --agent to qualify an explicit artifact",
	DisableFlagParsing: true,
	RunE: func(_ *cobra.Command, _ []string) error {
		return fmt.Errorf("source-agent promotion no longer edits a CLI compatibility roster; use codefly test source --agent <publisher/name:version> to qualify an artifact")
	},
}
