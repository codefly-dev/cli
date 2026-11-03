package gitops

// --- The cell file a publication carries ---
//
// The cell file is the cell's APPROVED-BUILD list: the admission policy a cell
// enforces is derived from `namespaces[].workloads[].containers[].image`, so a
// pod whose build the file does not name is refused by the cluster. The render
// derives the file from every module's rendered tree (RenderCell), and a
// publication is already held to it — it may not remove what the delivery base
// branch's file admits (holdCellToBase). What was missing was the delivery
// itself: publish wrote each module's own path and left behind the file that
// gates it, so a round of promotions could merge every module's new build
// while the cell still approved only the builds before them, and each refused
// workload looked like a different application fault.
//
// WHO CARRIES IT. Every publication, not the host module's alone and not a
// step of its own:
//
//   - The file is whole or it is absent, never partial. It is derived from the
//     composition as a whole, and a render refuses to write it while any
//     composed module has no render for the environment
//     (refuseUnrenderedModules), so the file a publication finds in the
//     workspace describes every module the composition delivers. That is what
//     makes carrying it safe when one module is published alone.
//   - A module's own build is then approved by its own promotion. Were the
//     host module's publication the only carrier, a round that does not
//     republish the host would deliver builds nothing approves — the same
//     outage — and the ordering that would hide it (the host published first)
//     is one lane's workflow, never a rule of the delivery.
//   - A step of its own would be a second mechanism for what a publication
//     already does: clone the delivery repository, hold the file to the base
//     branch, write a signed commit, open the promotion. It would also be a
//     step to forget, which is the defect itself.
//
// Carrying it on every publication delivers the same bytes more than once in a
// round: every module of one `render --all` carries the one file that render
// derived, so their promotions agree and merge. A module re-rendered mid-round
// carries a different file, and git reports that conflict rather than quietly
// merging two approved-build lists.
//
// IT IS NOT SIGNED, and belongs in no signed set. The signed documents — the
// module manifests, the module envelope and the paired declarations — are
// carriers a host verifies against the release key before it admits a
// generation (publish_delivery.go), each one a document with a binding and a
// monotonic generation. The cell file has neither: it is regenerated whole,
// and it is read by the cell's infrastructure as values straight from the
// reviewed branch, by a consumer that takes no envelope and could verify none.
// Its custody is the signed promotion commit that carries it and the review
// that merges it — the same custody as every rendered manifest beside it. A
// signature nothing checks would only look like one.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/core/resources"
)

// publicationCell is the environment's cell file as a publication carries it:
// the workspace file the render wrote, read once, with the path it is
// delivered at.
type publicationCell struct {
	// source is the workspace file, and delivered its path in the delivery
	// repository (deliveryCellPath).
	source, delivered string
	// data is the file exactly as the render wrote it: what is delivered, and
	// what the reviewer reads. file is the same bytes through the file's own
	// reader, for the hold against the base branch's.
	data []byte
	file *cellfile.File
}

// publicationCellFile reads the cell file this publication carries, and
// answers nil when there is none: an environment with no host has no cell to
// describe, and a workspace with no cell file has none to deliver. Those are
// the same two cases a publication has nothing to hold to the base branch's
// file, so one answer serves the hold and the delivery.
func publicationCellFile(workspace *resources.Workspace, env *environments.Environment, gitopsPath string) (*publicationCell, error) {
	if env == nil || env.Host == nil {
		return nil, nil
	}
	source, err := CellPath(workspace, env)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read environment %s's cell file: %w", env.Name, err)
	}
	cell := &publicationCell{source: source, delivered: deliveryCellPath(gitopsPath, env.Name), data: data}
	if cell.file, err = cellfile.Parse(data); err != nil {
		return nil, fmt.Errorf("environment %s's cell file %s cannot be read, so it can neither be held to the delivery base branch's nor delivered: %w", env.Name, source, err)
	}
	return cell, nil
}

// carryInto writes the cell file into the promotion clone at the path the
// cell's infrastructure reads it from, beside the module trees it is derived
// from. The bytes are the render's own, verbatim: the file is one of the
// render's outputs, and a publication delivers what was rendered and reviewed
// rather than re-deriving it here.
func (cell *publicationCell) carryInto(repo string) error {
	// The directory is confined as every other delivery destination is (no
	// escape, no symbolic link traversed), and the file inside it is the one
	// name this package writes there; what stands at that name is held to a
	// regular file, so a link committed in the delivery repository is never
	// written through.
	directory, err := confinedJoin(repo, path.Dir(cell.delivered))
	if err != nil {
		return err
	}
	destination := filepath.Join(directory, cellfile.FileName)
	switch info, statErr := os.Lstat(destination); {
	case statErr == nil && !info.Mode().IsRegular():
		return fmt.Errorf("the delivery repository holds %s as %s and not a regular file, so the cell file is not written over it", cell.delivered, info.Mode().Type())
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("inspect the delivered cell file %s: %w", cell.delivered, statErr)
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create the delivery cell directory: %w", err)
	}
	// The same mode the render writes the file with (writeCell): git records
	// 100644 for either, since it keeps only the executable bit.
	if err = os.WriteFile(destination, cell.data, 0o600); err != nil {
		return fmt.Errorf("carry the cell file into the promotion: %w", err)
	}
	return nil
}

// cellNotice says why a publication to a hosted environment moves no
// approved-build list, so a promotion that cannot move it says so instead of
// looking complete. There are two such publications, and neither is a cell
// file left behind silently.
func cellNotice(env *environments.Environment, cell *publicationCell, restoreRevision, module string) string {
	if env == nil || env.Host == nil || (restoreRevision == "" && cell != nil) {
		return ""
	}
	if restoreRevision != "" {
		// A rollback restores the module's delivered tree from a revision; the
		// workspace's cell file names the builds rendered NOW, so delivering
		// it would approve the builds being rolled back AWAY from. The file is
		// derived from the rendered trees and never spliced from a delivered
		// one (refuseUnrenderedModules says why), so the restored build is
		// approved by rendering the module at the restored source and
		// publishing forward.
		return fmt.Sprintf("this rollback restores %s's delivered tree and moves no cell file: %s admits only the builds its cell file names, so if the file on the base branch does not name the restored build, render %s at the restored source and publish it forward",
			module, env.Name, module)
	}
	return fmt.Sprintf("environment %s declares a host and this checkout holds no cell file for it, so this promotion moves no approved-build list: render the composition for it (`codefly deploy gitops render --all --env %s`) and publish again",
		env.Name, env.Name)
}
