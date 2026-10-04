package generate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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

// protoStagingRoot is where the companion sees this run's staging tree, bound
// from a directory of its own rather than from inside the generation mount;
// see newProtoStaging.
const protoStagingRoot = "/staging"

// protoStagedTemplateName is the derived template the companion generates
// through; see protoStagingTemplate.
const protoStagedTemplateName = "buf.gen.staged.yaml"

// protoMountProbe is the file the host writes and the companion deletes to
// prove they share the staging tree. The tree is exclusive to the run, so
// nothing stale can be mistaken for it.
const protoMountProbe = ".codefly-mount-probe"

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
paths share) is refused. buf generates into a staging tree under that mount and
the CLI publishes to each ` + "`out`" + ` itself, so what a run emitted is known
independently of what the outputs already held: a generation that produced no
file fails and leaves them untouched, and an unchanged regeneration publishes
byte-identical content and succeeds.
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

	// buf generates into a staging tree and the CLI publishes from it, so what
	// this run emitted is knowable independently of what the outputs already
	// held. The tree is this run's alone and sits outside the generation
	// mount, where no `out` can name it.
	clean, err := protoTemplateClean(templatePath)
	if err != nil {
		return err
	}
	staging, err := newProtoStaging()
	if err != nil {
		return err
	}
	defer func() {
		if removeErr := os.RemoveAll(staging); removeErr != nil {
			result = errors.Join(result, w.Wrapf(removeErr, "cannot remove the generation staging directory %s", staging))
		}
	}()
	stagedTemplate, err := protoStagingTemplate(templatePath, outs, protoStagingRoot)
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(staging, protoStagedTemplateName), stagedTemplate, 0o600); err != nil {
		return w.Wrapf(err, "cannot write the staged generation template")
	}

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
	// Staging is mounted separately so it is reachable without being inside
	// any tree a template can declare as an output, and so `clean: true`
	// cannot delete the evidence publication reads.
	runner.WithMount(staging, protoStagingRoot)
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

	// Prove the companion's staging directory is the host's before anything
	// runs in it, so a mount that never reaches the host is reported as that
	// rather than as a generator that produced nothing.
	if err = verifyProtoStagingMount(ctx, protoRunnerCommand(runner), staging, protoStagingRoot, protoMountProbe); err != nil {
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

	// The input, template and path filters are absolute: a custom template may
	// live outside the proto directory, and the staged template always does.
	pathArgs := protoGenerationPathArgs(containerProto, true)
	args := make([]string, 0, 4+len(pathArgs))
	args = append(args, "generate", containerProto, "--template", path.Join(protoStagingRoot, protoStagedTemplateName))
	args = append(args, pathArgs...)
	proc, err = runner.NewProcess("buf", args...)
	if err != nil {
		return w.Wrapf(err, "cannot create process")
	}
	err = proc.Run(ctx)
	if err != nil {
		return w.Wrapf(err, "cannot generate proto code")
	}
	if err = publishProtoOutputs(ctx, staging, outs, clean, templatePath); err != nil {
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

// protoProcessRunner is what staging needs from the companion: the ability to
// start a process in it. The Docker environment the command drives satisfies
// it.
type protoProcessRunner interface {
	NewProcess(bin string, args ...string) (base.Proc, error)
}

// protoCommand runs one command inside the companion and reports whether it
// exited clean. Staging needs nothing else from the companion, so it takes
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

// Generation is staged, and the CLI — not buf — writes the declared outputs.
//
// buf syncs an output tree rather than rewriting it: it compares the bytes it
// generated against what is already under each `out` and writes only the files
// that are new or different. With `clean: true` it additionally prunes what it
// no longer generates, and still leaves byte-identical files untouched. So
// nothing observable about the output tree after a run distinguishes "the
// generator emitted exactly what was already there" from "the generator
// emitted nothing" or "the generation never reached the host" — and a file
// that was already on disk cannot be evidence that this run produced it.
//
// So buf generates into a staging tree instead, through a derived template
// that redirects every `out` into it, and the CLI publishes from there. The
// staged tree is what this run actually emitted, which makes the two questions
// the command has to answer separately answerable: an empty staging tree is a
// generation that produced nothing, and a tree that matches the declared
// output is an unchanged replay, which publishes byte-identically and
// succeeds (#885). Writing the consumer's tree from the host also removes the
// failure #836 was about — buf can no longer write an output the host cannot
// see, because buf no longer writes the output at all.
//
// buf's own `--output` cannot do this: it is prepended to each `out`, so an
// `out` reaching out of the template's directory (`../code/pkg/gen`, the
// go-grpc layout) resolves straight back out of the staging directory.
// Measured against companion 0.0.16: `-o /stage` with `out: ../sibling/gen`
// writes `/sibling/gen`, exactly where it writes without the flag. An
// absolute `out` lands where it says, and that is what the derived template
// carries.
// newProtoStaging creates the directory buf generates into: exclusive to this
// run, and empty, because that is what makes it evidence. os.MkdirAll would
// accept a directory that already exists, so a leftover from a killed run — or
// a run that started in the same millisecond — could supply files this run
// never generated, which is the one thing staging exists to rule out.
//
// It lives under the codefly home rather than inside the generation mount.
// An `out` can resolve to the mount root itself (`out: .` in a template that
// is its own output directory), and `clean: true` then deletes that tree —
// taking the staging directory with it, and with it the evidence publication
// reads. Somewhere no `out` can name is the only place it is safe. The codefly
// home rather than the OS temp directory because this path is bind-mounted,
// and the home is already a directory the CLI mounts from.
func newProtoStaging() (string, error) {
	root := filepath.Join(resources.CodeflyHomeDir(), "generate-proto")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", root, err)
	}
	staging, err := os.MkdirTemp(root, "staging-")
	if err != nil {
		return "", fmt.Errorf("cannot create a generation staging directory under %s: %w", root, err)
	}
	return staging, nil
}

func protoStagingSlot(staging string, out int) string {
	return filepath.Join(staging, strconv.Itoa(out))
}

// protoStagingTemplate is the caller's template with every `out` redirected
// into the companion's view of the staging tree, and nothing else changed: the
// plugins, their options, the managed-mode block and the version are the
// caller's, so what runs is the generation they declared.
//
// The slot an `out` is redirected to is its index in outs, so plugins sharing
// an output (protoc-gen-go, -go-grpc, -grpc-gateway and -connect-go all write
// the go-grpc layout's `../code/pkg/gen`) still generate into one directory,
// and one output's files are published to one place.
func protoStagingTemplate(templatePath string, outs []string, containerStaging string) ([]byte, error) {
	contents, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read generation template %s: %w", templatePath, err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return nil, fmt.Errorf("cannot parse generation template %s: %w", templatePath, err)
	}
	slots := make(map[string]string, len(outs))
	for i, out := range outs {
		slots[out] = path.Join(containerStaging, strconv.Itoa(i))
	}
	root := &document
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	plugins := protoYAMLField(root, "plugins")
	if plugins == nil || plugins.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("generation template %s declares no plugins", templatePath)
	}
	templateDir := filepath.Dir(templatePath)
	staged := 0
	for _, plugin := range plugins.Content {
		out := protoYAMLField(plugin, "out")
		if out == nil || strings.TrimSpace(out.Value) == "" {
			continue
		}
		slot, ok := slots[filepath.Join(templateDir, strings.TrimSpace(out.Value))]
		if !ok {
			return nil, fmt.Errorf("generation output %q in %s is not one of the outputs the template declares", out.Value, templatePath)
		}
		out.Kind, out.Tag, out.Style, out.Value = yaml.ScalarNode, "!!str", 0, slot
		staged++
	}
	if staged == 0 {
		return nil, fmt.Errorf("no plugin in %s declares an `out` to generate into", templatePath)
	}
	return yaml.Marshal(&document)
}

