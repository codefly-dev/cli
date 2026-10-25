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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
	"github.com/gofrs/flock"
)

// LocalFrontend builds a declared frontend for a native run, once per change of
// what its build reads (frontendSources): its npm project's files, tracked or
// new, the local packages it depends on, and the top-level configuration of the
// directories above it. Nothing else — a Go edit that restarts a service,
// another module — triggers a build, and the caller builds before it takes any
// delivery lock. The build reads a staged copy of exactly those files, so what
// it reads is what was digested, and it never writes into the component.
type LocalFrontend struct {
	// StateDir keeps, per module, the inputs the last bundle was built from,
	// the digest it was retained under, and the native build's staged copy
	// with its own installed dependencies.
	StateDir string
	// Docker builds in FrontendBuilderImage, as deployment does, for a frontend
	// whose owning service runs in a container. Otherwise the frontend's own
	// npm build runs natively, in the staged copy.
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
	// Installed is the lockfile digest the staged copy's dependencies were
	// last installed from (`npm ci`), for a native build.
	Installed string `json:"installed,omitempty"`
	// Bundle is the retained bundle's digest.
	Bundle string `json:"bundle"`
}

// localFrontendBuildBound bounds one build, and the wait for another run's
// build of the same frontend.
const localFrontendBuildBound = 10 * time.Minute

