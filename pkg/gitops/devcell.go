package gitops

// A dev deployment into a hosted environment carries the environment's cell
// file with it: a cell admits only the builds that file names. The file is
// re-derived from the environment's working renders, and publish delivers it
// held to the file the delivery base branch carries, so before a dev
// deployment re-derives it, the re-derived file must approve nothing the base
// branch's file does not already approve except the one service's new build.

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

// devCell is the cell file a dev deployment starts from: the one the
// environment's delivery base branch carries — the file publish holds the
// publication to — or, for an environment that declares no delivery
// repository, the one on disk.
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
	delivers, err := declaresDeliveryRepository(workspace, env)
	if err != nil {
		return devCell{}, err
	}
	if delivers {
		config, _, baseBranch, pathRoot, resolveErr := resolveGitops(workspace, env.Name, true)
		if resolveErr != nil {
			return devCell{}, resolveErr
		}
		delivered := deliveryCellPath(pathRoot, env.Name)
		cell.origin = fmt.Sprintf("the cell file %s on the delivery base branch %s", delivered, baseBranch)
		data, err = baseBranchFile(ctx, config.RepoURL, baseBranch, delivered)
		if errors.Is(err, errBaseFileAbsent) {
			return devCell{}, fmt.Errorf("environment %s's delivery base branch %s carries no cell file at %s, so it approves no build to replace: publish the environment's modules first (codefly deploy gitops publish <module> --env %s)", env.Name, baseBranch, delivered, env.Name)
		}
		if err != nil {
			return devCell{}, fmt.Errorf("environment %s's cell file is held to the one on its delivery base branch %s of %s, which could not be read, so nothing approves a build to replace: %w", env.Name, baseBranch, config.RepoURL, err)
		}
	} else {
		cell.origin = "the cell file on disk"
		data, err = os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return devCell{}, fmt.Errorf("environment %s has no cell file at %s: %s", env.Name, rel, renderFirst)
		}
		if err != nil {
			return devCell{}, fmt.Errorf("read environment %s's cell file: %w", env.Name, err)
		}
	}
	if cell.file, err = cellfile.Parse(data); err != nil {
		return devCell{}, fmt.Errorf("environment %s's starting cell file (%s) cannot be read: %w: %s", env.Name, cell.origin, err, renderFirst)
	}
	return cell, nil
}

// declaresDeliveryRepository reports whether env delivers to a gitops
// repository, its own or the workspace default, as publish resolves it.
func declaresDeliveryRepository(workspace *resources.Workspace, env *environments.Environment) (bool, error) {
	if env.Gitops != nil && strings.TrimSpace(env.Gitops.RepoURL) != "" {
		return true, nil
	}
	defaults, err := environments.WorkspaceGitops(workspace)
	if err != nil {
		return false, err
	}
	return defaults != nil && strings.TrimSpace(defaults.RepoURL) != "", nil
}

// errBaseFileAbsent is a delivery base branch that was read and holds no file
// at the path asked for, as opposed to one that could not be read at all.
var errBaseFileAbsent = errors.New("the delivery base branch holds no such file")

// baseBranchFile reads one file of branch as the delivery repository holds it
// now, through a throwaway shallow fetch: the same base a publish clones. The
// fetch never prompts: a repository this shell holds no credential for fails
// at once, naming git's reason, rather than waiting on a terminal.
func baseBranchFile(ctx context.Context, repository, branch, path string) ([]byte, error) {
	temp, err := os.MkdirTemp("", "codefly-delivery-base-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(temp) }()
	if _, err = gitCommand(ctx, temp, "init", "--quiet", "--bare"); err != nil {
		return nil, err
	}
	if _, err = gitCommand(ctx, temp, "check-ref-format", "--branch", branch); err != nil {
		return nil, fmt.Errorf("invalid delivery base branch %q: %w", branch, err)
	}
	if _, err = gitCommandWithEnv(ctx, temp, []string{"GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never"},
		"fetch", "--quiet", "--no-tags", "--depth=1", "--", repository, "refs/heads/"+branch); err != nil {
		return nil, err
	}
	data, err := gitCommandBytes(ctx, temp, "show", "FETCH_HEAD:"+path)
	if err != nil && gitSaysAbsent(err) {
		return nil, errBaseFileAbsent
	}
	return data, err
}

// deriveDevCell re-derives the environment's cell file from the workspace and
// refuses it, naming what moved, when it differs from the starting file by
// anything but the image digests of service's own workloads and the source
// revision: anything else is a render or a configuration change the branch
// does not carry, which the publication of the dev deployment would approve
// with it.
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
			"re-deriving environment %s's cell file from its working renders changes more than %s's images — %s differ from %s — so publishing it would approve renders, dev deployments or configuration the delivery base does not carry: publish them first (after `codefly deploy gitops render --all --env %s` if the renders are stale), then deploy again",
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
