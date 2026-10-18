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
	"regexp"
	"strings"

	corecomposition "github.com/codefly-dev/core/composition"
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

// frontendBuildContext stages what a component's frontend build may read
// into scratch and returns it, with the component's slash-separated path in
// it. A component inside a git repository (a module cache keeps one as a
// checkout does) builds from that repository's committed tree at HEAD, so a
// remote's dependency on a package beside it resolves exactly as in the
// repository's own CI, and nothing untracked or ignored — a workspace's local
// secrets, another module's cache, node_modules — ever reaches the builder;
// uncommitted edits are not built either. A component in no repository builds
// from its own directory alone, so a remote depending on a package beside it
// cannot build there (a verified package: a follow-up). A component the
// repository does not track at HEAD is refused by name rather than built from
// a tree that is not its own.
func frontendBuildContext(ctx context.Context, module *resources.Module, scratch string) (string, string, error) {
	repository := ""
	for dir := module.Dir(); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			repository = dir
			break
		}
		if filepath.Dir(dir) == dir {
			return module.Dir(), ".", nil
		}
	}
	component, err := filepath.Rel(repository, module.Dir())
	if err != nil {
		return "", "", err
	}
	var archive, stderr bytes.Buffer
	// The tree, not the commit: an archive of a commit opens with a pax global
	// header that core's safe extractor rightly refuses.
	command := exec.CommandContext(ctx, "git", "-C", repository, "archive", "--format=tar", "HEAD^{tree}")
	command.Stdout, command.Stderr = &archive, &stderr
	if err = command.Run(); err != nil {
		return "", "", fmt.Errorf("read the committed tree of %s for the frontend of %s: %w: %s", repository, module.Name, err, strings.TrimSpace(stderr.String()))
	}
	source := filepath.Join(scratch, "source")
	if err = corecomposition.ExtractArchive(ctx, archive.Bytes(), source); err != nil {
		return "", "", fmt.Errorf("stage the committed tree of %s for the frontend of %s (a committed symlink or special file is refused): %w", repository, module.Name, err)
	}
	if info, err := os.Stat(filepath.Join(source, component)); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%s is not tracked at HEAD of %s, so its frontend has no committed source to build from", module.Name, repository)
	}
	return source, filepath.ToSlash(component), nil
}

// buildPath is what a generated Dockerfile may name: plain path segments,
// never a quote, a space, a variable or a line break.
var buildPath = regexp.MustCompile(`^[A-Za-z0-9._@/-]+$`)

// BuildFrontendBundle builds a component's frontend from its own source and
// writes exactly the bundle (FrontendBundleDir) into destination, which must
// not exist yet. The build runs in FrontendBuilderImage with `npm ci` and
// `npm run build`, on the context frontendBuildContext stages: nothing is
// written into the component's directory, which for a release is the
// immutable module cache. The builder sees no variable from this process, so
// an ASSET_PREFIX left in the shell cannot bake an origin into the remote.
func BuildFrontendBundle(ctx context.Context, module *resources.Module, destination string) error {
	project, err := FrontendProject(module)
	if err != nil {
		return err
	}
	services, err := filepath.Rel(module.Dir(), module.ServicesDir())
	if err != nil {
		return err
	}
	if _, err = os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("frontend bundle destination %s must not exist", destination)
	}
	scratch, err := os.MkdirTemp("", "codefly-frontend-source-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	root, component, err := frontendBuildContext(ctx, module, scratch)
	if err != nil {
		return err
	}
	workdir := path.Join("/source", component, project)
	bundle := path.Join("/source", component, filepath.ToSlash(services), FrontendBundleDir)
	if !buildPath.MatchString(workdir) || !buildPath.MatchString(bundle) {
		return fmt.Errorf("%s: the frontend path %s holds a character a build path may not carry", module.Name, path.Join(component, project))
	}
	dockerfile := fmt.Sprintf("FROM %s AS build\nWORKDIR /source\nCOPY [\".\", \".\"]\nWORKDIR %s\nRUN [\"sh\", \"-c\", \"npm ci && npm run build\"]\nFROM scratch\nCOPY --from=build [\"%s/\", \"/\"]\n",
		FrontendBuilderImage, workdir, bundle)
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