// Bundle answers the digest of the module's frontend bundle as its sources are
// now, building it only when no bundle is retained for these sources.
func (b LocalFrontend) Bundle(ctx context.Context, module *resources.Module) (string, error) {
	sources, err := selectFrontendSources(ctx, module)
	if err != nil {
		return "", err
	}
	inputs, lock, err := sources.digest(module, b.Docker)
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
		stage := func(ctx context.Context, _ *resources.Module, scratch string) (string, string, error) {
			destination := filepath.Join(scratch, "source")
			return destination, sources.component, sources.sync(ctx, destination)
		}
		if err = buildFrontendBundle(ctx, module, root, stage); err != nil {
			return "", err
		}
	} else {
		staging := filepath.Join(b.StateDir, module.Name, "source")
		if root, err = sources.buildNatively(ctx, module, staging, lock != record.Installed); err != nil {
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

// frontendSources is what a local frontend build reads, relative to root (the
// enclosing repository, or the component in none):
//   - every file of the npm project its repository tracks, and every new one
//     it does not ignore — but never a new dotenv file (.env, .env.*), which
//     the deployed build, reading the committed tree, never sees;
//   - the same of each local package the project depends on (a `file:` or
//     `link:` dependency), and of theirs;
//   - the top-level regular files of every directory from root down to the
//     project (a tsconfig base, an .npmrc), as tracked.
//
// Never read: this frontend's bundle, any node_modules, .git or .codefly; and,
// for a project at the component root, the component's services other than
// the remote and every Go source.
type frontendSources struct {
	root, component, project string
	files                    []string
}

func selectFrontendSources(ctx context.Context, module *resources.Module) (*frontendSources, error) {
	project, err := FrontendProject(module)
	if err != nil {
		return nil, err
	}
	root := frontendRepository(module.Dir())
	repository := root != ""
	if !repository {
		root = module.Dir()
	}
	component, err := filepath.Rel(root, module.Dir())
	if err != nil {
		return nil, err
	}
	projectDir := filepath.Join(module.Dir(), filepath.FromSlash(project))
	bundle := rootRelative(root, filepath.Join(module.ServicesDir(), filepath.FromSlash(FrontendBundleDir)))
	services := rootRelative(root, module.ServicesDir())
	remote := path.Join(services, path.Dir(FrontendBundleDir))
	atComponentRoot := project == "."
	selected := map[string]bool{}
	add := func(relative string, tracked bool) {
		base := path.Base(relative)
		switch {
		case frontendBuildOutput(relative, bundle):
		case !tracked && (base == ".env" || strings.HasPrefix(base, ".env.")):
		case atComponentRoot && strings.HasPrefix(relative, services+"/") && relative != remote && !strings.HasPrefix(relative, remote+"/"):
		case atComponentRoot && goSource(base):
		default:
			selected[relative] = true
		}
	}
	packages, err := localPackages(root, projectDir)
	if err != nil {
		return nil, fmt.Errorf("the frontend of %s: %w", module.Name, err)
	}
	for _, dir := range packages {
		names, err := listFiles(ctx, dir, repository, true)
		if err != nil {
			return nil, fmt.Errorf("select the frontend sources of %s: %w", module.Name, err)
		}
		for name, tracked := range names {
			add(rootRelative(root, filepath.Join(dir, name)), tracked)
		}
	}
	for dir := projectDir; dir != root; {
		dir = filepath.Dir(dir)
		if rootRelative(root, dir) == ".." || strings.HasPrefix(rootRelative(root, dir), "../") {
			break
		}
		names, err := listFiles(ctx, dir, repository, false)
		if err != nil {
			return nil, fmt.Errorf("select the frontend sources of %s: %w", module.Name, err)
		}
		for name, tracked := range names {
			// Configuration a build may look up the tree for; a symlink here
			// (AGENTS.md -> CLAUDE.md) is not one, and is left out.
			info, statErr := os.Lstat(filepath.Join(dir, name))
			if statErr == nil && info.Mode().IsRegular() && (tracked || !repository) && !goSource(name) {
				add(rootRelative(root, filepath.Join(dir, name)), tracked)
			}
		}
	}
	sources := &frontendSources{root: root, component: filepath.ToSlash(component), project: rootRelative(root, projectDir)}
	for relative := range selected {
		sources.files = append(sources.files, relative)
	}
	sort.Strings(sources.files)
	return sources, nil
}

// localPackages is the project and every local package it depends on — a
// `file:` or `link:` dependency naming a directory — and theirs, each inside
// root: a build cannot read a package the repository does not hold.
func localPackages(root, project string) ([]string, error) {
	var packages []string
	seen := map[string]bool{}
	var visit func(string) error
	visit = func(dir string) error {
		if seen[dir] {
			return nil
		}
		if relative, err := filepath.Rel(root, dir); err != nil || !filepath.IsLocal(relative) && relative != "." {
			return fmt.Errorf("local package %s is outside %s, so no build can read it", dir, root)
		}
		seen[dir] = true
		packages = append(packages, dir)
		data, err := os.ReadFile(filepath.Join(dir, "package.json"))
		if err != nil {
			return nil
		}
		var manifest map[string]map[string]string
		_ = json.Unmarshal(data, &manifest)
		for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
			for _, spec := range manifest[field] {
				local, ok := strings.CutPrefix(spec, "file:")
				if !ok {
					local, ok = strings.CutPrefix(spec, "link:")
				}
				if !ok {
					continue
				}
				target := filepath.Join(dir, filepath.FromSlash(local))
				if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
					if err := visit(target); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	return packages, visit(project)
}

// listFiles lists the files under dir (only its own, unless recursive),
// relative to it, each marked tracked or not: in a repository, what it tracks
// and what it does not ignore; in none, every file, none of them tracked.
func listFiles(ctx context.Context, dir string, repository, recursive bool) (map[string]bool, error) {
	names := map[string]bool{}
	if !repository {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(dir, path)
			if err != nil || relative == "." {
				return err
			}
			if entry.IsDir() {
				if !recursive || frontendBuildOutput(relative, "") {
					return filepath.SkipDir
				}
				return nil
			}
			names[relative] = false
			return nil
		})
		return names, err
	}
	for _, listing := range []struct {
		tracked bool
		args    []string
	}{{false, []string{"--others", "--exclude-standard"}}, {true, []string{"--cached"}}} {
		var stderr bytes.Buffer
		args := append([]string{"-C", dir, "ls-files", "-z"}, listing.args...)
		command := exec.CommandContext(ctx, "git", args...)
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files in %s: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
		}
		for _, name := range strings.Split(string(output), "\x00") {
			if name != "" && (recursive || !strings.Contains(name, "/")) {
				names[filepath.FromSlash(name)] = listing.tracked
			}
		}
	}
	return names, nil
}

// digest is the sources by path, mode and content, with how they are built,
// and the project's lockfile digest apart. A symlink or special file among
// them is refused by name: the deployed build refuses one too.
func (s *frontendSources) digest(module *resources.Module, docker bool) (string, string, error) {
	sum := sha256.New()
	_, _ = fmt.Fprintf(sum, "docker=%t image=%s project=%s\x00", docker, FrontendBuilderImage, s.project)
	var lock string
	present := 0
	for _, relative := range s.files {
		full := filepath.Join(s.root, filepath.FromSlash(relative))
		info, err := os.Lstat(full)
		if errors.Is(err, fs.ErrNotExist) {
			continue // a tracked file deleted in the working tree
		}
		if err != nil {
			return "", "", err
		}
		if !info.Mode().IsRegular() {
			return "", "", fmt.Errorf("the frontend of %s cannot be built: %s is not a regular file (a symlink or special file); the deployed build refuses it too, so commit its target instead", module.Name, full)
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return "", "", err
		}
		present++
		hash := sha256.Sum256(content)
		_, _ = fmt.Fprintf(sum, "%s\x00%o\x00%x\x00", relative, info.Mode().Perm(), hash)
		if relative == path.Join(s.project, "package-lock.json") || relative == path.Join(s.project, "npm-shrinkwrap.json") {
			lock = hex.EncodeToString(hash[:])
		}
	}
	if present == 0 {
		return "", "", fmt.Errorf("the frontend of %s cannot be built: its npm project %s holds no file to build from", module.Name, s.project)
	}
	return hex.EncodeToString(sum.Sum(nil)), lock, nil
}

// sync makes destination hold exactly the sources, under their root-relative
// paths — writing each, removing every other file — except node_modules,
// which a native build installs there and keeps between builds.
func (s *frontendSources) sync(ctx context.Context, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, relative := range s.files {
		wanted[relative] = true
	}
	target, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer target.Close()
	err = fs.WalkDir(target.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !wanted[name] {
			return target.Remove(name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	source, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer source.Close()
	for _, relative := range s.files {
		if err = ctx.Err(); err != nil {
			return err
		}
		name := filepath.FromSlash(relative)
		info, err := source.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		data, err := source.ReadFile(name)
		if err != nil {
			return err
		}
		if err = target.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			return err
		}
		if err = target.WriteFile(name, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// buildNatively runs the frontend's own npm build in staging, a copy of
// exactly its sources — `npm ci` first when the copy's dependencies were not
// installed from this lockfile — and answers the bundle it wrote. The
// component's own node_modules and files are never touched. A failure names the
// command and carries the end of its output.
func (s *frontendSources) buildNatively(ctx context.Context, module *resources.Module, staging string, install bool) (string, error) {
	if err := s.sync(ctx, staging); err != nil {
		return "", fmt.Errorf("stage the frontend sources of %s: %w", module.Name, err)
	}
	projectDir := filepath.Join(staging, filepath.FromSlash(s.project))
	if _, err := os.Stat(filepath.Join(projectDir, "node_modules")); err != nil {
		install = true
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
			return "", fmt.Errorf("build the frontend of %s (%s in %s): %w\n%s", module.Name, strings.Join(step, " "), s.project, err, tail(output.String(), 40))
		}
	}
	services, err := filepath.Rel(module.Dir(), module.ServicesDir())
	if err != nil {
		return "", err
	}
	root := filepath.Join(staging, filepath.FromSlash(s.component), services, filepath.FromSlash(FrontendBundleDir))
	if manifest, err := os.Stat(filepath.Join(root, FrontendBundleManifest)); err != nil || !manifest.Mode().IsRegular() {
		return "", fmt.Errorf("the frontend build of %s wrote no regular %s at the root of %s", module.Name, FrontendBundleManifest, FrontendBundleDir)
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

// rootRelative is full's slash-separated path under root.
func rootRelative(root, full string) string {
	relative, err := filepath.Rel(root, full)
	if err != nil {
		return filepath.ToSlash(full)
	}
	return filepath.ToSlash(relative)
}

// goSource reports whether a file is Go code or a Go module file, which no
// frontend build reads.
func goSource(name string) bool {
	base := filepath.Base(name)
	return strings.HasSuffix(base, ".go") || base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum"
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
