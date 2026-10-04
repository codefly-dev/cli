package orchestration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

// goModulePrefetch downloads the Go module graphs a flow's recipes declare
// (DockerBuildRecipe.go_module_downloads) once, on the host, before any image
// is built, and hands each build the result as a module proxy.
//
// Fetching inside the build needed a credential mounted into BuildKit, and it
// needed that credential to still be valid whenever the build ran — hours into a
// long render, a short-lived CI token no longer was. Here the fetch runs with
// the host's own go toolchain, so it authenticates exactly as the developer's
// or the CI job's `go` does (GOPRIVATE, git configuration, netrc, GOPROXY), and
// it runs in the snapshot's plan phase, before the first image build. The build
// then reads every module from a directory: no credential reaches it, and none
// has to outlive the start of the flow.
//
// Every module root of the flow is fetched into one module cache, so a
// dependency shared by several services is downloaded and stored once, and each
// build is handed that cache's proxy tree. A build reads only the versions its
// own go.sum names, so the modules another service fetched are inert to it.
// Only the proxy tree is kept: the sources the go tool extracts and the version
// control clones it makes for direct fetches are never read by a build, and a
// render fetches many graphs on a runner with bounded disk.
type goModulePrefetch struct {
	mu      sync.Mutex
	root    string
	fetched map[string]string
	// run executes the go tool; a test substitutes it.
	run func(ctx context.Context, dir string, env []string, args ...string) error
}

func newGoModulePrefetch() *goModulePrefetch {
	return &goModulePrefetch{fetched: map[string]string{}, run: runGoCommand}
}

// fetch downloads the module graph rooted at moduleDir (the directory holding
// its go.mod) and returns the directory to supply as its module proxy: the
// cache/download tree of a module cache, which is the Go module proxy layout.
// A module fetched earlier in the flow is not fetched again.
func (p *goModulePrefetch) fetch(ctx context.Context, moduleDir string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if proxy, ok := p.fetched[moduleDir]; ok {
		return proxy, nil
	}
	if _, err := os.Stat(filepath.Join(moduleDir, "go.mod")); err != nil {
		return "", fmt.Errorf("declared Go module root %s has no go.mod: %w", moduleDir, err)
	}
	if p.root == "" {
		root, err := os.MkdirTemp("", "codefly-go-modules-*")
		if err != nil {
			return "", fmt.Errorf("cannot stage the Go module prefetch: %w", err)
		}
		p.root = root
	}
	modCache := filepath.Join(p.root, "modcache")
	env := goPrefetchEnv(os.Environ(), modCache)
	// The same two reads the build makes: the modules its packages need, and the
	// go.mod of every module in the graph, which listing it loads.
	for _, args := range [][]string{
		{"mod", "download"},
		{"list", "-m", "-json", "all"},
	} {
		if err := p.run(ctx, moduleDir, env, args...); err != nil {
			return "", fmt.Errorf("cannot download the Go modules of %s: %w", moduleDir, err)
		}
	}
	proxy := filepath.Join(modCache, "cache", "download")
	// A module with no dependencies downloads nothing; its proxy is empty.
	if err := os.MkdirAll(proxy, 0o755); err != nil {
		return "", fmt.Errorf("cannot stage the Go module proxy of %s: %w", moduleDir, err)
	}
	if err := pruneToProxy(modCache); err != nil {
		return "", fmt.Errorf("cannot prune the Go module cache of %s: %w", moduleDir, err)
	}
	p.fetched[moduleDir] = proxy
	return proxy, nil
}

