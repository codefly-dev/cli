package generate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
var protoLocal bool
var protoTemplate string

// pinnedProtocPlugins are the locally-installed code generators the
// --local path uses, pinned so regeneration is byte-reproducible. Bump
// these in lockstep with go.mod. Mirrors the versions that
// core/generated/generate.sh installed (which this command replaces).
var pinnedProtocPlugins = []struct{ bin, mod string }{
	{"protoc-gen-go", "google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11"},
	{"protoc-gen-go-grpc", "google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.1"},
	{"protoc-gen-grpc-gateway", "github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@v2.29.0"},
	{"protoc-gen-connect-go", "connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.20.0"},
	{"goimports", "golang.org/x/tools/cmd/goimports@latest"},
}

// ProtoCmd generates code from local proto files.
var ProtoCmd = &cobra.Command{
	Use:   "proto",
	Short: "generate Go/Python code from local proto files",
	Long: `Generate code from local proto files without pushing to buf.build first.

Two modes:
  (default) Docker companion image — runs buf inside codeflydev/proto, using
            the buf.gen.yaml in the --proto directory (Go+gRPC via BSR remote
            plugins, plus gateway/connect/openapi/TS). Needs Docker.
  --local   Locally-installed, version-pinned plugins (protoc-gen-go,
            -go-grpc, -grpc-gateway, -connect-go) + goimports. Offline and
            byte-reproducible. Replaces core/generated/generate.sh.

Examples:
  codefly generate proto --proto ../proto --output ./generated
  codefly generate proto --proto ../proto --output ./generated --local
  codefly generate proto --proto ../proto --output . --local --template buf.gen.local.yaml
`,
	Run: func(cmd *cobra.Command, args []string) {
		ctx, done := common.NewContext()
		defer done()

		var err error
		if protoLocal {
			err = generateProtoLocal(ctx, protoDir, outputDir, protoTemplate)
		} else {
			err = generateProtoCode(ctx, protoDir, outputDir)
		}
		cli.ExitOnError(err, "Cannot generate proto code")
		cli.Header(1, "Proto code generated successfully!")
		cli.Done()
	},
}

// generateProtoLocal regenerates Go bindings with locally-installed,
// version-pinned plugins (no Docker, no BSR remote). It is a faithful Go
// port of core/generated/generate.sh: ensure plugins are installed, run
// `buf generate <proto> --template <template>` from the output dir, then
// goimports the result. The output dir must contain the template
// (default buf.gen.local.yaml).
func generateProtoLocal(ctx context.Context, protoDir, outputDir, template string) error {
	w := wool.Get(ctx).In("generateProtoLocal")

	protoDir, err := shared.SolvePath(protoDir)
	if err != nil {
		return w.Wrapf(err, "cannot solve proto path")
	}
	outputDir, err = shared.SolvePath(outputDir)
	if err != nil {
		return w.Wrapf(err, "cannot solve output path")
	}
	if template == "" {
		template = "buf.gen.local.yaml"
	}
	if ok, _ := shared.FileExists(ctx, filepath.Join(outputDir, template)); !ok {
		return w.NewError("template %s not found in output directory: %s", template, outputDir)
	}

	// $(go env GOPATH)/bin must be on PATH so buf can exec the plugins we
	// install below.
	gopath, err := exec.CommandContext(ctx, "go", "env", "GOPATH").Output()
	if err != nil {
		return w.Wrapf(err, "cannot read GOPATH")
	}
	binDir := filepath.Join(string(trimNL(gopath)), "bin")
	env := append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cli.Info("Ensuring pinned codegen plugins are installed")
	for _, p := range pinnedProtocPlugins {
		if _, lookErr := exec.LookPath(p.bin); lookErr == nil {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(binDir, p.bin)); statErr == nil {
			continue
		}
		cli.Info("  go install %s", p.mod)
		if err := runDir(ctx, env, "", "go", "install", p.mod); err != nil {
			return w.Wrapf(err, "install %s", p.mod)
		}
	}

	cli.Info("buf generate (local plugins) from %s", protoDir)
	if err := runDir(ctx, env, outputDir, "buf", "generate", protoDir, "--template", template); err != nil {
		return w.Wrapf(err, "buf generate")
	}

	cli.Info("goimports")
	if err := runDir(ctx, env, outputDir, "goimports", "-w", "go"); err != nil {
		// Non-fatal: import grouping is cosmetic, the bindings are valid.
		cli.Warning("goimports failed (non-fatal): %v", err)
	}
	return nil
}

// runDir runs name+args in dir (cwd when empty) with env, streaming output.
func runDir(ctx context.Context, env []string, dir, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		c.Dir = dir
	}
	c.Env = env
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
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

func init() {
	ProtoCmd.Flags().StringVar(&protoDir, "proto", "", "path to proto source directory (required)")
	ProtoCmd.Flags().StringVar(&outputDir, "output", "", "path to output directory with buf.gen.yaml (required)")
	ProtoCmd.Flags().BoolVar(&protoLocal, "local", false, "Generate Go bindings with locally-installed, version-pinned plugins (offline, reproducible) instead of the Docker companion")
	ProtoCmd.Flags().StringVar(&protoTemplate, "template", "", "buf template filename inside --output for --local mode (default: buf.gen.local.yaml)")
	_ = ProtoCmd.MarkFlagRequired("proto")
	_ = ProtoCmd.MarkFlagRequired("output")
}
