package solutionrun

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/agents/helpers/code"
	"github.com/codefly-dev/core/resources"
)

// ErrFrontendInputsChanged rejects a bundle built while its selected inputs
// changed. The caller may reconcile again; this is not a successful delivery.
var ErrFrontendInputsChanged = errors.New("frontend inputs changed during staging or build; reconcile the current source")

// InputDigest uses the build's canonical selector, including local packages and
// named ignored tarballs. Watch events alone do not establish a changed build.
func (b LocalFrontend) InputDigest(ctx context.Context, module *resources.Module) (string, error) {
	sources, err := selectFrontendSources(ctx, module)
	if err != nil {
		return "", err
	}
	inputs, _, err := sources.digest(module, b.Docker)
	return inputs, err
}

func (s *frontendSources) stage(ctx context.Context, destination string, module *resources.Module, docker bool, expected string) error {
	if err := s.sync(ctx, destination); err != nil {
		return err
	}
	staged := *s
	staged.root = destination
	actual, _, err := staged.digest(module, docker)
	if err != nil {
		return err
	}
	if actual != expected {
		return ErrFrontendInputsChanged
	}
	return nil
}

// FrontendWatchInterest survives an invalid package graph. Its bounded tree is
// the same repository/component boundary localPackages enforces. Only outputs
// are pruned: tracked ignored files and named ignored tarballs are valid inputs.
// Git metadata uses exact files, including worktree-specific index locations.
func FrontendWatchInterest(ctx context.Context, module *resources.Module) (code.WatchTree, []string, error) {
	root := frontendRepository(module.Dir())
	repository := root != ""
	if !repository {
		root = module.Dir()
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return code.WatchTree{}, nil, err
	}
	component, err := filepath.EvalSymlinks(module.Dir())
	if err != nil {
		return code.WatchTree{}, nil, err
	}
	services, err := filepath.Rel(module.Dir(), module.ServicesDir())
	if err != nil {
		return code.WatchTree{}, nil, err
	}
	bundle := rootRelative(root, filepath.Join(component, services, filepath.FromSlash(FrontendBundleDir)))
	tree := code.WatchTree{Root: root, Exclude: func(p string) bool {
		return frontendBuildOutput(rootRelative(root, p), bundle)
	}}
	var files []string
	if repository {
		for _, name := range []string{"index", "info/exclude"} {
			output, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--path-format=absolute", "--git-path", name).Output()
			if err != nil {
				return tree, nil, fmt.Errorf("resolve frontend watch Git metadata %s: %w", name, err)
			}
			files = append(files, strings.TrimSpace(string(output)))
		}
	}
	return tree, files, nil
}
