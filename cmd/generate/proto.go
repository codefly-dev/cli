package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var protoDir string
var outputDir string
var protoPaths []string
var protoTemplate string
var protoLocal bool

// ProtoCmd generates code from local proto files.
var ProtoCmd = &cobra.Command{
	Use:   "proto",
	Short: "Generate Go and Python bindings from local protobuf files",
	Long: `Generate code from local proto files without pushing to buf.build first.

Runs buf inside the versioned proto companion image, using the buf.gen.yaml in
the --proto directory, or an explicit --template relative to --output.
The companion mounts the nearest directory holding --proto, --output, the
template and every ` + "`out`" + ` the template declares, so outputs beside the proto
directory (out: ../code/pkg/gen) are written on the host. An ` + "`out`" + ` that
escapes the owning workspace (or, outside a workspace, the directory the named
paths share) is refused, and a run that writes no file under any declared
` + "`out`" + ` fails rather than reporting success.
--local selects --output/buf.gen.local.yaml, not execution on the host.
Go, gRPC, Connect, gateway, OpenAPI and TypeScript
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

	templatePath, err := resolveProtoTemplate(protoDir, outputDir, protoTemplate, protoLocal)
	if err != nil {
		return err
	}
	templateDir := filepath.Dir(templatePath)

	// Ensure output directory exists
	_, err = shared.CheckDirectoryOrCreate(ctx, outputDir)
	if err != nil {
		return w.Wrapf(err, "cannot create output directory")
	}

	// Mount inputs, the template and every output it declares together; buf
	// resolves output paths against its working directory, which is the
	// template's own directory. An `out` beside the proto directory
	// (`../code/pkg/gen`, the go-grpc layout) widens the mount to reach it:
	// anything buf writes outside the mount is written into the container and
	// discarded.
	outs, err := protoTemplateOutputs(templatePath)
	if err != nil {
		return err
	}
	boundary, err := protoMountBoundary(ctx, protoDir)
	if err != nil {
		return err
	}
	commonRoot, err := protoMountRoot(protoDir, outputDir, templateDir, outs, boundary)
	if err != nil {
		return err
	}

	// Compute container-internal paths relative to the common root.
	relProto, _ := filepath.Rel(commonRoot, protoDir)
	containerProto := filepath.Join("/workspace", relProto)
	relTemplateDir, _ := filepath.Rel(commonRoot, templateDir)
	containerTemplateDir := filepath.Join("/workspace", relTemplateDir)

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
	// Work from the template directory so custom output paths retain their meaning.
	runner.WithMount(commonRoot, "/workspace")
	runner.WithWorkDir(containerTemplateDir)
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
	proc, err := runner.NewProcess("buf", "dep", "update", containerProto)
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot update buf dependencies")
	}

	w.Info("Generating proto code...")

	// Snapshot the declared outputs so a generation that wrote nothing on the
	// host is an error, not "generated successfully".
	before, err := snapshotProtoOutputs(outs)
	if err != nil {
		return err
	}

	// The input and path filters are absolute because a custom template may
	// live outside the proto directory.
	pathArgs := protoGenerationPathArgs(containerProto, true)
	args := make([]string, 0, 4+len(pathArgs))
	args = append(args, "generate", containerProto, "--template", filepath.Base(templatePath))
	args = append(args, pathArgs...)
	proc, err = runner.NewProcess("buf", args...)
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot generate proto code")
	}
	if err = requireProtoOutputsWritten(outs, before, templatePath); err != nil {
		return err
	}

	// buf's Go is not the Go a repository commits: every consumer runs
	// goimports over it and gates its checked-in bindings on that shape. The
	// companion owns that pass and runs it in the image, so the tree it hands
	// back is the committed one. See core's proto.FormatGoOutputs.
	w.Info("Formatting generated Go...")
	if err = proto.FormatGoOutputs(ctx, runner, templateDir, filepath.Base(templatePath), commonRoot, "/workspace"); err != nil {
		return w.Wrapf(err, "cannot format generated Go")
	}

	return nil
}

// protoMountRoot is the host directory mounted into the companion: the nearest
// common ancestor of the proto input, the --output directory, the template's
// directory and every `out` the template declares. An `out` that resolves
// outside boundary is refused rather than widening the mount past it.
func protoMountRoot(protoDir, outputDir, templateDir string, outs []string, boundary string) (string, error) {
	root := commonAncestor(commonAncestor(protoDir, outputDir), templateDir)
	scope := "the workspace " + boundary
	if boundary == "" {
		boundary = root
		scope = boundary + ", the directory shared by --proto, --output and the template (no workspace owns " + protoDir + ")"
	}
	for _, out := range outs {
		if !pathWithin(boundary, out) {
			return "", fmt.Errorf("generation output %s lies outside %s; every `out` in the template must stay under it", out, scope)
		}
		root = commonAncestor(root, out)
	}
	if root == "" || filepath.Dir(root) == root {
		return "", fmt.Errorf("proto input, output and template must share a directory below the filesystem root")
	}
	return root, nil
}

// protoMountBoundary is the directory no template output may escape: the
// workspace owning the proto directory, or, outside any workspace, nothing
// beyond the directories the caller named (the empty string).
func protoMountBoundary(ctx context.Context, protoDir string) (string, error) {
	dir, err := resources.FindUpFrom[resources.Workspace](ctx, protoDir)
	if err != nil {
		return "", fmt.Errorf("cannot look up the workspace owning %s: %w", protoDir, err)
	}
	if dir == nil {
		return "", nil
	}
	return filepath.Clean(*dir), nil
}

// protoTemplateOutputs returns every `out` the buf template declares, as
// absolute host paths resolved against the template's directory, which is
// where buf resolves them. An absolute `out` is refused: it names a host path
// the companion cannot see, so buf would write it inside the container.
func protoTemplateOutputs(templatePath string) ([]string, error) {
	contents, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read generation template %s: %w", templatePath, err)
	}
	var document struct {
		Plugins []struct {
			Out string `yaml:"out"`
		} `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return nil, fmt.Errorf("cannot parse generation template %s: %w", templatePath, err)
	}
	templateDir := filepath.Dir(templatePath)
	seen := map[string]bool{}
	var outs []string
	for _, plugin := range document.Plugins {
		out := strings.TrimSpace(plugin.Out)
		if out == "" {
			continue
		}
		if filepath.IsAbs(out) {
			return nil, fmt.Errorf("generation output %q in %s is absolute; the companion resolves `out` inside its own filesystem, so it must be relative to the template", out, templatePath)
		}
		out = filepath.Join(templateDir, out)
		if !seen[out] {
			seen[out] = true
			outs = append(outs, out)
		}
	}
	sort.Strings(outs)
	return outs, nil
}

