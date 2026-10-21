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
	"syscall"
	"time"

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
// credential helper, an ssh agent); nothing secret reaches the build. Like the
// go tool's fetches, which run in repositories under the module cache, it runs
// in a repository of its own, so the configuration of a checkout the CLI runs
// from is not consulted; global and environment configuration is.
//
// Each repository has one bare mirror at GitSourceMirrorPath(repository),
// holding each declared commit under GitSourceMirrorRef(commit). The mirror is
// shallow by design: it carries the locked commits and nothing else, which
// serves a consumer that fetches by commit id, as a package manager installing
// a locked git dependency does (`git fetch <url> +<commit>:refs/commit/<commit>`).
// Every recipe of the flow is handed the one directory holding all the mirrors,
// as every Go recipe is handed one module proxy: a build fetches only the
// commits its own lockfile names, so another service's mirrors are inert.
type gitSourcePrefetch struct {
	mu      sync.Mutex
	root    string
	fetched map[[2]string]bool
	// formats is the object format each repository's mirror was created with.
	formats map[string]string
	// run executes git and returns its standard output; a test substitutes it.
	run func(ctx context.Context, env []string, args ...string) (string, error)
}

func newGitSourcePrefetch() *gitSourcePrefetch {
	return &gitSourcePrefetch{fetched: map[[2]string]bool{}, formats: map[string]string{}, run: runGitCommand}
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
	// A 40-digit id is SHA-1 and a 64-digit one SHA-256; a repository has one.
	format := "sha1"
	if len(commit) == 64 {
		format = "sha256"
	}
	if known, ok := p.formats[repository]; ok && known != format {
		return "", fmt.Errorf("a git source is declared with both %s and %s commit ids", known, format)
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
	if _, known := p.formats[repository]; !known {
		// An empty template: a host template's hooks or configuration would
		// otherwise be copied into the mirror, and so into the build context.
		if _, err := p.run(ctx, env, "init", "--quiet", "--bare", "--template=", "--object-format="+format, mirror); err != nil {
			return "", fmt.Errorf("cannot create the git source mirror: %w", err)
		}
		p.formats[repository] = format
	}
	git := func(args ...string) (string, error) {
		return p.run(ctx, env, append([]string{"--git-dir=" + mirror}, args...)...)
	}
	ref := coreservices.GitSourceMirrorRef(commit)
	// A host that serves a commit by id gives just that commit. One that refuses
	// an unadvertised commit (protocol v0 without allowAnySHA1InWant) gives it
	// with its branches and tags, which is how any git client then reaches it.
	if _, err := git("fetch", "--depth=1", "--end-of-options", repository, "+"+commit+":"+ref); err != nil {
		full := []string{"fetch"}
		if _, shallow := os.Stat(filepath.Join(mirror, "shallow")); shallow == nil {
			full = append(full, "--unshallow")
		}
		full = append(full, "--end-of-options", repository, "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
		if _, fullErr := git(full...); fullErr != nil {
			return "", fmt.Errorf("cannot fetch commit %s of a declared git source: %w", commit, errors.Join(err, fullErr))
		}
		if _, err := git("update-ref", ref, commit); err != nil {
			return "", fmt.Errorf("the declared git source does not hold commit %s: %w", commit, err)
		}
	}
	// The mirror ref must name exactly the locked commit: a branch spelled like
	// a commit id is never taken for it.
	resolved, err := git("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("the declared git source does not hold commit %s: %w", commit, err)
	}
	if resolved != commit {
		return "", fmt.Errorf("the declared git source resolves commit %s to %s", commit, resolved)
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
	p.formats = map[string]string{}
	return err
}

// gitSourcePrefetchEnv is the host environment, so the host's git configuration
// decides how a repository is reached, as goPrefetchEnv leaves it to decide how
// a module is. What it imposes:
//   - no prompt for a credential the host lacks (GIT_TERMINAL_PROMPT=0, and
//     GCM_INTERACTIVE=never unless the host chose, as the go tool does), so the
//     fetch fails instead of hanging;
//   - gitPrefetchConfig;
//   - no variable pointing git at another repository or template;
//   - no tracing: a trace prints a credential helper's command line and
//     protocol traffic into the output this reports.
//
// It sets no GIT_ALLOW_PROTOCOL: git checks that after the host's insteadOf
// rewrites, so it would refuse a host rewrite to a local path; the declared URL
// itself is https or ssh, which the recipe's validation enforces.
func gitSourcePrefetchEnv(base []string) []string {
	env := make([]string, 0, len(base)+2)
	interactive := false
	for _, entry := range base {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_TERMINAL_PROMPT", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
			"GIT_TEMPLATE_DIR", "GIT_CURL_VERBOSE":
			continue
		case "GCM_INTERACTIVE":
			interactive = value != ""
		}
		if strings.HasPrefix(key, "GIT_TRACE") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	if !interactive {
		env = append(env, "GCM_INTERACTIVE=never")
	}
	return withGitPrefetchConfig(env)
}

var errGitMissing = errors.New("a recipe declares git sources, which the CLI fetches with the host's git, and `git` is not on PATH")

// gitWaitDelay bounds how long a cancelled git may hold its output open: a
// credential helper or askpass it started can outlive it.
const gitWaitDelay = 5 * time.Second

// runGitCommand runs git in its own process group, so cancelling the context
// stops git and every helper it started, and returns its trimmed standard
// output.
func runGitCommand(ctx context.Context, env []string, args ...string) (string, error) {
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		return "", errGitMissing
	}
	command := exec.CommandContext(ctx, gitBinary, args...)
	command.Env = env
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = gitWaitDelay
	var stdout, output bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &output
	if err := command.Run(); err != nil {
		output.Write(stdout.Bytes())
		return "", fmt.Errorf("git %s: %w\n%s", redact.URLCredentials(strings.Join(args, " ")), err, redact.URLCredentials(lastLines(output.String(), 40)))
	}
	return strings.TrimSpace(stdout.String()), nil
}
