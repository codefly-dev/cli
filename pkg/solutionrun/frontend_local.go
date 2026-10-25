package solutionrun

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// localFrontendBuildContext snapshots the same repository boundary used by the
// deployed builder, using Git's selection of current source files. This frontend's
// output and installed dependencies are excluded even when tracked, so a prior dist
// cannot hide a failed build or invalidate the content cache on every delivery.
func localFrontendBuildContext(ctx context.Context, module *resources.Module, scratch string) (string, string, error) {
	root := frontendRepository(module.Dir())
	repository := root != ""
	if !repository {
		root = module.Dir()
	}
	bundle, err := filepath.Rel(root, filepath.Join(module.ServicesDir(), filepath.FromSlash(FrontendBundleDir)))
	if err != nil {
		return "", "", err
	}
	var names []string
	if repository {
		command := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
		output, err := command.Output()
		if err != nil {
			return "", "", fmt.Errorf("select local frontend sources for %s: %w", module.Name, err)
		}
		names = strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	} else {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if frontendBuildOutput(relative, bundle) && entry.IsDir() {
				return filepath.SkipDir
			}
			if !entry.IsDir() {
				names = append(names, relative)
			}
			return nil
		})
		if err != nil {
			return "", "", fmt.Errorf("select local frontend sources for %s: %w", module.Name, err)
		}
	}
	component, err := filepath.Rel(root, module.Dir())
	if err != nil {
		return "", "", err
	}
	source, err := os.OpenRoot(root)
	if err != nil {
		return "", "", err
	}
	defer source.Close()
	destination := filepath.Join(scratch, "source")
	if err = os.MkdirAll(destination, 0o700); err != nil {
		return "", "", err
	}
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return "", "", err
		}
		if name == "" || frontendBuildOutput(name, bundle) {
			continue
		}
		info, err := source.Lstat(name)
		if os.IsNotExist(err) {
			continue // a tracked file deleted in the working tree
		}
		if err != nil {
			return "", "", err
		}
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("frontend of %s: source %s is not a regular file", module.Name, name)
		}
		data, err := source.ReadFile(name)
		if err != nil {
			return "", "", err
		}
		target := filepath.Join(destination, name)
		if err = os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", "", err
		}
		if err = os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return "", "", err
		}
	}
	return destination, filepath.ToSlash(component), nil
}

func frontendBuildOutput(path, bundle string) bool {
	path, bundle = filepath.ToSlash(path), filepath.ToSlash(bundle)
	if path == bundle || strings.HasPrefix(path, bundle+"/") {
		return true
	}
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		switch segment {
		case ".git", ".codefly", "node_modules":
			return true
		}
	}
	return false
}
