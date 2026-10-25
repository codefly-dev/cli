package solutionrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/gofrs/flock"
)

// LocalFrontend builds a declared frontend for a native run, once per change of
// its own inputs: the files of its npm project the repository tracks (its
// lockfile among them), with their content in the working tree. Nothing else
// in the repository — a Go edit that restarts a service, another module —
// triggers a build, and the caller builds before it takes any delivery lock.
type LocalFrontend struct {
	// StateDir keeps, per module, the inputs the last bundle was built from and
	// the digest it was retained under.
	StateDir string
	// Docker builds in FrontendBuilderImage, as deployment does, for a frontend
	// whose owning service runs in a container. Otherwise the frontend's own
	// npm build runs natively in its project.
	Docker bool
	// Retain archives a built bundle directory into the artifact cache and
	// answers its digest; Retained reports whether a digest is still there.
	Retain   func(ctx context.Context, root string) (string, error)
	Retained func(digest string) bool
}

// localFrontendRecord is what StateDir keeps for one module.
type localFrontendRecord struct {
	// Inputs is the digest of the inputs the bundle was built from.
	Inputs string `json:"inputs"`
	// Installed is the lockfile digest the project's dependencies were last
	// installed from (`npm ci`), for a native build.
	Installed string `json:"installed,omitempty"`
	// Bundle is the retained bundle's digest.
	Bundle string `json:"bundle"`
}

// localFrontendBuildBound bounds one build, and the wait for another run's
// build of the same frontend.
const localFrontendBuildBound = 10 * time.Minute

// Bundle answers the digest of the module's frontend bundle as its inputs are
// now, building it only when no bundle is retained for these inputs.
func (b LocalFrontend) Bundle(ctx context.Context, module *resources.Module) (string, error) {
	project, err := FrontendProject(module)
	if err != nil {
		return "", err
	}
	projectDir := filepath.Join(module.Dir(), filepath.FromSlash(project))
	inputs, lock, err := b.inputs(ctx, module, projectDir)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(b.StateDir, 0o700); err != nil {
		return "", err
	}
	recordPath := filepath.Join(b.StateDir, module.Name+".json")
	guard := flock.New(recordPath + ".lock")
	locked, err := guard.TryLockContext(ctx, 250*time.Millisecond)
	if err != nil || !locked {
		return "", fmt.Errorf("wait for the frontend build of %s: %w", module.Name, errors.Join(err, ctx.Err()))
	}
	defer func() { _ = guard.Unlock() }()
	var record localFrontendRecord
	if data, readErr := os.ReadFile(recordPath); readErr == nil {
		_ = json.Unmarshal(data, &record)
	}
	if record.Inputs == inputs && record.Bundle != "" && b.Retained(record.Bundle) {
		return record.Bundle, nil
	}
	ctx, cancel := context.WithTimeout(ctx, localFrontendBuildBound)
	defer cancel()
	var root string
	if b.Docker {
		scratch, scratchErr := os.MkdirTemp("", "codefly-run-frontend-")
		if scratchErr != nil {
			return "", scratchErr
		}
		defer func() { _ = os.RemoveAll(scratch) }()
		root = filepath.Join(scratch, "bundle")
		if err = buildFrontendBundle(ctx, module, root, localFrontendBuildContext); err != nil {
			return "", err
		}
	} else {
		if root, err = buildFrontendNatively(ctx, module, projectDir, lock != record.Installed); err != nil {
			return "", err
		}
		record.Installed = lock
	}
	if record.Bundle, err = b.Retain(ctx, root); err != nil {
		return "", fmt.Errorf("retain the frontend bundle of %s: %w", module.Name, err)
	}
	record.Inputs = inputs
	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return record.Bundle, os.WriteFile(recordPath, data, 0o600)
}

// inputs digests the frontend's own inputs — each tracked file of its npm
// project, by path, mode and content — with how it is built, and answers the
// digest of its lockfile apart. A symlink among them is refused by name: the
// deployed build reads the committed tree and refuses one too.
func (b LocalFrontend) inputs(ctx context.Context, module *resources.Module, projectDir string) (string, string, error) {
	bundle, err := filepath.Rel(projectDir, filepath.Join(module.ServicesDir(), filepath.FromSlash(FrontendBundleDir)))
	if err != nil {
		return "", "", err
	}
	names, err := trackedFiles(ctx, projectDir)
	if err != nil {
		return "", "", fmt.Errorf("select the frontend sources of %s: %w", module.Name, err)
	}
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "docker=%t image=%s\x00", b.Docker, FrontendBuilderImage)
	var lock string
	tracked := false
	for _, name := range names {
		if frontendBuildOutput(name, bundle) {
			continue
		}
		info, statErr := os.Lstat(filepath.Join(projectDir, name))
		if errors.Is(statErr, fs.ErrNotExist) {
			continue // a tracked file deleted in the working tree
		}
		if statErr != nil {
			return "", "", statErr
		}
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("the frontend of %s cannot be built: %s is not a regular file (a symlink or special file); the deployed build refuses it too, so commit its target instead",
				module.Name, filepath.Join(projectDir, name))
		}
		content, readErr := os.ReadFile(filepath.Join(projectDir, name))
		if readErr != nil {
			return "", "", readErr
		}
		tracked = true
		sum := sha256.Sum256(content)
		_, _ = fmt.Fprintf(digest, "%s\x00%o\x00%x\x00", filepath.ToSlash(name), info.Mode().Perm(), sum)
		if name == "package-lock.json" || name == "npm-shrinkwrap.json" {
			lock = hex.EncodeToString(sum[:])
		}
	}
	if !tracked {
		return "", "", fmt.Errorf("the frontend of %s cannot be built: its repository tracks no file of %s, and the deployed build reads only what is committed", module.Name, projectDir)
	}
	return hex.EncodeToString(digest.Sum(nil)), lock, nil
}