// pruneToProxy removes everything in a module cache but its proxy tree
// (cache/download): the extracted module sources and the version control
// clones under cache/vcs. The go tool re-extracts from the proxy tree when a
// later fetch needs a module again.
func pruneToProxy(modCache string) error {
	entries, err := os.ReadDir(modCache)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "cache" {
			continue
		}
		if err = os.RemoveAll(filepath.Join(modCache, entry.Name())); err != nil {
			return err
		}
	}
	caches, err := os.ReadDir(filepath.Join(modCache, "cache"))
	if err != nil {
		return err
	}
	for _, entry := range caches {
		if entry.Name() == "download" {
			continue
		}
		if err = os.RemoveAll(filepath.Join(modCache, "cache", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// proxiesFor fetches every download a recipe declares and returns the proxy to
// supply under each declared build-context name. contextDir is the recipe's
// resolved build context, which module roots are relative to.
func (p *goModulePrefetch) proxiesFor(ctx context.Context, contextDir string, recipe *builderv0.DockerBuildRecipe) (map[string]string, error) {
	downloads := recipe.GetGoModuleDownloads()
	if len(downloads) == 0 {
		return nil, nil
	}
	proxies := make(map[string]string, len(downloads))
	for _, download := range downloads {
		moduleDir := filepath.Join(contextDir, filepath.FromSlash(download.GetModuleRoot()))
		rel, err := filepath.Rel(contextDir, moduleDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("recipe %s declares Go module root %q outside its build context", recipe.GetName(), download.GetModuleRoot())
		}
		proxy, err := p.fetch(ctx, moduleDir)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", recipe.GetName(), err)
		}
		proxies[download.GetProxyContext()] = proxy
	}
	return proxies, nil
}

// recipeMissesGoModuleProxies reports whether a recipe declares a Go module
// download that has no fetched proxy under its build-context name.
func recipeMissesGoModuleProxies(recipe *builderv0.DockerBuildRecipe, proxies map[string]string) bool {
	for _, download := range recipe.GetGoModuleDownloads() {
		if _, ok := proxies[download.GetProxyContext()]; !ok {
			return true
		}
	}
	return false
}

// Close removes every module cache the flow fetched.
func (p *goModulePrefetch) Close() error {
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
	p.fetched = map[string]string{}
	return err
}

// goPrefetchEnv is the host environment with the module cache redirected to the
// prefetch's own. Everything that decides how a module is reached — GOPRIVATE,
// GOPROXY, GONOSUMDB, GOSUMDB, NETRC, git's credentials and insteadOf rewrites —
// is the host's, so the prefetch reaches a private module exactly when the
// host's `go` does. GOWORK is off because a build resolves its module alone, as
// the recipe does; -modcacherw keeps the cache removable; -mod=readonly makes an
// incomplete go.sum fail here, as it would in the build, instead of being
// repaired. The one thing it does impose on git is gitPrefetchConfig.
func goPrefetchEnv(base []string, modCache string) []string {
	configured := hostGitConfigCount(base)
	env := make([]string, 0, len(base)+4+2*len(gitPrefetchConfig))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		switch {
		case key == "GOMODCACHE", key == "GOFLAGS", key == "GOWORK":
			continue
		case key == "GIT_CONFIG_COUNT":
			// Re-emitted below with the prefetch's own entries counted in.
			continue
		case gitConfigIndex(key) >= configured:
			// An entry at or past the host's own count is one git never reads,
			// so dropping it loses no host configuration — and it keeps the
			// indices the prefetch appends from colliding with a stale one.
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GOMODCACHE="+modCache, "GOFLAGS=-mod=readonly -modcacherw", "GOWORK=off")
	for _, setting := range gitPrefetchConfig {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", configured, setting[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", configured, setting[1]),
		)
		configured++
	}
	return append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(configured))
}

// gitPrefetchConfig is what the prefetch imposes on every git the go tool runs
// in its module cache: no automatic repository maintenance.
//
// A `git fetch` ends by launching `git maintenance run --auto --detach`, which
// repacks the repository it just fetched into — in a process that outlives the
// fetch. The go tool reaches a pseudo-version's commit with `git fetch --depth=1
// <hash>`, and when it then needs that commit's ancestry (a pseudo-version names
// a base tag, so validating it reads tags the shallow clone does not have) it
// deepens the clone: `git fetch` of every ref, then `git fetch --unshallow`. The
// maintenance the first of those launched rewrites .git/shallow while the
// unshallow is working from it, and git refuses the fetch:
//
//	fatal: shallow file has changed since we read it
//
// which the go tool reports as `invalid pseudo-version`, failing the render that
// was only fetching modules (codefly-dev/cli#886). Both settings are named
// because the task that rewrites the clone is reached two ways: git ≥ 2.30 runs
// it as the `maintenance` command, older gits as `gc --auto`.
//
// Nothing is given up by refusing it. This cache exists for the length of one
// flow and is deleted with it, every clone in it is pruned as soon as its
// modules are in the proxy tree (pruneToProxy), and no later command reads the
// repository being maintained.
var gitPrefetchConfig = [][2]string{
	{"maintenance.auto", "false"},
	{"gc.auto", "0"},
}

// hostGitConfigCount is how many configuration entries the host already passes
// to git in the environment: GIT_CONFIG_COUNT, with a GIT_CONFIG_KEY_<n> and
// GIT_CONFIG_VALUE_<n> for each index below it. A value git itself would reject
// counts as none, so the prefetch's entries start at 0 and the unusable ones are
// left behind instead of being carried into every git the go tool runs.
func hostGitConfigCount(base []string) int {
	for _, entry := range base {
		if value, ok := strings.CutPrefix(entry, "GIT_CONFIG_COUNT="); ok {
			count, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || count < 0 {
				return 0
			}
			return count
		}
	}
	return 0
}

// gitConfigIndex is the index of a GIT_CONFIG_KEY_<n> or GIT_CONFIG_VALUE_<n>
// variable, and -1 for every other name.
func gitConfigIndex(key string) int {
	for _, prefix := range []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"} {
		suffix, ok := strings.CutPrefix(key, prefix)
		if !ok {
			continue
		}
		index, err := strconv.Atoi(suffix)
		if err != nil || index < 0 {
			return -1
		}
		return index
	}
	return -1
}

var errGoToolchainMissing = errors.New("a recipe declares Go module downloads, which the CLI fetches with the host's go toolchain, and `go` is not on PATH")

func runGoCommand(ctx context.Context, dir string, env []string, args ...string) error {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		return errGoToolchainMissing
	}
	command := exec.CommandContext(ctx, goBinary, args...)
	command.Dir = dir
	command.Env = env
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, redactURLCredentials(lastLines(output.String(), 40)))
	}
	return nil
}

// urlCredentials matches the userinfo of a URL. A git remote rewritten with a
// token (a CI job's insteadOf) can surface in the go tool's error output, and a
// wool field is logged verbatim.
var urlCredentials = regexp.MustCompile(`://[^/\s@]+@`)

func redactURLCredentials(text string) string {
	return urlCredentials.ReplaceAllString(text, "://***@")
}

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
