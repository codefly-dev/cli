package generate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/companions/proto"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/base"
	runners "github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// protoContainerRoot is where the companion sees the mounted host tree.
const protoContainerRoot = "/workspace"

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
paths share) is refused. Before generating, each declared ` + "`out`" + ` is proved to
be the host directory it names, writable from inside the companion; afterwards,
the declared outputs must hold generated files. An unchanged regeneration
writes nothing and succeeds — buf syncs an output tree rather than rewriting it.
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
	containerProto := path.Join(protoContainerRoot, filepath.ToSlash(relProto))
	relTemplateDir, _ := filepath.Rel(commonRoot, templateDir)
	containerTemplateDir := path.Join(protoContainerRoot, filepath.ToSlash(relTemplateDir))

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
	runner.WithMount(commonRoot, protoContainerRoot)
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

	// Prove the companion writes the host's output tree before anything runs
	// in it. buf's output sync makes a generation that landed inside the
	// container indistinguishable, afterwards, from one that had nothing to
	// rewrite — so the mount is checked directly, and up front.
	if err = verifyProtoOutputMounts(ctx, protoRunnerCommand(runner), outs, commonRoot, protoContainerRoot, protoOutputProbe(name)); err != nil {
		return err
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
	if err = requireProtoOutputsPopulated(outs, templatePath); err != nil {
		return err
	}

	// buf's Go is not the Go a repository commits: every consumer runs
	// goimports over it and gates its checked-in bindings on that shape. The
	// companion owns that pass and runs it in the image, so the tree it hands
	// back is the committed one. See core's proto.FormatGoOutputs.
	w.Info("Formatting generated Go...")
	if err = proto.FormatGoOutputs(ctx, runner, templateDir, filepath.Base(templatePath), commonRoot, protoContainerRoot); err != nil {
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

// protoProcessRunner is what output validation needs from the companion: the
// ability to start a process in it. The Docker environment the command drives
// satisfies it.
type protoProcessRunner interface {
	NewProcess(bin string, args ...string) (base.Proc, error)
}

// protoCommand runs one command inside the companion and reports whether it
// exited clean. Validation needs nothing else from the companion, so it takes
// this rather than a runner: the mount contract is then exercised by a
// closure, and only the one adapter below depends on base.Proc.
type protoCommand func(ctx context.Context, bin string, args ...string) error

func protoRunnerCommand(runner protoProcessRunner) protoCommand {
	return func(ctx context.Context, bin string, args ...string) error {
		proc, err := runner.NewProcess(bin, args...)
		if err != nil {
			return err
		}
		return proc.Run(ctx)
	}
}

// protoOutputProbe names the file a run uses to prove its output mounts. The
// container name makes it unique to the run, so a probe some killed run left
// behind can never stand in for this one's.
func protoOutputProbe(run string) string {
	return ".codefly-mount-probe-" + run
}

// verifyProtoOutputMounts proves that every output the template declares is
// the host directory the CLI resolved, and that what the companion writes
// there reaches the host.
//
// This, and not an observed write, is what the command can actually check.
// buf syncs an output tree rather than rewriting it: it compares the bytes it
// generated against what is already under each `out` and writes only the
// files that are new or different — with `clean: true` it additionally deletes
// the files it no longer generates, and still leaves identical ones untouched.
// So an unchanged regeneration legitimately touches nothing (#885), and "did
// any file get written?" cannot separate that from a generation the host never
// received. Whether the companion's `out` is the host's directory *can* be
// answered directly, and it is the question #836 was really about: the host
// drops a uniquely named probe in each declared output, the companion deletes
// it, and the host checks every one is gone. An `out` the companion resolves
// inside its own filesystem, one outside the mount, and a mount the companion
// cannot write through all fail here — before any generation runs, so a
// template that cannot deliver its outputs never half-writes a tree.
//
// hostRoot is the host directory the companion mounts at containerRoot.
func verifyProtoOutputMounts(ctx context.Context, companion protoCommand, outs []string, hostRoot, containerRoot, probe string) (result error) {
	if len(outs) == 0 {
		return nil
	}
	// Installed before the first probe is written: a failure partway through
	// the loop below must not leave one behind either. A probe must not
	// outlive the check — a stray file under a generated output is exactly
	// what a consumer's drift gate reports.
	defer func() { result = errors.Join(result, removeProtoOutputProbes(outs, probe)) }()

	containerProbes := make([]string, 0, len(outs))
	for _, out := range outs {
		if !pathWithin(hostRoot, out) {
			return fmt.Errorf("generation output %s lies outside the companion mount %s; every `out` in the template must stay under it", out, hostRoot)
		}
		rel, err := filepath.Rel(hostRoot, out)
		if err != nil {
			return fmt.Errorf("cannot locate generation output %s under the companion mount %s: %w", out, hostRoot, err)
		}
		// The host owns the probe, so clearing it never depends on the user
		// the companion runs as, and the output directory a first generation
		// needs is created here — host-owned, and with the same mode the
		// command already gives --output — rather than by the container.
		if _, err := shared.CheckDirectoryOrCreate(ctx, out); err != nil {
			return fmt.Errorf("cannot create generation output %s: %w", out, err)
		}
		if err := os.WriteFile(filepath.Join(out, probe), nil, 0o600); err != nil {
			return fmt.Errorf("cannot write the mount probe in generation output %s: %w", out, err)
		}
		containerProbes = append(containerProbes, path.Join(containerRoot, filepath.ToSlash(rel), probe))
	}
	if err := companion(ctx, "rm", append([]string{"--"}, containerProbes...)...); err != nil {
		return fmt.Errorf("the companion cannot reach every output the template declares (%s) through the mount of %s, so buf would write them inside the container and the host would keep whatever tree it already has: %w", strings.Join(outs, ", "), hostRoot, err)
	}
	for i, out := range outs {
		_, err := os.Lstat(filepath.Join(out, probe))
		if err == nil {
			return fmt.Errorf("the companion's %s is not the host's %s: what it writes there never reaches the host, so generation would be discarded", containerProbes[i], out)
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("cannot check the generation output mount for %s: %w", out, err)
		}
	}
	return nil
}

// removeProtoOutputProbes clears this run's probe from every declared output,
// whatever the check did. A probe the companion already deleted is gone.
func removeProtoOutputProbes(outs []string, probe string) error {
	var errs []error
	for _, out := range outs {
		if err := os.Remove(filepath.Join(out, probe)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("cannot remove the mount probe from %s: %w", out, err))
		}
	}
	return errors.Join(errs...)
}

// requireProtoOutputsPopulated fails a generation that left every declared
// output empty. With the mounts proven by verifyProtoOutputMounts and buf
// exiting clean, an output tree holding files *is* the generated tree —
// written by this run, or already byte-identical to what this run generated.
// One holding nothing means no plugin produced anything, which is a failure,
// not a success with an empty result.
//
// Any output, not every output: a template may pair a plugin that generates
// for this input with one that legitimately has nothing to emit, the way
// openapiv2 emits nothing for a contract carrying no REST annotations.
func requireProtoOutputsPopulated(outs []string, templatePath string) error {
	if len(outs) == 0 {
		return nil
	}
	for _, out := range outs {
		populated := false
		err := filepath.WalkDir(out, func(file string, entry os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) && file == out {
					return filepath.SkipDir
				}
				return err
			}
			if entry.Type().IsRegular() {
				populated = true
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("cannot inspect generation output %s: %w", out, err)
		}
		if populated {
			return nil
		}
	}
	return fmt.Errorf("generation left every output declared by %s (%s) empty; nothing was generated", templatePath, strings.Join(outs, ", "))
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