// trackedFiles lists the files under dir its repository tracks, relative to
// dir, as the deployed build's committed-tree boundary selects them: nothing
// untracked, ignored or not. A directory in no repository is its own files.
func trackedFiles(ctx context.Context, dir string) ([]string, error) {
	if frontendRepository(dir) == "" {
		var names []string
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(dir, path)
			if err != nil || relative == "." {
				return err
			}
			if entry.IsDir() {
				if frontendBuildOutput(relative, "") {
					return filepath.SkipDir
				}
				return nil
			}
			names = append(names, relative)
			return nil
		})
		return names, err
	}
	var stderr bytes.Buffer
	command := exec.CommandContext(ctx, "git", "-C", dir, "ls-files", "--cached", "-z")
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var names []string
	for _, name := range strings.Split(string(output), "\x00") {
		if name != "" {
			names = append(names, filepath.FromSlash(name))
		}
	}
	return names, nil
}

// buildFrontendNatively runs the frontend's own npm build in its project —
// `npm ci` first when its dependencies were not installed from this lockfile —
// and answers the bundle it wrote. A failure names the command and carries the
// end of its output.
func buildFrontendNatively(ctx context.Context, module *resources.Module, projectDir string, install bool) (string, error) {
	if !install {
		if _, err := os.Stat(filepath.Join(projectDir, "node_modules")); err != nil {
			install = true
		}
	}
	steps := [][]string{{"npm", "run", "build"}}
	if install {
		steps = append([][]string{{"npm", "ci"}}, steps...)
	}
	for _, step := range steps {
		//nolint:gosec // G204: fixed npm executable and arguments, no shell.
		command := exec.CommandContext(ctx, step[0], step[1:]...)
		command.Dir = projectDir
		command.Env = nativeBuildEnvironment(os.Environ())
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Run(); err != nil {
			return "", fmt.Errorf("build the frontend of %s (%s in %s): %w\n%s", module.Name, strings.Join(step, " "), projectDir, err, tail(output.String(), 40))
		}
	}
	root, built, err := FrontendBundle(module)
	if err != nil {
		return "", err
	}
	if !built {
		return "", fmt.Errorf("the frontend build of %s wrote no %s under %s", module.Name, FrontendBundleDir, module.ServicesDir())
	}
	return root, nil
}

// nativeBuildEnvironment is what a native frontend build sees of this process:
// what finds and runs npm and reaches its registry, and nothing a bundler
// would bake into the remote (an ASSET_PREFIX left in the shell), as the
// containerized build sees nothing at all.
func nativeBuildEnvironment(environ []string) []string {
	var kept []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		switch {
		case name == "PATH", name == "HOME", name == "USER", name == "LOGNAME", name == "TMPDIR", name == "LANG", name == "LC_ALL",
			name == "NODE_EXTRA_CA_CERTS", name == "SSL_CERT_FILE",
			strings.EqualFold(name, "HTTP_PROXY"), strings.EqualFold(name, "HTTPS_PROXY"), strings.EqualFold(name, "NO_PROXY"),
			strings.HasPrefix(strings.ToLower(name), "npm_config_"):
			kept = append(kept, entry)
		}
	}
	return kept
}

// localFrontendBuildContext stages what a containerized local build reads: the
// files of the enclosing repository it tracks, with their working-tree
// content, so a remote's dependency on a sibling package resolves as in the
// deployed build. Untracked files never reach the builder, ignored or not, as
// they never reach the deployed one. A symlink inside the frontend's own
// project is refused (LocalFrontend.inputs); one elsewhere in the repository is
// not this frontend's input and is left out of the context.
func localFrontendBuildContext(ctx context.Context, module *resources.Module, scratch string) (string, string, error) {
	root := frontendRepository(module.Dir())
	if root == "" {
		root = module.Dir()
	}
	bundle, err := filepath.Rel(root, filepath.Join(module.ServicesDir(), filepath.FromSlash(FrontendBundleDir)))
	if err != nil {
		return "", "", err
	}
	names, err := trackedFiles(ctx, root)
	if err != nil {
		return "", "", fmt.Errorf("select local frontend sources for %s: %w", module.Name, err)
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
	target, err := os.OpenRoot(destination)
	if err != nil {
		return "", "", err
	}
	defer target.Close()
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			return "", "", err
		}
		if frontendBuildOutput(name, bundle) {
			continue
		}
		info, err := source.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a tracked file deleted in the working tree
		}
		if err != nil {
			return "", "", err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, err := source.ReadFile(name)
		if err != nil {
			return "", "", err
		}
		if err = target.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return "", "", err
		}
		if err = target.WriteFile(name, data, info.Mode().Perm()); err != nil {
			return "", "", err
		}
	}
	return destination, filepath.ToSlash(component), nil
}

// frontendBuildOutput reports whether a path is a build's output or tooling
// state rather than its input: this frontend's bundle, any node_modules, .git
// or .codefly.
func frontendBuildOutput(path, bundle string) bool {
	path = filepath.ToSlash(path)
	if bundle != "" {
		bundle = filepath.ToSlash(bundle)
		if path == bundle || strings.HasPrefix(path, bundle+"/") {
			return true
		}
	}
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case ".git", ".codefly", "node_modules":
			return true
		}
	}
	return false
}
