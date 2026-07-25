package publish_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Masterminds/semver"
	"github.com/stretchr/testify/require"

	"github.com/codefly-dev/cli/cmd/publish"
)

// writeManifest is a small helper for the four manifest shapes.
// Returns the absolute path so tests can assert on it.
func writeManifest(t *testing.T, dir, relPath, version string) string {
	t.Helper()
	target := filepath.Join(dir, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(target,
		[]byte("version: "+version+"\n"), 0o600))
	return target
}

// initBareGitRepo creates a minimal git repo at dir with a single
// commit on main and an empty origin remote. Used to satisfy the
// release engine's pre-flight gates.
func initBareGitRepo(t *testing.T, dir string) (origin string) {
	t.Helper()
	originDir := t.TempDir()
	run := func(workDir string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir = workDir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v failed:\n%s", args, out)
	}

	// Bare origin
	run(originDir, "init", "--bare", "-b", "main")
	// Working repo
	run(dir, "init", "-b", "main")
	run(dir, "remote", "add", "origin", originDir)
	// gpgsign off — tests run on machines without keys configured.
	run(dir, "config", "commit.gpgsign", "false")
	run(dir, "config", "tag.gpgsign", "false")
	run(dir, "config", "user.email", "test@example.com")
	run(dir, "config", "user.name", "Test")

	// Initial commit + push so the working repo's main has an upstream.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("init\n"), 0o600))
	run(dir, "add", "seed.txt")
	run(dir, "commit", "-m", "seed")
	run(dir, "push", "-u", "origin", "main")
	return originDir
}

// --- Detect tests --------------------------------------------------

func TestDetect_AgentManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")

	m, err := publish.Detect(dir)
	require.NoError(t, err)
	require.Equal(t, publish.ModeAgent, m.Mode)
	require.Equal(t, "0.1.0", m.Version.String())
	require.Equal(t, "v0.1.0", m.Tag())
}

func TestDetect_CoreModuleManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "version/info.codefly.yaml", "1.2.3")

	m, err := publish.Detect(dir)
	require.NoError(t, err)
	require.Equal(t, publish.ModeCoreModule, m.Mode)
	require.Equal(t, "1.2.3", m.Version.String())
}

func TestDetect_CLIManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "pkg/cli/info.yaml", "0.0.99")

	m, err := publish.Detect(dir)
	require.NoError(t, err)
	require.Equal(t, publish.ModeCLI, m.Mode)
}

func TestDetect_StandaloneModuleManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "info.codefly.yaml", "2.0.0")

	m, err := publish.Detect(dir)
	require.NoError(t, err)
	require.Equal(t, publish.ModeStandaloneModule, m.Mode)
}

func TestDetect_NoManifest_ReturnsActionableError(t *testing.T) {
	dir := t.TempDir()
	_, err := publish.Detect(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no codefly manifest",
		"missing manifest must surface a clear, actionable error message")
}

func TestDetect_MultipleManifests_IsConfigError(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	writeManifest(t, dir, "info.codefly.yaml", "0.1.0")

	_, err := publish.Detect(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "multiple manifests",
		"ambiguous detection must fail loud — silent picking is the source of every drift bug")
}

func TestDetect_InvalidSemver_FailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "agent.codefly.yaml", "not-a-version")

	_, err := publish.Detect(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid semver")
}

// --- Bump tests ----------------------------------------------------

func TestBump_PatchMinorMajor(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "agent.codefly.yaml", "1.2.3")
	m, err := publish.Detect(dir)
	require.NoError(t, err)

	for _, tc := range []struct {
		bump string
		want string
	}{
		{"patch", "1.2.4"},
		{"minor", "1.3.0"},
		{"major", "2.0.0"},
		{"", "1.2.4"}, // default = patch
	} {
		next, err := m.Bump(tc.bump)
		require.NoError(t, err, "bump %q", tc.bump)
		require.Equal(t, tc.want, next.String(), "bump %q must produce %s", tc.bump, tc.want)
	}
}

func TestBump_InvalidType_FailsLoud(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, "agent.codefly.yaml", "1.0.0")
	m, _ := publish.Detect(dir)
	_, err := m.Bump("revolutionary")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid bump type")
}

// --- WriteVersion: minimum-diff edit -------------------------------

func TestWriteVersion_MinimumDiff(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "agent.codefly.yaml")
	// Manifest with non-version content + comments — must be preserved.
	require.NoError(t, os.WriteFile(target, []byte(`# this manifest is sacred
publisher: codefly.dev
kind: codefly:service
name: testagent
version: 0.1.0
# end
`), 0o600))

	m, err := publish.Detect(dir)
	require.NoError(t, err)

	next, _ := semver.NewVersion("0.1.1")
	require.NoError(t, m.WriteVersion(next))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, `# this manifest is sacred
publisher: codefly.dev
kind: codefly:service
name: testagent
version: 0.1.1
# end
`, string(got),
		"WriteVersion must replace ONLY the version line, preserving comments + sibling fields verbatim")
}

