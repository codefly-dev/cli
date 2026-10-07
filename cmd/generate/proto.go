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
paths share) is refused, resolving symlinks so a symlinked component cannot
smuggle an output past it. buf generates into a staging tree of its own, mounted
separately, and the CLI publishes to each ` + "`out`" + ` itself — so what a run
emitted is known independently of what the outputs already held: a generation
that produced no file fails and leaves them untouched, and an unchanged
regeneration publishes byte-identical content and succeeds.
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
	RunE: func(_ *cobra.Command, _ []string) error {
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
	commonRoot, boundary, err := protoMountRoot(protoDir, outputDir, templateDir, outs, boundary)
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

	// Whether a container this run cannot remove is anyone's to collect; see
	// protoTeardownOutcome.
	recoverable := projectContainerRecovery(ctx)

	// Create Docker runner
	runner, err := runners.NewDockerEnvironment(ctx, image, protoDir, name)
	if err != nil {
		return w.Wrapf(err, "cannot create docker runner")
	}
	configureProtoRunnerUser(runner)

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
		if err = runner.Shutdown(cleanupCtx); err != nil {
			// The runner resolves its name and owns an immutable container ID.
			// Report that acquired ID, never the unresolved name supplied above.
			containerID, idErr := runner.ContainerID()
			if idErr != nil || containerID == "" {
				result = errors.Join(result, fmt.Errorf("cannot shut down generation container (identity unavailable): %w", err), idErr)
				return
			}
			result = errors.Join(result, protoTeardownOutcome(err, containerID, recoverable))
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
	if err = publishProtoOutputs(staging, boundary, outs, clean, templatePath); err != nil {
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

// configureProtoRunnerUser keeps private generated files owned by the invoking
// user. Linux preserves the creating UID on bind mounts, so root-owned 0700
// output directories cannot be published or cleaned up by a non-root caller.
// The companion sets HOME=/tmp to support arbitrary UIDs, as contract generation
// already requires. The mount qualification uses this same configuration.
func configureProtoRunnerUser(runner *runners.DockerEnvironment) {
	runner.WithUser(fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
}

// protoTeardownOutcome decides what a failed teardown means for the command.
//
// Removing the container is housekeeping, not generation, and this command has
// already arranged for a leftover to be collected: the container is marked
// ephemeral (WithEphemeral) and this process projected the recovery ownership
// that a later run's sweep matches. A removal that fails therefore leaves
// exactly the state an interrupted generate leaves — which
// TestAnInterruptedGenerateLeavesARecoverableContainer qualifies against a real
// daemon in CI. Folding it into the command's result reports a correct
// generation as a failed one, and a consumer's drift gate
// (`generate proto && git diff --exit-code`) cannot tell the two apart.
//
// So with ownership projected the failure is a warning that names the
// container, which keeps the leftover visible to whoever wants to remove it
// now rather than at the next run. Without ownership — outside a workspace, an
// unwritable home — nothing will collect it, and then the leak is the
// command's to report.
//
// What this is NOT is a blanket downgrade. The removal timeouts are core's,
// fixed at ten seconds per Docker call in its own fresh contexts
// (runners/dockerrun/docker_runner.go, Stop and remove), so nothing the CLI
// passes down shortens them: `context.WithoutCancel` carries no deadline.
// Measured idle on the companion image with both of this command's mounts,
// stop takes ~3.2s — the full SIGTERM grace, because the paused container's
// PID 1 does not handle it — and force-remove ~0.1s. Those idle measurements do
// not establish why removal sometimes exceeds its deadline. Raising the
// deadline is core's call, not this command's.
//
// One residual, stated rather than papered over: the cross-scope sweep that
// makes a leftover collectible whatever naming scope a later run picks needs a
// durable host identity, which core resolves and warns about separately and
// does not expose. With ownership projected but no durable identity, only the
// exact-scope sweep runs, and that one can miss a leftover — core says so when
// it happens.
func protoTeardownOutcome(err error, container string, recoverable bool) error {
	if !recoverable {
		return fmt.Errorf("cannot shut down the generation container %s, and this run projected no container recovery ownership, so nothing will collect it: %w", container, err)
	}
	cli.Warning("could not remove the generation container %s (%v); it is marked ephemeral and eligible for scoped recovery — `docker rm -f %s` removes it now; recovery across naming scopes requires a durable host identity", container, err, container)
	return nil
}

// protoMountRoot is the host directory mounted into the companion — the nearest
// common ancestor of the proto input, the --output directory, the template's
// directory and every `out` the template declares — along with the effective
// boundary no output may escape.
//
// The escape test is made on resolved paths. A lexical one is bypassed by a
// symlinked component: `out: ../link/gen` where `link` points outside the
// workspace reads as inside it, and then everything anchored on that
// conclusion — publication, and `clean: true` deleting the output first — acts
// outside the boundary the caller was promised. Only the components that exist
// can be symlinks, so an output buf has yet to create is resolved as far as it
// exists.
func protoMountRoot(protoDir, outputDir, templateDir string, outs []string, boundary string) (root string, effective string, err error) {
	root = commonAncestor(commonAncestor(protoDir, outputDir), templateDir)
	scope := "the workspace " + boundary
	if boundary == "" {
		boundary = root
		scope = boundary + ", the directory shared by --proto, --output and the template (no workspace owns " + protoDir + ")"
	}
	resolvedBoundary, err := resolveExistingPath(boundary)
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve %s: %w", boundary, err)
	}
	for _, out := range outs {
		resolvedOut, err := resolveExistingPath(out)
		if err != nil {
			return "", "", fmt.Errorf("cannot resolve generation output %s: %w", out, err)
		}
		if !pathWithin(resolvedBoundary, resolvedOut) {
			detail := ""
			if resolvedOut != filepath.Clean(out) {
				detail = fmt.Sprintf(" (it resolves to %s, through a symlink)", resolvedOut)
			}
			return "", "", fmt.Errorf("generation output %s lies outside %s%s; every `out` in the template must stay under it", out, scope, detail)
		}
		root = commonAncestor(root, out)
	}
	if root == "" || filepath.Dir(root) == root {
		return "", "", fmt.Errorf("proto input, output and template must share a directory below the filesystem root")
	}
	return root, boundary, nil
}

// resolveExistingPath returns p with every symlink along it resolved, keeping
// the part that does not exist yet. buf creates output directories, so a
// declared `out` need not exist — but only an existing component can be a
// symlink, so resolving the longest existing ancestor is what decides whether
// the path can leave a boundary.
func resolveExistingPath(p string) (string, error) {
	p = filepath.Clean(p)
	remainder := ""
	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, remainder), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", fmt.Errorf("no part of %s exists", p)
		}
		remainder = filepath.Join(filepath.Base(p), remainder)
		p = parent
	}
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
func publishProtoOutputs(staging, boundary string, outs []string, clean bool, templatePath string) error {
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
	// Everything from here mutates the host, and all of it goes through one
	// os.Root anchored at the boundary no output may escape. Resolving the
	// outputs against that boundary up front (protoMountRoot) establishes that
	// they are inside it; anchoring the writes there keeps it true, which a
	// check alone cannot — a symlink swapped in between the check and the
	// write would otherwise escape it, and `clean: true` deletes before it
	// writes.
	root, err := os.OpenRoot(boundary)
	if err != nil {
		return fmt.Errorf("cannot open the generation boundary %s: %w", boundary, err)
	}
	defer func() { _ = root.Close() }()
	rels := make([]string, len(outs))
	for i, out := range outs {
		rel, err := filepath.Rel(boundary, out)
		if err != nil || !pathWithin(boundary, out) {
			return fmt.Errorf("generation output %s is not under the boundary %s", out, boundary)
		}
		rels[i] = rel
	}

	if clean {
		for i, out := range outs {
			if err := cleanProtoOutput(root, rels[i], out, staging); err != nil {
				return err
			}
		}
	}
	for i, out := range outs {
		if len(emitted[i]) == 0 && !clean {
			continue
		}
		// A generated source tree is read by the tooling of whoever owns it,
		// so it keeps the usual directory mode rather than being narrowed.
		if err := root.MkdirAll(rels[i], 0o755); err != nil { //nolint:gosec // G301: a generated source tree its own tooling must read
			return fmt.Errorf("cannot create generation output %s (a path that leaves %s is refused): %w", out, boundary, err)
		}
		slot := protoStagingSlot(staging, i)
		for _, rel := range emitted[i] {
			if err := publishProtoFile(root, slot, out, rels[i], rel); err != nil {
				return err
			}
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
func cleanProtoOutput(root *os.Root, rel, out, staging string) error {
	if filepath.Clean(out) == filepath.Clean(staging) {
		return fmt.Errorf("generation output %s is the staging tree itself; cleaning it would delete what was generated", out)
	}
	keep := ""
	if pathWithin(out, staging) {
		inside, err := filepath.Rel(out, staging)
		if err != nil {
			return fmt.Errorf("cannot locate the staging tree under generation output %s: %w", out, err)
		}
		keep = strings.Split(inside, string(filepath.Separator))[0]
	}
	// An output that is the boundary itself has no name to remove within the
	// root, so it — like an output holding the staging tree — is emptied entry
	// by entry instead. Either way what buf's clean leaves behind is an output
	// holding only what this run emitted.
	if keep == "" && rel != "." {
		if err := root.RemoveAll(rel); err != nil {
			return fmt.Errorf("cannot clean generation output %s (a path that leaves the boundary is refused): %w", out, err)
		}
		return nil
	}
	dir, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot clean generation output %s: %w", out, err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return fmt.Errorf("cannot clean generation output %s: %w", out, err)
	}
	for _, entry := range entries {
		if entry.Name() == keep {
			continue
		}
		if err := root.RemoveAll(path.Join(rel, entry.Name())); err != nil {
			return fmt.Errorf("cannot clean generation output %s: %w", out, err)
		}
	}
	return nil
}

// publishProtoFile writes one generated file — named by its path relative to
// the staging slot — into the declared output, through the boundary root. A
// destination that already holds those bytes is left alone, so an unchanged
// replay changes neither content nor modification time.
//
// The root is what stops anything on disk redirecting the write. A directory
// in the published tree that is a symlink out of the boundary, or a symlinked
// component in the output's own path, would otherwise have publication
// overwrite a file no `out` names. os.Root resolves every name beneath the
// boundary and refuses one that leaves it, absolute symlinks included, so an
// escape is an error instead of a write. A symlink that stays inside the
// boundary is still followed: that is the tree's own business.
//
// The staging side is re-checked too: rel comes from a walk of the slot, which
// cannot ascend and skips everything that is not a regular file — WalkDir does
// not descend into symlinked directories — but the whole point of publishing
// from the host is that nothing the companion produced decides where the host
// writes.
func publishProtoFile(root *os.Root, slot, out, outRel, rel string) error {
	from := filepath.Join(slot, rel)
	if !pathWithin(slot, from) {
		return fmt.Errorf("generated file %q does not stay under the staging tree it was generated into", rel)
	}
	generated, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("cannot read generated file %s: %w", from, err)
	}
	to := path.Join(outRel, filepath.ToSlash(rel))
	published, readErr := root.ReadFile(to)
	if readErr == nil && bytes.Equal(published, generated) {
		return nil
	}
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot publish %s into %s (a path that leaves the boundary, such as a symlink pointing outside it, is refused rather than followed): %w", rel, out, readErr)
	}
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("cannot inspect generated file %s: %w", from, err)
	}
	if dir := path.Dir(to); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: a generated source tree its own tooling must read
			return fmt.Errorf("cannot create %s inside %s (a path that leaves the boundary is refused): %w", dir, out, err)
		}
	}
	if err := root.WriteFile(to, generated, info.Mode().Perm()); err != nil {
		return fmt.Errorf("cannot publish %s into %s (a path that leaves the boundary is refused): %w", rel, out, err)
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