// protoYAMLField returns the value node of key in a mapping, whose Content
// alternates key and value.
func protoYAMLField(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// protoTemplateClean reports the template's document-level `clean`, which asks
// buf to delete each `out` before generating. Staging moves that decision to
// publication: buf would only ever clean the staging tree, so the CLI applies
// it to the declared output instead.
func protoTemplateClean(templatePath string) (bool, error) {
	contents, err := os.ReadFile(templatePath)
	if err != nil {
		return false, fmt.Errorf("cannot read generation template %s: %w", templatePath, err)
	}
	var document struct {
		Clean bool `yaml:"clean"`
	}
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return false, fmt.Errorf("cannot parse generation template %s: %w", templatePath, err)
	}
	return document.Clean, nil
}

// verifyProtoStagingMount proves the companion's staging directory is the
// host's, and writable through the mount, before any generation runs. A
// generation into a staging tree the host cannot read would otherwise surface
// as "the generator produced nothing", which names the wrong cause.
//
// The probe lives in the staging tree, which belongs to this run — never in a
// declared output, where a probe left behind by a killed run is exactly what a
// consumer's drift gate reports.
func verifyProtoStagingMount(ctx context.Context, companion protoCommand, hostStaging, containerStaging, probe string) (result error) {
	host := filepath.Join(hostStaging, probe)
	// Registered before the probe exists, so no path out of this function can
	// leave one behind.
	defer func() {
		if err := os.Remove(host); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, fmt.Errorf("cannot remove the mount probe %s: %w", host, err))
		}
	}()
	if err := os.WriteFile(host, nil, 0o600); err != nil {
		return fmt.Errorf("cannot write the mount probe in %s: %w", hostStaging, err)
	}
	if err := companion(ctx, "rm", "--", path.Join(containerStaging, probe)); err != nil {
		return fmt.Errorf("the companion cannot reach its staging directory %s (the host's %s) through the mount, so nothing it generates would reach the host: %w", containerStaging, hostStaging, err)
	}
	if _, err := os.Lstat(host); err == nil {
		return fmt.Errorf("the companion's %s is not the host's %s: what it writes there never reaches the host, so nothing generated would be published", containerStaging, hostStaging)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("cannot check the staging mount for %s: %w", hostStaging, err)
	}
	return nil
}

