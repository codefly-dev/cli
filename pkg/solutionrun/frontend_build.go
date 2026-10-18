package solutionrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// FrontendBuilderImage is the image a component's frontend bundle is built
// in: Node 24, the line every remote's own CI builds with, pinned by digest so
// the build reads the same toolchain wherever it runs. The daemon's registry
// mirror serves it like any other base.
const FrontendBuilderImage = "docker.io/library/node:24-bookworm-slim@sha256:a5f4366e834695d5f363b920c104953a535d3a9864d1bdf74566a0ebe01daee5"

// FrontendProject is the npm project a component's frontend build runs in,
// slash-separated and relative to the module: the remote's own directory
// (fe-remote beside the services) when it holds a package.json, else the
// module root when that does — a component keeping one npm project for its
// remote and its tooling. Either one's build writes FrontendBundleDir.
func FrontendProject(module *resources.Module) (string, error) {
	if module == nil || module.Dir() == "" {
		return "", errors.New("a frontend build needs a component directory")
	}
	services, err := filepath.Rel(module.Dir(), module.ServicesDir())
	if err != nil || !filepath.IsLocal(services) {
		return "", fmt.Errorf("%s: the services directory is not inside the component", module.Name)
	}
	remote := path.Join(filepath.ToSlash(services), path.Dir(FrontendBundleDir))
	for _, project := range []string{remote, "."} {
		if info, err := os.Stat(filepath.Join(module.Dir(), filepath.FromSlash(project), "package.json")); err == nil && info.Mode().IsRegular() {
			return project, nil
		}
	}
	return "", fmt.Errorf("%s declares a frontend and holds no npm project to build it from: no package.json in %s or at the component root", module.Name, remote)
}

// FrontendSourceRoot is the directory a component's frontend build takes as
// its context: the repository the component is released from (the nearest
// directory above it holding .git, which a module cache keeps as a checkout
// does), so a remote's dependency on a package beside the component in that
// repository resolves exactly as in the repository's own CI; the component's
// own directory when it sits in no repository.
func FrontendSourceRoot(module *resources.Module) string {
	for dir := module.Dir(); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return module.Dir()
		}
		dir = parent
	}
}

// BuildFrontendBundle builds a component's frontend from its own source and
// writes exactly the bundle (FrontendBundleDir) into destination, which must
// not exist yet. The build runs in FrontendBuilderImage with `npm ci` and
// `npm run build`, on the copy of FrontendSourceRoot the builder takes as its
// context: nothing is written into the component's directory, which for a
// release is the immutable module cache. The builder sees no variable from
// this process, so an ASSET_PREFIX left in the shell cannot bake an origin
// into the remote.
func BuildFrontendBundle(ctx context.Context, module *resources.Module, destination string) error {
	project, err := FrontendProject(module)
	if err != nil {
		return err
	}
	root := FrontendSourceRoot(module)
	component, err := filepath.Rel(root, module.Dir())
	if err != nil {
		return err
	}
	services, err := filepath.Rel(module.Dir(), module.ServicesDir())
	if err != nil {
		return err
	}
	component = filepath.ToSlash(component)
	bundle := path.Join(component, filepath.ToSlash(services), FrontendBundleDir)
	dockerfile := fmt.Sprintf("FROM %s AS build\nWORKDIR /source\nCOPY . .\nWORKDIR /source/%s\nRUN npm ci && npm run build\nFROM scratch\nCOPY --from=build /source/%s/ /\n",
		FrontendBuilderImage, path.Join(component, project), bundle)
	if _, err = os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("frontend bundle destination %s must not exist", destination)
	}
	//nolint:gosec // G204: fixed docker executable, separate arguments, no shell; the Dockerfile arrives on stdin.
	command := exec.CommandContext(ctx, "docker", "build", "--file", "-", "--output", "type=local,dest="+destination, root)
	command.Stdin = strings.NewReader(dockerfile)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Run(); err != nil {
		return fmt.Errorf("build the frontend of %s in %s (npm ci && npm run build in %s): %w\n%s", module.Name, FrontendBuilderImage, path.Join(component, project), err, tail(output.String(), 40))
	}
	if _, err = os.Stat(filepath.Join(destination, FrontendBundleManifest)); err != nil {
		return fmt.Errorf("the frontend build of %s wrote no %s at the root of %s", module.Name, FrontendBundleManifest, bundle)
	}
	return nil
}

// tail keeps the last lines of a build log: where npm and the bundler say
// what failed.
func tail(log string, lines int) string {
	all := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}
