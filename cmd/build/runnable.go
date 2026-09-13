package build

import (
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/cli/cmd/common"
	runnableops "github.com/codefly-dev/cli/pkg/runnable"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
)

var runnableOutput string
var runnableJSON bool

var RunnableCmd = &cobra.Command{
	Use:   "runnable <name>",
	Short: "Build and verify a native Runnable package through its agent",
	Long: `Generate and prepare the loaded Runnable through its agent, package a native
archive, and verify its release descriptor and actual artifact digest. The output
directory must be new. The name is module/name or an unambiguous bare name.

The agent owns language tooling and launch information. This command does not
install a binding, invoke a task or build an image. Build-time service prerequisites
and internal library preparation are not yet supported.`,
	Example: "  codefly build runnable word-count\n  codefly build runnable backend/word-count --output=/tmp/word-count-build --json",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()
		workspace, err := common.LoadWorkspace(ctx)
		if err != nil {
			return err
		}
		r, err := workspace.FindRunnableByName(ctx, args[0])
		if err != nil {
			return err
		}
		output := runnableOutput
		if output == "" {
			output = filepath.Join(workspace.Dir(), ".codefly", "build", "runnables", r.Module(), r.Name, r.Version)
		}
		output, err = filepath.Abs(output)
		if err != nil {
			return err
		}
		pkg, err := runnableops.Build(ctx, workspace, r, output, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		if runnableJSON {
			encoded, marshalErr := protojson.MarshalOptions{Indent: "  ", UseProtoNames: true}.Marshal(pkg)
			if marshalErr != nil {
				return marshalErr
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Built Runnable %s/%s@%s\nPackage: %s\nDigest: %s\n", r.Module(), r.Name, r.Version, filepath.Join(output, "artifacts", runnableops.PackageFile), pkg.GetDigest())
		return err
	},
}

func init() {
	RunnableCmd.Flags().StringVar(&runnableOutput, "output", "", "New build directory (defaults to .codefly/build/runnables/module/name/version)")
	RunnableCmd.Flags().BoolVar(&runnableJSON, "json", false, "Emit the verified Runnable package descriptor as JSON")
}