func TestWriteVersion_PreservesTrailingComment(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "info.codefly.yaml")
	require.NoError(t, os.WriteFile(target,
		[]byte("version: 0.0.1 # tracked manually\n"), 0o600))

	m, err := publish.Detect(dir)
	require.NoError(t, err)

	next, _ := semver.NewVersion("0.0.2")
	require.NoError(t, m.WriteVersion(next))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "version: 0.0.2 # tracked manually\n", string(got),
		"trailing comment after the value must survive the rewrite")
}

// --- Engine: dry-run path -------------------------------------------

func TestEngine_Release_DryRun_NoSideEffects(t *testing.T) {
	dir := t.TempDir()
	initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	m, err := publish.Detect(dir)
	require.NoError(t, err)

	engine := &publish.Engine{
		Manifest: m,
		BumpType: "patch",
		DryRun:   true,
		WorkDir:  dir,
	}

	tag, err := engine.Release(context.Background())
	require.NoError(t, err)
	require.Equal(t, "v0.1.1", tag, "dry-run must compute the would-be tag")

	// Manifest must NOT be modified.
	got, _ := os.ReadFile(target)
	require.Equal(t, "version: 0.1.0\n", string(got),
		"dry-run must leave the manifest untouched")

	// Tag must NOT exist.
	out, _ := exec.Command("git", "-C", dir, "tag", "-l", tag).Output()
	require.Empty(t, out, "dry-run must not create the tag")
}

// --- Engine: full release path -------------------------------------

func TestEngine_Release_FullFlow(t *testing.T) {
	dir := t.TempDir()
	origin := initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	m, err := publish.Detect(dir)
	require.NoError(t, err)

	engine := &publish.Engine{
		Manifest: m,
		BumpType: "minor",
		WorkDir:  dir,
	}

	tag, err := engine.Release(context.Background())
	require.NoError(t, err, "full flow must succeed against a clean repo")
	require.Equal(t, "v0.2.0", tag)

	// Manifest is bumped.
	got, _ := os.ReadFile(target)
	require.Equal(t, "version: 0.2.0\n", string(got))

	// Tag exists locally.
	out, err := exec.Command("git", "-C", dir, "tag", "-l", tag).Output()
	require.NoError(t, err)
	require.Contains(t, string(out), tag)

	// Tag exists on origin.
	out, err = exec.Command("git", "-C", origin, "tag", "-l", tag).Output()
	require.NoError(t, err)
	require.Contains(t, string(out), tag, "tag must have been pushed to origin")
}

// --- Engine: pre-flight gates --------------------------------------

func TestEngine_Release_AbortsOnDirtyTree(t *testing.T) {
	dir := t.TempDir()
	initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	// Dirty the tree.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scratch.txt"),
		[]byte("uncommitted\n"), 0o600))

	m, _ := publish.Detect(dir)
	engine := &publish.Engine{Manifest: m, BumpType: "patch", WorkDir: dir}

	_, err := engine.Release(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "uncommitted changes",
		"dirty tree must be the FIRST pre-flight failure surfaced")
}

func TestEngine_Release_AbortsOnNonMainBranch(t *testing.T) {
	dir := t.TempDir()
	initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	// Switch to a non-main branch.
	require.NoError(t, runGit(dir, "checkout", "-b", "feature"))

	m, _ := publish.Detect(dir)
	engine := &publish.Engine{Manifest: m, BumpType: "patch", WorkDir: dir}

	_, err := engine.Release(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "not on main",
		"non-main branch must be refused with a clear message")
}

// TestEngine_Release_ReconcilesPastExistingTag pins the bump-from-latest-tag
// behavior: the manifest (0.1.0) lags behind the highest existing tag
// (v0.1.1), so a naive patch bump would re-create v0.1.1 and collide. The
// engine must instead reconcile the bump base up to the latest tag and cut the
// next free tag (v0.1.2), leaving the existing tag untouched. We NEVER
// overwrite an existing tag — we skip past it.
func TestEngine_Release_ReconcilesPastExistingTag(t *testing.T) {
	dir := t.TempDir()
	origin := initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	// Pre-create the tag a naive manifest-only bump would collide with.
	require.NoError(t, runGit(dir, "tag", "v0.1.1"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))
	require.NoError(t, runGit(dir, "push", "origin", "v0.1.1"))

	m, _ := publish.Detect(dir)
	engine := &publish.Engine{Manifest: m, BumpType: "patch", WorkDir: dir}

	tag, err := engine.Release(context.Background())
	require.NoError(t, err, "must reconcile past the existing tag, not abort")
	require.Equal(t, "v0.1.2", tag,
		"bump base reconciles to the latest tag (v0.1.1) → next free tag is v0.1.2")

	// The pre-existing tag must be left exactly where it was — never overwritten.
	out, err := exec.Command("git", "-C", origin, "rev-parse", "v0.1.1").Output()
	require.NoError(t, err, "v0.1.1 must still exist on origin")
	preExisting := string(out)

	out, err = exec.Command("git", "-C", dir, "rev-parse", "v0.1.1").Output()
	require.NoError(t, err)
	require.Equal(t, preExisting, string(out),
		"the existing v0.1.1 tag must point at its original commit — never moved")

	// And the new tag landed on origin.
	out, err = exec.Command("git", "-C", origin, "tag", "-l", "v0.1.2").Output()
	require.NoError(t, err)
	require.Contains(t, string(out), "v0.1.2", "the reconciled tag must be pushed to origin")
}