func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type protoOutputFile struct {
	modTime time.Time
	size    int64
}

// snapshotProtoOutputs records every regular file under the declared outputs.
// A missing output directory is an empty snapshot: buf creates it.
func snapshotProtoOutputs(outs []string) (map[string]protoOutputFile, error) {
	files := map[string]protoOutputFile{}
	for _, out := range outs {
		err := filepath.WalkDir(out, func(file string, entry os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && file == out {
					return filepath.SkipDir
				}
				return err
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files[file] = protoOutputFile{modTime: info.ModTime(), size: info.Size()}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("cannot inspect generation output %s: %w", out, err)
		}
	}
	return files, nil
}

// requireProtoOutputsWritten fails a generation whose template declares
// outputs but which left no new or rewritten file under any of them. buf
// rewrites every file it generates, so even a no-change regeneration moves
// modification times; an untouched tree means buf wrote somewhere the host
// cannot see.
func requireProtoOutputsWritten(outs []string, before map[string]protoOutputFile, templatePath string) error {
	if len(outs) == 0 {
		return nil
	}
	after, err := snapshotProtoOutputs(outs)
	if err != nil {
		return err
	}
	for file, now := range after {
		if was, ok := before[file]; !ok || !was.modTime.Equal(now.modTime) || was.size != now.size {
			return nil
		}
	}
	return fmt.Errorf("generation wrote no file under any output declared by %s (%s); nothing was regenerated", templatePath, strings.Join(outs, ", "))
}

func resolveProtoTemplate(protoDir, outputDir, template string, local bool) (string, error) {
	var templatePath string
	switch {
	case template != "":
		templatePath = template
		if !filepath.IsAbs(templatePath) {
			templatePath = filepath.Join(outputDir, templatePath)
		}
	case local:
		templatePath = filepath.Join(outputDir, "buf.gen.local.yaml")
	default:
		templatePath = filepath.Join(protoDir, "buf.gen.yaml")
	}
	info, err := os.Stat(templatePath)
	if err != nil {
		return "", fmt.Errorf("cannot read generation template %s: %w", templatePath, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("generation template is not a regular file: %s", templatePath)
	}
	return filepath.Clean(templatePath), nil
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
	ProtoCmd.Flags().StringVar(&protoTemplate, "template", "", "generation template, relative to --output or absolute; runs in its own directory")
	ProtoCmd.Flags().BoolVar(&protoLocal, "local", false, "select --output/buf.gen.local.yaml; plugins still run inside the pinned companion")
	_ = ProtoCmd.MarkFlagRequired("proto")
	_ = ProtoCmd.MarkFlagRequired("output")
}
