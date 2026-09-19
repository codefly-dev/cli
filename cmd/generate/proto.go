package generate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/companions/proto"
	runners "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var protoDir string
var outputDir string
var protoPaths []string

// ProtoCmd generates code from local proto files.
var ProtoCmd = &cobra.Command{
	Use:   "proto",
	Short: "Generate Go and Python bindings from local protobuf files",
	Long: `Generate code from local proto files without pushing to buf.build first.

Runs buf inside the versioned proto companion image, using the buf.gen.yaml in
the --proto directory: Go, gRPC, Connect, gateway, OpenAPI and TypeScript
outputs, then goimports over every Go output the template declares. Nothing
runs on the host but Docker, and the plugins and the formatter are the image's,
pinned by its tag, so two machines regenerate the same bytes.

Examples:
  codefly generate proto --proto ../proto --output ./generated
  codefly generate proto --proto ../proto --output ../code --path saas/v1/service.proto
`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, done := common.NewContext()
		defer done()
		ctx, stop := common.SignalContext(ctx)
		defer stop()

		if err := generateProtoCode(ctx, protoDir, outputDir); err != nil {
			return fmt.Errorf("cannot generate proto code: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		cli.Header(1, "Proto code generated successfully!")
		return nil
	},
}

func generateProtoCode(ctx context.Context, protoDir string, outputDir string) (result error) {
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

	w.Info("Using proto companion image", wool.Field("image", image.FullName()))

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

	projectContainerRecovery(ctx)

	// Create Docker runner
	runner, err := runners.NewDockerEnvironment(ctx, image, protoDir, name)
	if err != nil {
		return w.Wrapf(err, "cannot create docker runner")
	}

	// A proto-gen container holds no state worth preserving: it is created,
	// driven once and shut down in the defer below. Marking it ephemeral is what
	// makes a leaked one recoverable at all — the exact-scope sweep compares a
	// hash that includes the naming scope, which a later run may set differently
	// (--naming-scope, a non-local --env, --temporary-ports), while the
	// disposable sweep is keyed on the naming-scope-independent namespace. Only
	// a container whose owning process is gone is ever reaped, so a concurrent
	// generate is never disturbed.
	runner.WithEphemeral()

	// Mount the common ancestor so both proto and output paths are accessible.
	// Work from the proto dir where buf.gen.yaml lives — buf resolves output
	// paths relative to buf.gen.yaml's location.
	runner.WithMount(commonRoot, "/workspace")
	runner.WithWorkDir(containerProto)
	runner.WithPause()

	defer func() {
		cleanupCtx := context.WithoutCancel(ctx)
		if err := runner.Shutdown(cleanupCtx); err != nil {
			result = errors.Join(result, w.Wrapf(err, "cannot shutdown proto runner"))
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
	args := []string{"generate"}
	args = append(args, protoGenerationPathArgs(protoDir, false)...)
	proc, err = runner.NewProcess("buf", args...)
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot generate proto code")
	}

	// buf's Go is not the Go a repository commits: every consumer runs
	// goimports over it and gates its checked-in bindings on that shape. The
	// companion owns that pass and runs it in the image, so the tree it hands
	// back is the committed one. See core's proto.FormatGoOutputs.
	w.Info("Formatting generated Go...")
	if err = proto.FormatGoOutputs(ctx, runner, protoDir, "buf.gen.yaml", commonRoot, "/workspace"); err != nil {
		return w.Wrapf(err, "cannot format generated Go")
	}

	return nil
}

// commonAncestor returns the longest shared directory prefix of two absolute paths.
func commonAncestor(a, b string) string {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	partsA := splitPath(a)
	partsB := splitPath(b)
	n := min(len(partsB), len(partsA))
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

func protoGenerationPathArgs(protoDir string, absolute bool) []string {
	args := make([]string, 0, len(protoPaths)*2)
	for _, path := range protoPaths {
		if absolute {
			path = filepath.Join(protoDir, path)
		}
		args = append(args, "--path", path)
	}
	return args
}

func init() {
	ProtoCmd.Flags().StringVar(&protoDir, "proto", "", "path to proto source directory (required)")
	ProtoCmd.Flags().StringVar(&outputDir, "output", "", "path to output directory with buf.gen.yaml (required)")
	ProtoCmd.Flags().StringSliceVar(&protoPaths, "path", nil, "limit generation to a proto-relative path (repeatable)")
	_ = ProtoCmd.MarkFlagRequired("proto")
	_ = ProtoCmd.MarkFlagRequired("output")
}
