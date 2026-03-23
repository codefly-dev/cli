package generate

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/companions/proto"
	runners "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var protoDir string
var outputDir string

// ProtoCmd generates code from local proto files using Docker
var ProtoCmd = &cobra.Command{
	Use:   "proto",
	Short: "generate Go/Python code from local proto files (uses Docker)",
	Long: `Generate code from local proto files using the codefly proto companion Docker image.
This allows you to regenerate proto code without pushing to buf.build first.

Example:
  codefly generate proto --proto ../proto --output ./generated
`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		err := generateProtoCode(ctx, protoDir, outputDir)
		cli.ExitOnError(err, "Cannot generate proto code")
		cli.Header(1, "Proto code generated successfully!")
		cli.Done()
	},
}

func generateProtoCode(ctx context.Context, protoDir string, outputDir string) error {
	w := wool.Get(ctx).In("generateProtoCode")

	// Resolve paths
	protoDir, err := shared.SolvePath(protoDir)
	if err != nil {
		return w.Wrapf(err, "cannot solve proto path")
	}

	outputDir, err = shared.SolvePath(outputDir)
	if err != nil {
		return w.Wrapf(err, "cannot solve output path")
	}

	// Check Docker is running
	if !runners.DockerEngineRunning(ctx) {
		return w.NewError("Docker is not running. Please start Docker first.")
	}

	// Get the companion image
	image, err := proto.CompanionImage(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot get companion image")
	}

	w.Info("Using proto companion image", wool.Field("image", fmt.Sprintf("%s:%s", image.Name, image.Tag)))

	// Ensure output directory exists
	_, err = shared.CheckDirectoryOrCreate(ctx, outputDir)
	if err != nil {
		return w.Wrapf(err, "cannot create output directory")
	}

	// Check if buf.gen.yaml exists in output dir
	bufGenPath := filepath.Join(outputDir, "buf.gen.yaml")
	if ok, _ := shared.FileExists(ctx, bufGenPath); !ok {
		return w.NewError("buf.gen.yaml not found in output directory: %s", outputDir)
	}

	// Create a unique container name
	name := fmt.Sprintf("proto-gen-%d", time.Now().UnixMilli())

	// Create Docker runner
	runner, err := runners.NewDockerEnvironment(ctx, image, protoDir, name)
	if err != nil {
		return w.Wrapf(err, "cannot create docker runner")
	}

	// Mount proto source and output directories
	runner.WithMount(protoDir, "/proto")
	runner.WithMount(outputDir, "/output")
	runner.WithWorkDir("/output")
	runner.WithPause()

	defer func() {
		err = runner.Shutdown(ctx)
		if err != nil {
			w.Warn("cannot shutdown runner", wool.ErrField(err))
		}
	}()

	err = runner.Init(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot init runner")
	}

	w.Info("Updating buf dependencies...")

	// Update buf dependencies
	proc, err := runner.NewProcess("buf", "dep", "update")
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot update buf dependencies")
	}

	w.Info("Generating proto code...")

	// Generate code - use local proto as input
	proc, err = runner.NewProcess("buf", "generate", "/proto")
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot generate proto code")
	}

	return nil
}

func init() {
	ProtoCmd.Flags().StringVar(&protoDir, "proto", "", "path to proto source directory (required)")
	ProtoCmd.Flags().StringVar(&outputDir, "output", "", "path to output directory with buf.gen.yaml (required)")
	_ = ProtoCmd.MarkFlagRequired("proto")
	_ = ProtoCmd.MarkFlagRequired("output")
}
