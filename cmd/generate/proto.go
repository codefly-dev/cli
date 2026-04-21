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

	// buf.gen.yaml lives in the proto dir — that's where buf runs from.
	bufGenPath := filepath.Join(protoDir, "buf.gen.yaml")
	if ok, _ := shared.FileExists(ctx, bufGenPath); !ok {
		return w.NewError("buf.gen.yaml not found in proto directory: %s", protoDir)
	}

	// Ensure output directory exists
	_, err = shared.CheckDirectoryOrCreate(ctx, outputDir)
	if err != nil {
		return w.Wrapf(err, "cannot create output directory")
	}

	// Find the common ancestor of proto and output dirs so that
	// buf.gen.yaml's relative output paths (e.g. "../code/pkg/gen") resolve
	// correctly inside the container.
	commonRoot := commonAncestor(protoDir, outputDir)
	if commonRoot == "" {
		return w.NewError("proto dir and output dir must share a common ancestor")
	}

	// Compute container-internal paths relative to the common root.
	relProto, _ := filepath.Rel(commonRoot, protoDir)
	containerProto := filepath.Join("/workspace", relProto)

	// Create a unique container name
	name := fmt.Sprintf("proto-gen-%d", time.Now().UnixMilli())

	// Create Docker runner
	runner, err := runners.NewDockerEnvironment(ctx, image, protoDir, name)
	if err != nil {
		return w.Wrapf(err, "cannot create docker runner")
	}

	// Mount the common ancestor so both proto and output paths are accessible.
	// Work from the proto dir where buf.gen.yaml lives — buf resolves output
	// paths relative to buf.gen.yaml's location.
	runner.WithMount(commonRoot, "/workspace")
	runner.WithWorkDir(containerProto)
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

	// Generate code from the proto dir — buf.gen.yaml is here, so relative
	// output paths like "../code/pkg/gen" resolve correctly within /workspace.
	proc, err = runner.NewProcess("buf", "generate")
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot generate proto code")
	}

	return nil
}

// commonAncestor returns the longest shared directory prefix of two absolute paths.
func commonAncestor(a, b string) string {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	partsA := splitPath(a)
	partsB := splitPath(b)
	n := len(partsA)
	if len(partsB) < n {
		n = len(partsB)
	}
	common := []string{}
	for i := 0; i < n; i++ {
		if partsA[i] != partsB[i] {
			break
		}
		common = append(common, partsA[i])
	}
	if len(common) == 0 {
		return ""
	}
	return filepath.Join(common...)
}

func splitPath(p string) []string {
	var parts []string
	for {
		dir, file := filepath.Split(p)
		if file != "" {
			parts = append([]string{file}, parts...)
		}
		if dir == p {
			// Root
			if dir != "" {
				parts = append([]string{dir}, parts...)
			}
			break
		}
		p = filepath.Clean(dir)
	}
	return parts
}

func init() {
	ProtoCmd.Flags().StringVar(&protoDir, "proto", "", "path to proto source directory (required)")
	ProtoCmd.Flags().StringVar(&outputDir, "output", "", "path to output directory with buf.gen.yaml (required)")
	_ = ProtoCmd.MarkFlagRequired("proto")
	_ = ProtoCmd.MarkFlagRequired("output")
}
