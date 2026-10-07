package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/shared"
	"github.com/spf13/cobra"
)

// The shells whose completion script this command writes.
const (
	shellBash       = "bash"
	shellZsh        = "zsh"
	shellFish       = "fish"
	shellPowerShell = "powershell"
)

var completionInstall bool

var CompletionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate or install shell completion scripts for Codefly",
	Long: `Generate the completion script for the given shell to stdout, or
write it to that shell's conventional location with --install.

  codefly completion zsh             # print to stdout
  codefly completion zsh --install   # install for the current user

--install replaces scripts/build/add_code_completion.sh.`,
	DisableFlagsInUseLine: true,
	ValidArgs:             []string{shellBash, shellZsh, shellFish, shellPowerShell},
	// Require exactly one of the valid shell args. Without this, bare
	// `codefly completion` indexed args[0] and panicked (index out of range).
	Args: cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()

		var buf bytes.Buffer
		var err error
		switch args[0] {
		case shellBash:
			err = cmd.Root().GenBashCompletion(&buf)
		case shellZsh:
			err = cmd.Root().GenZshCompletion(&buf)
		case shellFish:
			err = cmd.Root().GenFishCompletion(&buf, true)
		case shellPowerShell:
			err = cmd.Root().GenPowerShellCompletionWithDesc(&buf)
		default:
			return fmt.Errorf("unsupported shell type %q", args[0])
		}
		if err != nil {
			return fmt.Errorf("cannot generate completion script: %w", err)
		}

		if !completionInstall {
			if _, err = os.Stdout.Write(buf.Bytes()); err != nil {
				return fmt.Errorf("cannot write completion script: %w", err)
			}
			return nil
		}

		dest, err := completionInstallPath(args[0])
		if err != nil {
			return fmt.Errorf("cannot resolve completion install path: %w", err)
		}
		if err := shared.WriteFileAtomic(ctx, dest, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("cannot write completion file: %w", err)
		}
		cli.Info("Installed %s completion to %s", args[0], dest)
		return nil
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
	case shellZsh:
		omz := filepath.Join(home, ".oh-my-zsh", "completions")
		if info, statErr := os.Stat(filepath.Join(home, ".oh-my-zsh")); statErr == nil && info.IsDir() {
			return filepath.Join(omz, "_codefly"), nil
		}
		return filepath.Join(home, ".zsh", "completions", "_codefly"), nil
	case shellBash:
		return filepath.Join(home, ".local", "share", "bash-completion", "completions", "codefly"), nil
	case shellFish:
		return filepath.Join(home, ".config", shellFish, "completions", "codefly.fish"), nil
	default:
		return "", fmt.Errorf("--install is not supported for %s; redirect stdout to the right location manually", shell)
	}
}

func init() {
	CompletionCmd.Flags().BoolVar(&completionInstall, "install", false, "Write the completion script to the shell's conventional location instead of stdout")
}
