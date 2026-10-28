package orchestration

import (
	"context"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
)

// moduleArtifactResolverFor is the acquisition half of core's per-load module
// artifact seam: core owns the declaration, the profile precedence, the
// collision checks and the typed identity it computes from the bytes; the
// selected module, the confined read and the provenance of what was read are
// the CLI's, because only the CLI knows how this invocation resolved that
// module.
//
// Each ordinary Load acquires afresh — core shares one acquisition for the
// repeated uses WITHIN a load and starts clean on the next one — so this
// resolver reads the workspace as the load sees it and never caches across
// loads. A later load may legitimately see changed bytes; nothing here freezes
// them, and nothing rereads the file at emission to prove they were stable.
func ModuleArtifactResolverFor(workspace *resources.Workspace) configurations.ModuleArtifactResolver {
	return func(ctx context.Context, ref *basev0.ConfigurationModuleArtifactReference) ([]byte, error) {
		return acquireModuleArtifact(ctx, workspace, ref)
	}
}

// acquireModuleArtifact reads the declared path out of the already selected
// module, under that module's directory and nowhere else.
//
// The confinement is os.Root, so a symlink, a `..` element or an absolute path
// cannot leave the selection even if it survived core's reference validation.
// os.Root alone is not enough, though: it still happily opens a FIFO and then
// blocks forever on the read, and it says nothing about what was opened. So the
// open is non-blocking, the mode is checked before a byte is read, and the same
// open file is stat'ed again afterwards — a file replaced or rewritten under
// the read refuses rather than delivering a mixture of two versions. That is a
// same-file observation, not a snapshot of the tree against arbitrary
// concurrent mutation.
func acquireModuleArtifact(ctx context.Context, workspace *resources.Workspace, ref *basev0.ConfigurationModuleArtifactReference) ([]byte, error) {
	if workspace == nil {
		return nil, fmt.Errorf("module artifacts require a selected workspace")
	}
	if err := resources.ValidateModuleArtifactReference(ref); err != nil {
		return nil, err
	}
	dir, _, err := composition.SelectedModuleDir(ctx, workspace, ref.Module)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot confine module artifact reads to module %s", ref.Module)
	}
	defer root.Close()
	file, err := root.OpenFile(ref.Path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open module artifact %q in module %s", ref.Path, ref.Module)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot inspect module artifact %q in module %s", ref.Path, ref.Module)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("module artifact %q in module %s must be a regular file", ref.Path, ref.Module)
	}
	// One byte over core's inclusive inline bound is read deliberately: core
	// refuses the oversize value by its own threshold, and reading no further
	// keeps an enormous file from being pulled into memory to be rejected.
	content, err := io.ReadAll(io.LimitReader(file, resources.FileCarrierThreshold+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read module artifact %q in module %s", ref.Path, ref.Module)
	}
	after, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("cannot re-inspect module artifact %q in module %s", ref.Path, ref.Module)
	}
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || int64(len(content)) != before.Size() {
		return nil, fmt.Errorf("module artifact %q in module %s changed while it was being read", ref.Path, ref.Module)
	}
	// And the SELECTION, re-asked around the read. The same-open-file checks
	// above only say the file did not move under us; they say nothing about
	// whether this is still the file the composition selects. A
	// re-materialization or an edited overlay between the open and here
	// re-points the module, and these bytes would then be delivered as the
	// selected module's while another directory is what the next load reads.
	// Re-resolving is cheap (no pull, no reload) and its refusal is the same
	// one as before the read.
	reselected, _, err := composition.SelectedModuleDir(ctx, workspace, ref.Module)
	if err != nil {
		return nil, err
	}
	if reselected != dir {
		return nil, fmt.Errorf("module %s was reselected while artifact %q was being read; resolve the composition again", ref.Module, ref.Path)
	}
	return content, nil
}

// deliveryBaseline is a phase's configuration payload as core must see it:
// every offer, in the order it was gathered, with nothing merged, deduplicated
// or reduced to a map first. A duplicate coordinate, an alias and a repeated
// pointer all stay visible, because the offer that would be LOST to precedence
// is exactly the one core has to refuse.
func deliveryBaseline(own *basev0.Configuration, workspace, dependencies []*basev0.Configuration) []*basev0.Configuration {
	baseline := make([]*basev0.Configuration, 0, len(workspace)+len(dependencies)+1)
	if own != nil {
		baseline = append(baseline, own)
	}
	baseline = append(baseline, workspace...)
	return append(baseline, dependencies...)
}

// detachConfigurations is the retained copy of a delivered payload: typed
// clones, so the generated artifact evidence travels with each value and a
// later load, filter or clone of the original slices cannot quietly rewrite
// what this service was delivered.
func detachConfigurations(confs []*basev0.Configuration) []*basev0.Configuration {
	detached := make([]*basev0.Configuration, 0, len(confs))
	for _, conf := range confs {
		if conf == nil {
			continue
		}
		detached = append(detached, proto.CloneOf(conf))
	}
	return detached
}