// protoStagedFiles returns what the companion generated into one staging slot,
// as paths relative to it. A slot buf never created is an empty result: the
// plugin writing there produced nothing.
func protoStagedFiles(slot string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(slot, func(file string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && file == slot {
				return filepath.SkipDir
			}
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(slot, file)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// publishProtoOutputs copies what this generation emitted into the outputs the
// template declares, and refuses a generation that emitted nothing.
//
// The refusal is decided from the staging tree alone, before anything is
// published, so a generation that produced no file leaves every declared
// output exactly as it found it — a file already on disk is never mistaken for
// one this run produced.
//
// Publication adds and overwrites; it does not prune. An `out` may be a broad
// source root holding handwritten files beside the generated ones (which is
// why core's FormatGoOutputs formats only files carrying the generated-code
// notice), and a generator that stops emitting a file leaves the old one in
// place exactly as buf does. `clean: true` is the caller asking for the
// opposite, and replaces the declared output with what this run emitted.
//
// A file whose bytes are already on disk is left untouched, mirroring buf's
// own sync: an unchanged replay then changes no content and no modification
// time, which is what a drift gate over a generated tree reads.
func publishProtoOutputs(ctx context.Context, staging string, outs []string, clean bool, templatePath string) error {
	emitted := make([][]string, len(outs))
	total := 0
	for i, out := range outs {
		files, err := protoStagedFiles(protoStagingSlot(staging, i))
		if err != nil {
			return fmt.Errorf("cannot inspect what was generated for %s: %w", out, err)
		}
		emitted[i] = files
		total += len(files)
	}
	if total == 0 {
		return fmt.Errorf("generation produced no file for any output declared by %s (%s); nothing was generated", templatePath, strings.Join(outs, ", "))
	}
	// Every destination is cleaned before anything is published, never
	// interleaved. Outputs nest — a plugin writing `nested/x.ts` into the slot
	// for `out: gen` publishes it to the same path `out: gen/nested` owns — so
	// cleaning one output after publishing another would delete files this run
	// had already published. With no copy yet made, the order of the cleans
	// cannot matter.
	if clean {
		for _, out := range outs {
			if err := cleanProtoOutput(out, staging); err != nil {
				return err
			}
		}
	}
	for i, out := range outs {
		if len(emitted[i]) == 0 && !clean {
			continue
		}
		if _, err := shared.CheckDirectoryOrCreate(ctx, out); err != nil {
			return fmt.Errorf("cannot create generation output %s: %w", out, err)
		}
		if err := publishProtoSlot(protoStagingSlot(staging, i), out, emitted[i]); err != nil {
			return err
		}
	}
	return nil
}

// cleanProtoOutput empties a declared output ahead of publication, which is
// what a template's `clean: true` asks for.
//
// It leaves a staging tree that happens to live inside the output alone.
// generateProtoCode cannot produce that — staging is created outside the
// generation mount, and every `out` is under it — but publication deleting its
// own evidence and then failing to read it is a trap worth closing in the
// function rather than only in its caller.
func cleanProtoOutput(out, staging string) error {
	if filepath.Clean(out) == filepath.Clean(staging) {
		return fmt.Errorf("generation output %s is the staging tree itself; cleaning it would delete what was generated", out)
	}
	if !pathWithin(out, staging) {
		if err := os.RemoveAll(out); err != nil {
			return fmt.Errorf("cannot clean generation output %s: %w", out, err)
		}
		return nil
	}
	rel, err := filepath.Rel(out, staging)
	if err != nil {
		return fmt.Errorf("cannot locate the staging tree under generation output %s: %w", out, err)
	}
	keep := strings.Split(rel, string(filepath.Separator))[0]
	entries, err := os.ReadDir(out)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot clean generation output %s: %w", out, err)
	}
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(out, entry.Name())); err != nil {
			return fmt.Errorf("cannot clean generation output %s: %w", out, err)
		}
	}
	return nil
}

