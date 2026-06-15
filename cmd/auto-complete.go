package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

var completionInstall bool

var CompletionCmd = &cobra.Command{
	Use:                   "completion [bash|zsh|fish|powershell]",
	Short:                 "Generate (or --install) the shell completion script",
	Long: `Generate the completion script for the given shell to stdout, or
write it to that shell's conventional location with --install.

  codefly completion zsh             # print to stdout
  codefly completion zsh --install   # install for the current user

--install replaces scripts/build/add_code_completion.sh.`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
	// Require exactly one of the valid shell args. Without this, bare
	// `codefly completion` indexed args[0] and panicked (index out of range).
	Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	Run: func(cmd *cobra.Command, args []string) {
		var buf bytes.Buffer
		var err error
		switch args[0] {
		case "bash":
			err = cmd.Root().GenBashCompletion(&buf)
		case "zsh":
			err = cmd.Root().GenZshCompletion(&buf)
		case "fish":
			err = cmd.Root().GenFishCompletion(&buf, true)
		case "powershell":
			err = cmd.Root().GenPowerShellCompletionWithDesc(&buf)
		default:
			cli.Error("Unsupported shell type <%s>.", args[0])
			cli.ExitError()
		}
		cli.ExitOnError(err, "cannot generate completion script")

		if !completionInstall {
			_, err = os.Stdout.Write(buf.Bytes())
			cli.ExitOnError(err, "cannot write completion script")
			return
		}

		dest, err := completionInstallPath(args[0])
		cli.ExitOnError(err, "cannot resolve completion install path")
		cli.ExitOnError(os.MkdirAll(filepath.Dir(dest), 0o755), "cannot create completion directory")
		cli.ExitOnError(os.WriteFile(dest, buf.Bytes(), 0o644), "cannot write completion file")
		cli.Info("Installed %s completion to %s", args[0], dest)
	},
}

// completionInstallPath returns the conventional per-user completion file
// for shell. zsh prefers an oh-my-zsh custom completions dir when present
// (matching the old add_code_completion.sh), falling back to ~/.zsh/completions.
func completionInstallPath(shell string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch shell {
	case "zsh":
		omz := filepath.Join(home, ".oh-my-zsh", "completions")
		if info, statErr := os.Stat(filepath.Join(home, ".oh-my-zsh")); statErr == nil && info.IsDir() {
			return filepath.Join(omz, "_codefly"), nil
		}
		return filepath.Join(home, ".zsh", "completions", "_codefly"), nil
	case "bash":
		return filepath.Join(home, ".local", "share", "bash-completion", "completions", "codefly"), nil
	case "fish":
		return filepath.Join(home, ".config", "fish", "completions", "codefly.fish"), nil
	default:
		return "", fmt.Errorf("--install is not supported for %s; redirect stdout to the right location manually", shell)
	}
}

func init() {
	CompletionCmd.Flags().BoolVar(&completionInstall, "install", false, "Write the completion script to the shell's conventional location instead of stdout")
}
