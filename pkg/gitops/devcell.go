package gitops

// A dev deployment into a hosted environment carries the environment's cell
// file with it: a cell admits only the builds that file names, and a
// validation cell takes that approval from the branch head's file. The file is
// re-derived from the whole workspace, so before a dev deployment commits it,
// the re-derived file must approve nothing the branch's file does not already
// approve except the one service's new build.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops/cellfile"
	"github.com/codefly-dev/core/resources"
)

// devCell is the cell file a dev deployment starts from: the one the branch
// approves (the workspace's HEAD), or, in a workspace that is not a git
// checkout, the one on disk.
type devCell struct {
	path   string
	origin string
	file   *cellfile.File
}

// loadDevCell reads the cell file a dev deployment into a hosted environment
// starts from, and refuses when there is none to start from.
func loadDevCell(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) (devCell, error) {
	path, err := CellPath(workspace, env)
	if err != nil {
		return devCell{}, err
	}
	rel, err := filepath.Rel(workspace.Dir(), path)
	if err != nil {
		return devCell{}, err
	}
	renderFirst := fmt.Sprintf("render the environment first: codefly deploy gitops render --all --env %s", env.Name)
	cell := devCell{path: path}
	var data []byte
	if _, gitErr := gitCommand(ctx, workspace.Dir(), "rev-parse", "--is-inside-work-tree"); gitErr == nil {
		cell.origin = "committed"
		data, err = gitCommandBytes(ctx, workspace.Dir(), "show", "HEAD:./"+filepath.ToSlash(rel))
		if err != nil {
			return devCell{}, fmt.Errorf("environment %s's cell file %s is not committed, so the branch approves no build to replace (%w): commit the render that wrote it, or %s", env.Name, rel, err, renderFirst)
		}
	} else {
		cell.origin = "on-disk"
		data, err = os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return devCell{}, fmt.Errorf("environment %s has no cell file at %s: %s", env.Name, rel, renderFirst)
		}
		if err != nil {
			return devCell{}, fmt.Errorf("read environment %s's cell file: %w", env.Name, err)
		}
	}
	if cell.file, err = cellfile.Parse(data); err != nil {
		return devCell{}, fmt.Errorf("environment %s's %s cell file %s cannot be read: %w: %s", env.Name, cell.origin, rel, err, renderFirst)
	}
	return cell, nil
}

// deriveDevCell re-derives the environment's cell file from the workspace and
// refuses it, naming what moved, when it differs from the starting file by
// anything but the image digests of service's own workloads and the source
// revision: anything else is a render or a configuration change the branch
// does not carry, which the dev deployment's commit would approve with it.
// Given the images the dev build produced, only digests of those repositories
// may move; before the build, when they are not known yet, any of the
// service's may.
func deriveDevCell(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, start devCell, service string, images []string) (cellfile.File, CellResult, error) {
	derived, result, err := deriveCell(ctx, workspace, env)
	if err != nil {
		return cellfile.File{}, CellResult{}, fmt.Errorf("environment %s's cell file cannot be re-derived, and its cell admits only the builds that file names: %w", env.Name, err)
	}
	var repositories map[string]bool
	if images != nil {
		repositories = make(map[string]bool, len(images))
		for _, image := range images {
			repository, _, splitErr := splitPinnedImage(image)
			if splitErr != nil {
				return cellfile.File{}, CellResult{}, splitErr
			}
			repositories[repository] = true
		}
	}
	moved, err := cellMovesBeyond(start.file, &derived, service, repositories)
	if err != nil {
		return cellfile.File{}, CellResult{}, err
	}
	if len(moved) > 0 {
		return cellfile.File{}, CellResult{}, fmt.Errorf(
			"re-deriving environment %s's cell file from this workspace changes more than %s's images — %s differ from the %s cell file — so committing it would approve renders, dev deployments or configuration the branch does not carry: commit them first (after `codefly deploy gitops render --all --env %s` if the trees are stale), then deploy again",
			env.Name, service, strings.Join(moved, ", "), start.origin, env.Name)
	}
	return derived, result, nil
}

// cellMovesBeyond names where derived differs from start once the image
// digests of service's workloads (of repositories, when given) and the source
// revision are set aside.
func cellMovesBeyond(start, derived *cellfile.File, service string, repositories map[string]bool) ([]string, error) {
	before, err := cellWithoutDevMoves(start, service, repositories)
	if err != nil {
		return nil, err
	}
	after, err := cellWithoutDevMoves(derived, service, repositories)
	if err != nil {
		return nil, err
	}
	var moved []string
	beforeNamespaces, afterNamespaces := before.Namespaces, after.Namespaces
	before.Namespaces, after.Namespaces = nil, nil
	if !reflect.DeepEqual(before, after) {
		moved = append(moved, "the cell's host or source")
	}
	byName := func(namespaces []cellfile.Namespace) map[string]cellfile.Namespace {
		out := make(map[string]cellfile.Namespace, len(namespaces))
		for _, namespace := range namespaces {
			out[namespace.Name] = namespace
		}
		return out
	}
	was, is := byName(beforeNamespaces), byName(afterNamespaces)
	names := make([]string, 0, len(was)+len(is))
	for name := range was {
		names = append(names, name)
	}
	for name := range is {
		if _, ok := was[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if !reflect.DeepEqual(was[name], is[name]) {
			moved = append(moved, "namespace "+name)
		}
	}
	return moved, nil
}

// cellWithoutDevMoves is a copy of file with what a dev deployment of service
// moves cleared: its workloads' image digests (only of repositories, when
// given) and the source revision. The copy goes through the file's own writer
// and reader, so file is untouched.
func cellWithoutDevMoves(file *cellfile.File, service string, repositories map[string]bool) (*cellfile.File, error) {
	data, err := file.Encode()
	if err != nil {
		return nil, err
	}
	copied, err := cellfile.Parse(data)
	if err != nil {
		return nil, err
	}
	if copied.Source != nil {
		copied.Source.Revision = ""
	}
	for n := range copied.Namespaces {
		for w := range copied.Namespaces[n].Workloads {
			workload := &copied.Namespaces[n].Workloads[w]
			if workload.Service != service {
				continue
			}
			for _, containers := range [][]cellfile.Container{workload.Containers, workload.InitContainers} {
				for c := range containers {
					if repositories == nil || repositories[containers[c].Image.Repository] {
						containers[c].Image.Digest = ""
					}
				}
			}
		}
	}
	return copied, nil
}