// publishProtoSlot writes one output's generated files into it, through an
// os.Root rooted at the output.
//
// The root is what stops the output's own contents redirecting the write. A
// directory in the published tree that is a symlink out of the output —
// `gen/nested` pointing at somewhere else entirely — would otherwise have
// publication overwrite a file the template never declared, on a host path
// outside every `out`. os.Root resolves every name beneath the output and
// refuses one that leaves it, absolute symlinks included, so the escape is an
// error instead of a write. (A symlinked `out` itself is still honoured: that
// is the caller's own declaration, and buf followed it too.)
func publishProtoSlot(slot, out string, emitted []string) error {
	output, err := os.OpenRoot(out)
	if err != nil {
		return fmt.Errorf("cannot open generation output %s: %w", out, err)
	}
	defer func() { _ = output.Close() }()
	for _, rel := range emitted {
		if err := publishProtoFile(output, slot, out, rel); err != nil {
			return err
		}
	}
	return nil
}

// publishProtoFile writes one generated file, named by its path relative to
// the staging slot, into the declared output. A destination that already holds
// those bytes is left alone, so an unchanged replay changes neither content
// nor modification time.
//
// The staging side is re-checked too: rel comes from a walk of the slot, which
// cannot ascend and skips everything that is not a regular file — filepath
// .WalkDir does not descend into symlinked directories — but the whole point
// of publishing from the host is that nothing the companion produced decides
// where the host writes.
func publishProtoFile(output *os.Root, slot, out, rel string) error {
	from := filepath.Join(slot, rel)
	if !pathWithin(slot, from) {
		return fmt.Errorf("generated file %q does not stay under the staging tree it was generated into", rel)
	}
	generated, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("cannot read generated file %s: %w", from, err)
	}
	published, readErr := output.ReadFile(rel)
	if readErr == nil && bytes.Equal(published, generated) {
		return nil
	}
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot publish %s into %s (a path in the output that leaves it, such as a symlink pointing elsewhere, is refused rather than followed): %w", rel, out, readErr)
	}
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("cannot inspect generated file %s: %w", from, err)
	}
	if dir := filepath.Dir(rel); dir != "." {
		// A generated source tree is read by the tooling of whoever owns it,
		// so it keeps the mode the generator gave it rather than being
		// narrowed here.
		if err := output.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a generated source tree its own tooling must read
			return fmt.Errorf("cannot create %s inside %s (a path that leaves the output is refused): %w", dir, out, err)
		}
	}
	if err := output.WriteFile(rel, generated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("cannot publish %s into %s (a path that leaves the output is refused): %w", rel, out, err)
	}
	return nil
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
