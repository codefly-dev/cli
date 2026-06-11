package cmd

import (
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var CompletionCmd = &cobra.Command{
	Use:                   "completion [bash|zsh|fish|powershell]",
	Short:                 "Generate completion script",
	Long:                  "To load completions",
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	// Require exactly one of the valid shell args. Without this, bare
	// `codefly completion` indexed args[0] and panicked (index out of range).
	Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	Run: func(cmd *cobra.Command, args []string) {
		var err error
		switch args[0] {
		case "bash":
			err = cmd.Root().GenBashCompletion(os.Stdout)
		case "zsh":
			err = cmd.Root().GenZshCompletion(os.Stdout)
		case "fish":
			err = cmd.Root().GenFishCompletion(os.Stdout, true)
		case "powershell":
			err = cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
		default:
			cli.Error("Unsupported shell type <%s>.", args[0])
			cli.ExitError()
		}
		cli.ExitOnError(err, "cannot generate completion script")
	},
}
