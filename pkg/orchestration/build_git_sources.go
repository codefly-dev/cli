package orchestration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"

	"github.com/codefly-dev/cli/pkg/redact"
)

// gitSourcePrefetch fetches the locked git commits a flow's recipes declare
// (DockerBuildRecipe.git_source_downloads) once, on the host, before any image
// is built, and hands each build the result as bare mirrors in a named build
// context. It is goModulePrefetch for a dependency a lockfile pins to a git
// commit rather than to a module version.
//
// The fetch runs with the host's own git and environment, so it authenticates
// exactly as the developer's or the CI job's git does (insteadOf rewrites, a
// credential helper, an ssh agent); nothing secret reaches the build. Each
// repository has one bare mirror at GitSourceMirrorPath(repository), holding
// each declared commit under GitSourceMirrorRef(commit), which is the ref a
// fetch by commit id writes, so a build fetching that commit from the mirror
// finds it. Every recipe of the flow is handed the one directory holding all the
// mirrors, as every Go recipe is handed one module proxy: a build fetches only
// the commits its own lockfile names, so another service's mirrors are inert.
type gitSourcePrefetch struct {
	mu      sync.Mutex
	root    string
	fetched map[[2]string]bool
	// run executes git; a test substitutes it.
	run func(ctx context.Context, env []string, args ...string) error
}

func newGitSourcePrefetch() *gitSourcePrefetch {
	return &gitSourcePrefetch{fetched: map[[2]string]bool{}, run: runGitCommand}
}

// fetch places commit of repository in its mirror and returns the directory
// holding every mirror. A commit fetched earlier in the flow is not fetched
// again.
func (p *gitSourcePrefetch) fetch(ctx context.Context, repository, commit string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fetched[[2]string{repository, commit}] {
		return p.root, nil
	}
	if p.root == "" {
		root, err := os.MkdirTemp("", "codefly-git-sources-*")
		if err != nil {
			return "", fmt.Errorf("cannot stage the git source prefetch: %w", err)
		}
		p.root = root
	}
	mirror := filepath.Join(p.root, coreservices.GitSourceMirrorPath(repository))
	env := gitSourcePrefetchEnv(os.Environ())
	if _, err := os.Stat(mirror); err != nil {
		if err := p.run(ctx, env, "init", "--quiet", "--bare", mirror); err != nil {
			return "", fmt.Errorf("cannot create the git source mirror: %w", err)
		}
	}
	git := func(args ...string) error {
		return p.run(ctx, env, append([]string{"--git-dir=" + mirror}, args...)...)
	}
	ref := coreservices.GitSourceMirrorRef(commit)
	// A host that serves a commit by id gives just that commit. One that refuses
	// an unadvertised commit (protocol v0 without allowAnySHA1InWant) gives it
	// with its branches and tags, which is how any git client then reaches it.
	if err := git("fetch", "--depth=1", "--end-of-options", repository, "+"+commit+":"+ref); err != nil {
		full := []string{"fetch"}
		if _, shallow := os.Stat(filepath.Join(mirror, "shallow")); shallow == nil {
			full = append(full, "--unshallow")
		}
		full = append(full, "--end-of-options", repository, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
		if fullErr := git(full...); fullErr != nil {
			return "", fmt.Errorf("cannot fetch commit %s of a declared git source: %w", commit, errors.Join(err, fullErr))
		}
		if err := git("update-ref", ref, commit); err != nil {
			return "", fmt.Errorf("the declared git source does not hold commit %s: %w", commit, err)
		}
	}
	if err := git("cat-file", "-e", commit+"^{commit}"); err != nil {
		return "", fmt.Errorf("the declared git source does not hold commit %s: %w", commit, err)
	}
	p.fetched[[2]string{repository, commit}] = true
	return p.root, nil
}

// mirrorsFor fetches every git source a recipe declares and returns the
// directory to supply under each declared mirror context.
func (p *gitSourcePrefetch) mirrorsFor(ctx context.Context, recipe *builderv0.DockerBuildRecipe) (map[string]string, error) {
	downloads := recipe.GetGitSourceDownloads()
	if len(downloads) == 0 {
		return nil, nil
	}
	mirrors := make(map[string]string, len(downloads))
	for index, download := range downloads {
		root, err := p.fetch(ctx, download.GetRepository(), download.GetCommit())
		if err != nil {
			return nil, fmt.Errorf("recipe %s git source %d: %w", recipe.GetName(), index, err)
		}
		mirrors[download.GetMirrorContext()] = root
	}
	return mirrors, nil
}

// recipeMissesGitSourceMirrors reports whether a recipe declares a git source
// that has no fetched mirror under its build-context name.
func recipeMissesGitSourceMirrors(recipe *builderv0.DockerBuildRecipe, contexts map[string]string) bool {
	for _, download := range recipe.GetGitSourceDownloads() {
		if _, ok := contexts[download.GetMirrorContext()]; !ok {
			return true
		}
	}
	return false
}

// Close removes every mirror the flow fetched.
func (p *gitSourcePrefetch) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root == "" {
		return nil
	}
	err := os.RemoveAll(p.root)
	p.root = ""
	p.fetched = map[[2]string]bool{}
	return err
}

// gitSourcePrefetchEnv is the host environment, so the host's git configuration
// decides how a repository is reached, as goPrefetchEnv leaves it to decide how
// a module is. It imposes three things: no prompt for a credential the host
// lacks (the fetch fails instead of hanging), gitPrefetchConfig, and no
// variable that would point git at a repository other than the mirror. It sets
// no GIT_ALLOW_PROTOCOL: git checks that after the host's insteadOf rewrites, so
// it would refuse a host rewrite to a local path; the declared URL itself is
// https or ssh, which the recipe's validation enforces.
func gitSourcePrefetchEnv(base []string) []string {
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		switch key, _, _ := strings.Cut(entry, "="); key {
		case "GIT_TERMINAL_PROMPT", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE":
			continue
		}
		env = append(env, entry)
	}
	return withGitPrefetchConfig(append(env, "GIT_TERMINAL_PROMPT=0"))
}

var errGitMissing = errors.New("a recipe declares git sources, which the CLI fetches with the host's git, and `git` is not on PATH")

func runGitCommand(ctx context.Context, env []string, args ...string) error {
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		return errGitMissing
	}
	command := exec.CommandContext(ctx, gitBinary, args...)
	command.Env = env
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", redact.URLCredentials(strings.Join(args, " ")), err, redact.URLCredentials(lastLines(output.String(), 40)))
	}
	return nil
}
