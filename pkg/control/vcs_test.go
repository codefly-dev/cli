package control

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo creates a real git repo with one commit and returns its dir.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	run("config", "commit.gpgsign", "false")
	run("config", "tag.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-m", "initial")
	return dir
}

func TestGitStatusCleanThenDirty(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	status, err := New().GitStatus(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" {
		t.Errorf("branch = %q, want main", status.Branch)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || status.RepositoryRoot != resolved {
		t.Errorf("repository root = %q, want %q (resolve error: %v)", status.RepositoryRoot, resolved, err)
	}
	if status.Dirty {
		t.Errorf("repo should be clean after commit, got Changed=%v", status.Changed)
	}

	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = New().GitStatus(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Dirty {
		t.Error("repo should be dirty after adding a file")
	}
	found := false
	for _, c := range status.Changed {
		if c == "new.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("Changed = %v, want it to include new.txt", status.Changed)
	}
	// Per-file detail: new.txt is untracked ("??"), not staged.
	var fileOK bool
	for _, f := range status.Files {
		if f.Path == "new.txt" {
			fileOK = true
			if f.Code != "??" || f.Staged {
				t.Errorf("new.txt file status = %+v, want code ?? and not staged", f)
			}
		}
	}
	if !fileOK {
		t.Errorf("Files = %+v, want an entry for new.txt", status.Files)
	}
}

func TestGitStatusReportsContainingRepositoryRootFromNestedDirectory(t *testing.T) {
	dir := initGitRepo(t)
	nested := filepath.Join(dir, "services", "api")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}

	status, err := New().GitStatus(t.Context(), nested)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || status.RepositoryRoot != resolved {
		t.Fatalf("repository root = %q, want %q (resolve error: %v)", status.RepositoryRoot, resolved, err)
	}
}

func TestGitCommitAndLog(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := New().GitCommit(ctx, GitCommitRequest{Dir: dir, Message: "add feature", Paths: []string{"feature.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if commit.SHA == "" {
		t.Error("commit SHA is empty")
	}

	log, err := New().GitLog(ctx, GitLogRequest{Dir: dir, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 2 {
		t.Fatalf("log has %d commits, want 2", len(log))
	}
	if log[0].Message != "add feature" {
		t.Errorf("newest commit message = %q, want %q", log[0].Message, "add feature")
	}
	if log[0].Author != "Test" {
		t.Errorf("author = %q, want Test", log[0].Author)
	}
	if log[0].ShortHash == "" || !strings.HasPrefix(log[0].SHA, log[0].ShortHash) {
		t.Errorf("short hash %q is not a prefix of %q", log[0].ShortHash, log[0].SHA)
	}
	if log[0].Date == "" {
		t.Error("commit date is empty")
	}
}

func TestGitCommitAllWorkspaceChanges(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(dir, "added.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal(err)
	}
	commit, err := New().GitCommit(ctx, GitCommitRequest{Dir: dir, Message: "replace files", All: true})
	if err != nil {
		t.Fatal(err)
	}
	if commit.SHA == "" {
		t.Fatal("commit SHA is empty")
	}
	status, err := New().GitStatus(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatalf("status after complete commit = %+v, want clean", status)
	}
}

func TestTypedGitPublicationLifecycle(t *testing.T) {
	dir := initGitRepo(t)
	ctx := t.Context()
	plane := New()

	branch, err := plane.GitBranch(ctx, GitBranchRequest{Dir: dir, Name: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if branch.Target != "feature" || branch.Revision == "" {
		t.Fatalf("branch result = %+v", branch)
	}
	checkedOut, err := plane.GitCheckout(ctx, GitCheckoutRequest{Dir: dir, Ref: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if checkedOut.Branch != "feature" {
		t.Fatalf("checkout result = %+v", checkedOut)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plane.GitCommit(ctx, GitCommitRequest{Dir: dir, Message: "add feature", Paths: []string{"feature.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := plane.GitCheckout(ctx, GitCheckoutRequest{Dir: dir, Ref: "main"}); err != nil {
		t.Fatal(err)
	}
	merged, err := plane.GitMerge(ctx, GitMergeRequest{Dir: dir, Ref: "feature", NoFastForward: true, Message: "merge feature"})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Branch != "main" || merged.Revision == "" {
		t.Fatalf("merge result = %+v", merged)
	}
	tagged, err := plane.GitTag(ctx, GitTagRequest{Dir: dir, Name: "v1.0.0", Message: "v1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if tagged.Target != "v1.0.0" || tagged.Revision != merged.Revision {
		t.Fatalf("tag result = %+v, merge = %+v", tagged, merged)
	}

	remote := t.TempDir()
	runGit(t, remote, "init", "--bare")
	runGit(t, dir, "remote", "add", "origin", remote)
	pushed, err := plane.GitPush(ctx, GitPushRequest{
		Dir: dir, Remote: "origin", Branch: "main", SetUpstream: true, Mode: GitPushFastForwardOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Remote != "origin" || pushed.Branch != "main" || pushed.Revision != merged.Revision {
		t.Fatalf("push result = %+v, merge = %+v", pushed, merged)
	}

	if err := os.WriteFile(filepath.Join(dir, "temporary.txt"), []byte("temporary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	temporary, err := plane.GitCommit(ctx, GitCommitRequest{Dir: dir, Message: "temporary", Paths: []string{"temporary.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	reverted, err := plane.GitRevert(ctx, GitRevertRequest{Dir: dir, Revision: temporary.SHA})
	if err != nil {
		t.Fatal(err)
	}
	if reverted.Target != temporary.SHA || reverted.Revision == temporary.SHA {
		t.Fatalf("revert result = %+v, temporary = %+v", reverted, temporary)
	}
}

func TestGitBranchForceMovesExistingBranch(t *testing.T) {
	dir := initGitRepo(t)
	ctx := t.Context()
	plane := New()

	original, err := plane.GitBranch(ctx, GitBranchRequest{Dir: dir, Name: "semantic-change"})
	if err != nil {
		t.Fatal(err)
	}
	writeCommit(t, plane, ctx, dir, "README.md", "new head\n", "new head")
	moved, err := plane.GitBranch(ctx, GitBranchRequest{
		Dir: dir, Name: "semantic-change", StartPoint: "HEAD", Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Revision == original.Revision {
		t.Fatalf("forced branch stayed at %s", original.Revision)
	}
}

func TestGitCheckoutDetachDoesNotAttachSymbolicRef(t *testing.T) {
	dir := initGitRepo(t)
	result, err := New().GitCheckout(t.Context(), GitCheckoutRequest{Dir: dir, Ref: "main", Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "HEAD" || result.Revision == "" {
		t.Fatalf("detached checkout = %+v", result)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGitMergeAbortsOnConflict(t *testing.T) {
	dir := initGitRepo(t)
	ctx := t.Context()
	plane := New()

	if _, err := plane.GitBranch(ctx, GitBranchRequest{Dir: dir, Name: "feature"}); err != nil {
		t.Fatal(err)
	}
	writeCommit(t, plane, ctx, dir, "README.md", "main side\n", "main edit")
	if _, err := plane.GitCheckout(ctx, GitCheckoutRequest{Dir: dir, Ref: "feature"}); err != nil {
		t.Fatal(err)
	}
	writeCommit(t, plane, ctx, dir, "README.md", "feature side\n", "feature edit")
	if _, err := plane.GitCheckout(ctx, GitCheckoutRequest{Dir: dir, Ref: "main"}); err != nil {
		t.Fatal(err)
	}

	if _, err := plane.GitMerge(ctx, GitMergeRequest{Dir: dir, Ref: "feature"}); err == nil {
		t.Fatal("expected a merge conflict error")
	}
	// The failed merge must be aborted, not left mid-merge.
	if _, err := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("merge left MERGE_HEAD in place (stat err = %v)", err)
	}
	status, err := gitStatusAt(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatalf("aborted merge left a dirty tree: %+v", status.Files)
	}
}

func TestGitRevertAbortsOnConflict(t *testing.T) {
	dir := initGitRepo(t)
	ctx := t.Context()
	plane := New()

	first := writeCommit(t, plane, ctx, dir, "README.md", "v1\n", "to v1")
	writeCommit(t, plane, ctx, dir, "README.md", "v2\n", "to v2")

	// Reverting the first edit conflicts because the second edit touched the
	// same line.
	if _, err := plane.GitRevert(ctx, GitRevertRequest{Dir: dir, Revision: first.SHA}); err == nil {
		t.Fatal("expected a revert conflict error")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "REVERT_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("revert left REVERT_HEAD in place (stat err = %v)", err)
	}
	status, err := gitStatusAt(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if status.Dirty {
		t.Fatalf("aborted revert left a dirty tree: %+v", status.Files)
	}
}

func TestGitTagRejectsOptionLikeName(t *testing.T) {
	dir := initGitRepo(t)
	ctx := t.Context()
	plane := New()

	// A leading-dash name must never be parsed as a git tag option: git rejects
	// it as an invalid tag name behind the `--` guard rather than acting on it.
	if _, err := plane.GitTag(ctx, GitTagRequest{Dir: dir, Name: "-d", Message: "boom"}); err == nil {
		t.Fatal("expected an option-like tag name to be rejected")
	}
}

func TestPublicHTTPSRepositorySnapshotIgnoresAmbientGitRewrite(t *testing.T) {
	source := initGitRepo(t)
	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repository.git")
	runGit(t, serverRoot, "clone", "--bare", "--", source, bare)
	runGit(t, bare, "update-server-info")

	server := httptest.NewTLSServer(http.FileServer(http.Dir(serverRoot)))
	defer server.Close()
	certificatePath := filepath.Join(t.TempDir(), "server.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSL_CAINFO", certificatePath)

	// Reproduce the observed customer failure: a developer-global insteadOf
	// rule silently changes the declared HTTPS transport before Git connects.
	// The public capability must ignore it without mutating the user's config.
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	maliciousRewrite := "[url \"file:///definitely-not-the-public-repository/\"]\n\tinsteadOf = " + server.URL + "/\n"
	if err := os.WriteFile(globalConfig, []byte(maliciousRewrite), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalConfig)

	revision, err := git(t.Context(), source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	request := MaterializeRepositorySnapshotRequest{
		Dir:               root,
		RepositoryURL:     server.URL + "/repository.git",
		CacheDirectory:    "cache/repository",
		Revision:          revision,
		FetchIdentity:     "public-isolation-test",
		SnapshotDirectory: "worktrees/repository/lease",
		RemoteAccess:      RepositoryRemoteAccessPublicHTTPS,
	}
	result, err := New().MaterializeRepositorySnapshot(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != revision || result.SnapshotDirectory != "worktrees/repository/lease" {
		t.Fatalf("materialized snapshot = %+v, want revision %s", result, revision)
	}
	retried, err := New().MaterializeRepositorySnapshot(t.Context(), request)
	if err != nil || retried != result {
		t.Fatalf("retried materialization = %+v, want %+v (error = %v)", retried, result, err)
	}
	body, err := os.ReadFile(filepath.Join(root, result.SnapshotDirectory, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello\n" {
		t.Fatalf("snapshot README = %q", body)
	}
	release := ReleaseRepositorySnapshotRequest{
		Dir: root, CacheDirectory: "cache/repository", SnapshotDirectory: result.SnapshotDirectory,
	}
	if err := New().ReleaseRepositorySnapshot(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	if err := New().ReleaseRepositorySnapshot(t.Context(), release); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, result.SnapshotDirectory)); !os.IsNotExist(err) {
		t.Fatalf("released snapshot still exists (stat error = %v)", err)
	}
	checkoutRequest := PrepareRepositoryCheckoutRequest{
		Dir: root, RepositoryURL: request.RepositoryURL, CacheDirectory: "mutable/repository",
		Revision: revision, FetchIdentity: "public-checkout-test", RemoteAccess: RepositoryRemoteAccessPublicHTTPS,
	}
	checkout, err := New().PrepareRepositoryCheckout(t.Context(), checkoutRequest)
	if err != nil || checkout.Revision != revision || checkout.DefaultBranch != "main" {
		t.Fatalf("prepared checkout = %+v, want revision %s on main (error = %v)", checkout, revision, err)
	}
	junk := filepath.Join(root, checkoutRequest.CacheDirectory, "generated.tmp")
	if err := os.WriteFile(junk, []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New().PrepareRepositoryCheckout(t.Context(), checkoutRequest); err != nil {
		t.Fatalf("retry mutable checkout: %v", err)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatalf("prepared checkout retained generated file (stat error = %v)", err)
	}
	corruptRequest := checkoutRequest
	corruptRequest.CacheDirectory = "interrupted/repository"
	corruptRequest.FetchIdentity = "public-interrupted-checkout-test"
	corruptPath := filepath.Join(root, corruptRequest.CacheDirectory)
	if err := os.MkdirAll(corruptPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptPath, "partial-clone.tmp"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := New().PrepareRepositoryCheckout(t.Context(), corruptRequest)
	if err != nil || recovered.Revision != revision {
		t.Fatalf("recovered interrupted checkout = %+v, want revision %s (error = %v)", recovered, revision, err)
	}
	if _, err := os.Stat(filepath.Join(corruptPath, "partial-clone.tmp")); !os.IsNotExist(err) {
		t.Fatalf("recovered checkout retained interrupted clone bytes (stat error = %v)", err)
	}
}

func TestPrepareRepositoryCheckoutRepairsMissingOrigin(t *testing.T) {
	source := initGitRepo(t)
	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repository.git")
	runGit(t, serverRoot, "clone", "--bare", "--", source, bare)
	revision, err := git(t.Context(), source, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	request := PrepareRepositoryCheckoutRequest{
		Dir: root, RepositoryURL: "file://" + bare, CacheDirectory: "cache/repository",
		Revision: revision, FetchIdentity: "missing-origin-repair", RemoteAccess: RepositoryRemoteAccessLocalFile,
	}
	if _, err := New().PrepareRepositoryCheckout(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(root, request.CacheDirectory)
	runGit(t, cachePath, "remote", "remove", "origin")
	junk := filepath.Join(cachePath, "generated.tmp")
	if err := os.WriteFile(junk, []byte("remove me"), 0o600); err != nil {
		t.Fatal(err)
	}

	repaired, err := New().PrepareRepositoryCheckout(t.Context(), request)
	if err != nil || repaired.Revision != revision {
		t.Fatalf("repaired checkout = %+v, want revision %s (error = %v)", repaired, revision, err)
	}
	configuredURL, err := git(t.Context(), cachePath, "config", "--get", "remote.origin.url")
	if err != nil {
		t.Fatalf("read repaired origin: %v", err)
	}
	if configuredURL != request.RepositoryURL {
		t.Fatalf("repaired origin = %q, want %q", configuredURL, request.RepositoryURL)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatalf("repaired checkout retained generated file (stat error = %v)", err)
	}
	if _, err := New().PrepareRepositoryCheckout(t.Context(), request); err != nil {
		t.Fatalf("retry repaired checkout: %v", err)
	}

	runGit(t, cachePath, "remote", "set-url", "origin", "file://"+filepath.Join(serverRoot, "different.git"))
	if _, err := New().PrepareRepositoryCheckout(t.Context(), request); err == nil || !strings.Contains(err.Error(), "does not match requested source") {
		t.Fatalf("mismatched origin error = %v, want source mismatch", err)
	}
}

func writeCommit(t *testing.T, plane Plane, ctx context.Context, dir, name, content, message string) GitCommit {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	commit, err := plane.GitCommit(ctx, GitCommitRequest{Dir: dir, Message: message, Paths: []string{name}})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// TestRemoveIncompleteRepositoryCacheRefusesGitRepository proves the cache
// recovery never deletes a real repository. rev-parse can fail for reasons
// other than "not a repository" (a broken environment, a missing git binary);
// the recovery must fail loudly rather than nuke an owned checkout, while still
// cleaning up a genuinely non-repository directory left by an interrupted clone.
func TestRemoveIncompleteRepositoryCacheRefusesGitRepository(t *testing.T) {
	repo := initGitRepo(t)
	if err := removeIncompleteRepositoryCache(repo); err == nil {
		t.Fatal("expected removal to be refused for a real git repository")
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatalf("real repository metadata was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("real repository content was removed: %v", err)
	}

	junk := t.TempDir()
	if err := os.WriteFile(filepath.Join(junk, "partial-clone.tmp"), []byte("interrupted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeIncompleteRepositoryCache(junk); err != nil {
		t.Fatalf("expected a non-repository directory to be removable: %v", err)
	}
	if _, err := os.Stat(junk); !os.IsNotExist(err) {
		t.Fatalf("non-repository cache was not removed (stat err = %v)", err)
	}
}

// TestPrepareRepositoryCheckoutCancelledContextPreservesCache proves a
// cancelled context — whose rev-parse failure looks identical to "not a
// repository" — never deletes a valid cache and returns the context error.
func TestPrepareRepositoryCheckoutCancelledContextPreservesCache(t *testing.T) {
	source := initGitRepo(t)
	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repository.git")
	runGit(t, serverRoot, "clone", "--bare", "--", source, bare)

	root := t.TempDir()
	request := PrepareRepositoryCheckoutRequest{
		Dir:            root,
		RepositoryURL:  "file://" + bare,
		CacheDirectory: "cache/repository",
		FetchIdentity:  "cancelled-context-test",
		RemoteAccess:   RepositoryRemoteAccessLocalFile,
	}
	if _, err := New().PrepareRepositoryCheckout(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(root, "cache", "repository")
	if _, err := os.Stat(filepath.Join(cachePath, ".git")); err != nil {
		t.Fatalf("cache was not populated: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().PrepareRepositoryCheckout(ctx, request)
	if err == nil {
		t.Fatal("expected a context error from a cancelled checkout")
	}
	if _, statErr := os.Stat(filepath.Join(cachePath, ".git")); statErr != nil {
		t.Fatalf("cancelled checkout deleted the valid cache: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cachePath, "README.md")); statErr != nil {
		t.Fatalf("cancelled checkout deleted cache content: %v", statErr)
	}
}
