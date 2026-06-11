package companion

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/spf13/cobra"
)

// PushCmd pushes already-built companion images to the registry.
// Equivalent to running `docker push codeflydev/<name>:<version>`,
// but reads the version from info.codefly.yaml so the human can't
// fat-finger a tag mismatch.
//
// This is a pure publish step — it does not rebuild. Pair with
// `companion build --push` if you want one-shot build+push.
var PushCmd = &cobra.Command{
	Use:   "push <name>",
	Short: "Push a built companion image to its registry",
	Long: `Push uses the version in <core>/companions/<name>/info.codefly.yaml
to compute the tag, then runs "docker push codeflydev/<name>:<version>".

The image must already exist locally. Build it first with
"codefly companion build <name>".`,
	Args: cobra.ExactArgs(1),
	Run:  runPush,
}

func init() {
	PushCmd.Flags().String("core-dir", "", "Path to the core directory (default: walk up from cwd)")
}

func runPush(cmd *cobra.Command, args []string) {
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
	cwd, err := os.Getwd()
	if err != nil {
		cli.Error("cannot read working directory: %v", err)
		cli.ExitError()
	}
	coreDir := coreDirFlag
	if coreDir == "" {
		coreDir = FindCompanionsRoot(cwd)
	}

	c, err := LoadCompanion(filepath.Join(coreDir, "companions", args[0]))
	if err != nil {
		cli.Error("cannot load companion %q: %v", args[0], err)
		cli.ExitError()
	}
	fmt.Printf("==> Pushing %s\n", c.Tag())
	if err := pushImage(c.Tag()); err != nil {
		cli.Error("push failed: %v", err)
		cli.ExitError()
	}
	fmt.Printf("    pushed %s\n", c.Tag())
}