// --- Engine: release hooks -----------------------------------------

// TestEngine_Release_BeforeCommitFailure_RevertsAndPushesNothing pins the
// abort contract the agent-release gate relies on: if BeforeCommit fails
// (e.g. release CI failed or a required platform is missing), the manifest
// bump is reverted and no commit, tag, or push happens.
func TestEngine_Release_BeforeCommitFailure_RevertsAndPushesNothing(t *testing.T) {
	dir := t.TempDir()
	origin := initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	m, err := publish.Detect(dir)
	require.NoError(t, err)

	engine := &publish.Engine{
		Manifest: m,
		BumpType: "patch",
		WorkDir:  dir,
		BeforeCommit: func(context.Context, string) error {
			return errCIFailed
		},
	}

	_, err = engine.Release(context.Background())
	require.ErrorIs(t, err, errCIFailed)

	// Manifest reverted to the committed version.
	got, _ := os.ReadFile(target)
	require.Equal(t, "version: 0.1.0\n", string(got),
		"a failed BeforeCommit must restore the manifest verbatim")

	// No tag anywhere.
	out, _ := exec.Command("git", "-C", dir, "tag", "-l").Output()
	require.Empty(t, strings.TrimSpace(string(out)), "no tag must be created locally")
	out, _ = exec.Command("git", "-C", origin, "tag", "-l").Output()
	require.Empty(t, strings.TrimSpace(string(out)), "no tag must be pushed to origin")

	// Clean tree — nothing committed.
	out, _ = exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	require.Empty(t, strings.TrimSpace(string(out)), "working tree must be clean after abort")
}

// TestEngine_Release_AfterPushFailure_ReturnsLiveTag confirms the tag is
// reported as shipped even when the post-push upload step fails — the tag
// is live and must not be silently swallowed.
func TestEngine_Release_AfterPushFailure_ReturnsLiveTag(t *testing.T) {
	dir := t.TempDir()
	origin := initBareGitRepo(t, dir)
	target := writeManifest(t, dir, "agent.codefly.yaml", "0.1.0")
	require.NoError(t, addAndCommit(dir, target, "add manifest"))
	require.NoError(t, runGit(dir, "push", "origin", "main"))

	m, err := publish.Detect(dir)
	require.NoError(t, err)

	var beforeCalled bool
	engine := &publish.Engine{
		Manifest: m,
		BumpType: "patch",
		WorkDir:  dir,
		BeforeCommit: func(context.Context, string) error {
			beforeCalled = true
			return nil
		},
		AfterPush: func(context.Context, string) error {
			return errUploadFailed
		},
	}

	tag, err := engine.Release(context.Background())
	require.ErrorIs(t, err, errUploadFailed)
	require.Equal(t, "v0.1.1", tag, "the live tag must be returned alongside the upload error")
	require.True(t, beforeCalled, "BeforeCommit must run before the tag is pushed")

	// The tag really was pushed — the failure is upload-only.
	out, err := exec.Command("git", "-C", origin, "tag", "-l", tag).Output()
	require.NoError(t, err)
	require.Contains(t, string(out), tag)
}

var (
	errCIFailed     = errSentinel("release CI failed")
	errUploadFailed = errSentinel("upload failed")
)

type errSentinel string

func (e errSentinel) Error() string { return string(e) }

// --- Helpers --------------------------------------------------------

func runGit(dir string, args ...string) error {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := c.CombinedOutput()
	if err != nil {
		return &execErr{args: args, out: string(out), err: err}
	}
	return nil
}

func addAndCommit(dir, file, msg string) error {
	if err := runGit(dir, "add", file); err != nil {
		return err
	}
	return runGit(dir, "commit", "-m", msg)
}

type execErr struct {
	args []string
	out  string
	err  error
}

func (e *execErr) Error() string {
	return "git " + e.args[0] + ": " + e.err.Error() + "\n" + e.out
}
