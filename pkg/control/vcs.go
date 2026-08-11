package control

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/core/resources"
)

// This file lifts the VCS group. Git is a generic operation with nothing
// language-specific about it, so — like the Gateway — the control plane runs the
// `git` binary directly (os/exec) against the workspace on the local machine.
// No plugin, no go-git dependency.

// gitDir returns dir when set, else the workspace root — every git op runs there.
func (p *planeImpl) gitDir(ctx context.Context, dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	ws, err := p.workspace(ctx)
	if err != nil {
		return "", err
	}
	return ws.Dir(), nil
}

// git runs a git subcommand in dir and removes only Git's trailing line ending
// from stdout (stderr is folded in on failure for a useful error). Leading
// whitespace is protocol data for commands such as `status --porcelain=v1`:
// trimming it shifts the XY columns and corrupts the first changed path.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitWithEnvironment(ctx, dir, nil, args...)
}

func gitWithEnvironment(ctx context.Context, dir string, environment []string, args ...string) (string, error) {
	return gitWithEnvironmentAndInput(ctx, dir, environment, nil, args...)
}

func gitWithEnvironmentAndInput(ctx context.Context, dir string, environment []string, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if environment != nil {
		cmd.Env = environment
	}
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(out.String(), "\r\n"), nil
}

// GitStatus reports branch, dirty state, changed files, and ahead/behind vs the
// upstream (best-effort — zero when there is no upstream).
func (p *planeImpl) GitStatus(ctx context.Context, dir string) (GitStatus, error) {
	repo, err := p.gitDir(ctx, dir)
	if err != nil {
		return GitStatus{}, err
	}
	return gitStatusAt(ctx, repo)
}

// gitStatusAt / gitDiffAt / gitLogAt / gitCommitAt run against an explicit repo
// dir, shared by the workspace-scoped planeImpl methods and service-scoped
// callers (see scope.go).
func gitStatusAt(ctx context.Context, repo string) (GitStatus, error) {
	repositoryRoot, err := git(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return GitStatus{}, err
	}
	branch, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return GitStatus{}, err
	}
	porcelain, err := git(ctx, repo, "status", "--porcelain=v1")
	if err != nil {
		return GitStatus{}, err
	}
	status := GitStatus{Branch: branch, RepositoryRoot: repositoryRoot}
	for _, line := range strings.Split(porcelain, "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: "XY <path>"; XY is the two status columns, path at 3.
		xy := line[:2]
		path := strings.TrimSpace(line[3:])
		status.Changed = append(status.Changed, path)
		status.Files = append(status.Files, GitFileStatus{
			Path:   path,
			Code:   xy,
			Staged: xy[0] != ' ' && xy[0] != '?',
		})
	}
	status.Dirty = len(status.Changed) > 0
	// Ahead/behind vs upstream — absent upstream is not an error, just zero.
	if counts, err := git(ctx, repo, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); err == nil {
		if fields := strings.Fields(counts); len(fields) == 2 {
			status.Behind, _ = strconv.Atoi(fields[0])
			status.Ahead, _ = strconv.Atoi(fields[1])
		}
	}
	return status, nil
}

// GitDiff returns the working-tree diff (or the staged diff when req.Staged),
// optionally scoped to req.Paths.
func (p *planeImpl) GitDiff(ctx context.Context, req GitDiffRequest) (string, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return "", err
	}
	return gitDiffAt(ctx, repo, req)
}

func gitDiffAt(ctx context.Context, repo string, req GitDiffRequest) (string, error) {
	args := []string{"diff"}
	if req.Staged {
		args = append(args, "--cached")
	}
	if len(req.Paths) > 0 {
		args = append(args, "--")
		args = append(args, req.Paths...)
	}
	return git(ctx, repo, args...)
}

const (
	maxUnifiedPatchBytes = 16 << 20
	unifiedPatchStrategy = "git-apply"
)

// ApplyPatch keeps unified-diff parsing, path handling, and Git command
// construction inside Codefly's VCS control plane. Callers supply mutation
// intent and receive source-free evidence only.
func (p *planeImpl) ApplyPatch(ctx context.Context, req ApplyPatchRequest) (ApplyPatchResult, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return ApplyPatchResult{}, err
	}
	return applyPatchAt(ctx, repo, req)
}

func applyPatchAt(ctx context.Context, repo string, req ApplyPatchRequest) (ApplyPatchResult, error) {
	patch := normalizeUnifiedPatch(req.Patch)
	if len(patch) == 0 {
		return ApplyPatchResult{}, fmt.Errorf("unified patch is required")
	}
	if len(patch) > maxUnifiedPatchBytes {
		return ApplyPatchResult{}, fmt.Errorf("unified patch exceeds %d-byte limit", maxUnifiedPatchBytes)
	}
	args := []string{"apply", "--whitespace=nowarn"}
	if req.Reverse {
		args = append(args, "--reverse")
	}
	pathsOutput, err := gitWithEnvironmentAndInput(ctx, repo, nil, patch, append(args, "--numstat", "-z")...)
	if err != nil {
		return ApplyPatchResult{}, fmt.Errorf("inspect unified patch: %w", err)
	}
	paths, err := parsePatchNumstat(pathsOutput)
	if err != nil {
		return ApplyPatchResult{}, err
	}
	if _, err := gitWithEnvironmentAndInput(ctx, repo, nil, patch, append(args, "--check")...); err != nil {
		return ApplyPatchResult{}, fmt.Errorf("check unified patch: %w", err)
	}
	result := ApplyPatchResult{ChangedFiles: paths, Strategy: unifiedPatchStrategy}
	if req.DryRun {
		return result, nil
	}
	if _, err := gitWithEnvironmentAndInput(ctx, repo, nil, patch, args...); err != nil {
		return ApplyPatchResult{}, fmt.Errorf("apply unified patch: %w", err)
	}
	result.Changed = len(paths) > 0
	return result, nil
}

func normalizeUnifiedPatch(patch string) []byte {
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	patch = strings.ReplaceAll(patch, "\r", "\n")
	if strings.TrimSpace(patch) == "" {
		return nil
	}
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	return []byte(patch)
}

func parsePatchNumstat(output string) ([]string, error) {
	entries := strings.Split(output, "\x00")
	paths := make([]string, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, "\t", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("git apply returned malformed numstat evidence")
		}
		path := fields[2]
		if path == "" {
			// With -z, a rename/copy has an empty path in the header followed by
			// old and new path records. The resulting file is the new identity.
			if i+2 >= len(entries) || entries[i+1] == "" || entries[i+2] == "" {
				return nil, fmt.Errorf("git apply returned incomplete rename evidence")
			}
			path = entries[i+2]
			i += 2
		}
		cleaned := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || filepath.IsAbs(filepath.FromSlash(path)) {
			return nil, fmt.Errorf("git apply returned unsafe changed path")
		}
		paths = append(paths, cleaned)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("unified patch declares no changed files")
	}
	return paths, nil
}

// GitLog returns up to req.Limit commits (default 20, capped at 1000).
func (p *planeImpl) GitLog(ctx context.Context, req GitLogRequest) ([]GitCommit, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return nil, err
	}
	return gitLogAt(ctx, repo, req)
}

func gitLogAt(ctx context.Context, repo string, req GitLogRequest) ([]GitCommit, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	// Unit-separator delimited fields avoid collisions with commit-message text.
	out, err := git(ctx, repo, "log", "--max-count="+strconv.Itoa(limit), "--format=%H%x1f%h%x1f%an%x1f%s%x1f%ai")
	if err != nil {
		return nil, err
	}
	var commits []GitCommit
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\x1f")
		if len(fields) != 5 {
			continue
		}
		commits = append(commits, GitCommit{
			SHA: fields[0], ShortHash: fields[1], Author: fields[2], Message: fields[3], Date: fields[4],
		})
	}
	return commits, nil
}

// GitCommit stages the requested workspace changes, commits with req.Message,
// and returns the new commit.
func (p *planeImpl) GitCommit(ctx context.Context, req GitCommitRequest) (GitCommit, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitCommit{}, err
	}
	return gitCommitAt(ctx, repo, req)
}

func gitCommitAt(ctx context.Context, repo string, req GitCommitRequest) (GitCommit, error) {
	if strings.TrimSpace(req.Message) == "" {
		return GitCommit{}, fmt.Errorf("commit message is required")
	}
	if req.All && len(req.Paths) > 0 {
		return GitCommit{}, fmt.Errorf("commit all and paths are mutually exclusive")
	}
	if req.All {
		if _, err := git(ctx, repo, "add", "--all"); err != nil {
			return GitCommit{}, err
		}
	} else if len(req.Paths) > 0 {
		args := append([]string{"add", "--"}, req.Paths...)
		if _, err := git(ctx, repo, args...); err != nil {
			return GitCommit{}, err
		}
	}
	if _, err := git(ctx, repo, "commit", "-m", req.Message); err != nil {
		return GitCommit{}, err
	}
	sha, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return GitCommit{}, err
	}
	return GitCommit{SHA: sha, Message: req.Message}, nil
}

// GitBranch creates a branch and resolves its exact commit.
func (p *planeImpl) GitBranch(ctx context.Context, req GitBranchRequest) (GitAct, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitAct{}, err
	}
	return gitBranchAt(ctx, repo, req)
}

func gitBranchAt(ctx context.Context, repo string, req GitBranchRequest) (GitAct, error) {
	name := strings.TrimSpace(req.Name)
	if err := validateBranchName(ctx, repo, name); err != nil {
		return GitAct{}, err
	}
	startPoint := strings.TrimSpace(req.StartPoint)
	if startPoint == "" {
		startPoint = "HEAD"
	}
	if err := validateRevision(startPoint); err != nil {
		return GitAct{}, err
	}
	args := []string{"branch"}
	if req.Force {
		args = append(args, "-f")
	}
	args = append(args, "--", name, startPoint)
	if _, err := git(ctx, repo, args...); err != nil {
		return GitAct{}, err
	}
	revision, err := git(ctx, repo, "rev-parse", name+"^{commit}")
	if err != nil {
		return GitAct{}, err
	}
	return GitAct{Target: name, Revision: revision}, nil
}

// GitCheckout switches to an existing ref and resolves the resulting HEAD.
func (p *planeImpl) GitCheckout(ctx context.Context, req GitCheckoutRequest) (GitAct, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitAct{}, err
	}
	return gitCheckoutAt(ctx, repo, req)
}

func gitCheckoutAt(ctx context.Context, repo string, req GitCheckoutRequest) (GitAct, error) {
	ref := strings.TrimSpace(req.Ref)
	if err := validateRevision(ref); err != nil {
		return GitAct{}, err
	}
	args := []string{"checkout"}
	if req.Detach {
		args = append(args, "--detach")
	}
	args = append(args, ref, "--")
	if _, err := git(ctx, repo, args...); err != nil {
		return GitAct{}, err
	}
	branch, err := currentBranch(ctx, repo)
	if err != nil {
		return GitAct{}, err
	}
	revision, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return GitAct{}, err
	}
	return GitAct{Branch: branch, Target: ref, Revision: revision}, nil
}

// GitPush publishes one branch and verifies the remote ref after git accepts it.
func (p *planeImpl) GitPush(ctx context.Context, req GitPushRequest) (GitPushResult, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitPushResult{}, err
	}
	return gitPushAt(ctx, repo, req)
}

func gitPushAt(ctx context.Context, repo string, req GitPushRequest) (GitPushResult, error) {
	remote := strings.TrimSpace(req.Remote)
	if remote == "" {
		remote = "origin"
	}
	if err := validateGitAtom("remote", remote); err != nil {
		return GitPushResult{}, err
	}
	if _, err := git(ctx, repo, "remote", "get-url", "--", remote); err != nil {
		return GitPushResult{}, err
	}
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		var err error
		branch, err = currentBranch(ctx, repo)
		if err != nil {
			return GitPushResult{}, err
		}
	}
	if err := validateBranchName(ctx, repo, branch); err != nil {
		return GitPushResult{}, err
	}
	if req.Mode != GitPushFastForwardOnly && req.Mode != GitPushForceWithLease {
		return GitPushResult{}, fmt.Errorf("git push mode is required")
	}
	revision, err := git(ctx, repo, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return GitPushResult{}, err
	}
	args := []string{"push", "--porcelain"}
	if req.SetUpstream {
		args = append(args, "--set-upstream")
	}
	if req.Mode == GitPushForceWithLease {
		args = append(args, "--force-with-lease")
	}
	refspec := "refs/heads/" + branch + ":refs/heads/" + branch
	args = append(args, "--", remote, refspec)
	if _, err := git(ctx, repo, args...); err != nil {
		return GitPushResult{}, err
	}
	remoteLine, err := git(ctx, repo, "ls-remote", "--exit-code", "--refs", remote, "refs/heads/"+branch)
	if err != nil {
		return GitPushResult{}, fmt.Errorf("verify pushed ref: %w", err)
	}
	fields := strings.Fields(remoteLine)
	if len(fields) < 2 || fields[0] != revision {
		return GitPushResult{}, fmt.Errorf("verify pushed ref: remote %s/%s is %q, want %s", remote, branch, remoteLine, revision)
	}
	return GitPushResult{Remote: remote, Branch: branch, Revision: revision}, nil
}

// GitTag creates an annotated or signed tag and resolves its commit target.
func (p *planeImpl) GitTag(ctx context.Context, req GitTagRequest) (GitAct, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitAct{}, err
	}
	return gitTagAt(ctx, repo, req)
}

func gitTagAt(ctx context.Context, repo string, req GitTagRequest) (GitAct, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return GitAct{}, fmt.Errorf("tag name is required")
	}
	if _, err := git(ctx, repo, "check-ref-format", "refs/tags/"+name); err != nil {
		return GitAct{}, fmt.Errorf("invalid tag name %q: %w", name, err)
	}
	revision := strings.TrimSpace(req.Revision)
	if revision == "" {
		revision = "HEAD"
	}
	if err := validateRevision(revision); err != nil {
		return GitAct{}, err
	}
	message := strings.TrimSpace(req.Message)
	if message == "" {
		return GitAct{}, fmt.Errorf("annotated tag message is required")
	}
	mode := "-a"
	if req.Sign {
		mode = "-s"
	}
	// `--` guards a leading-dash name from being parsed as a git tag option.
	if _, err := git(ctx, repo, "tag", mode, "-m", message, "--", name, revision); err != nil {
		return GitAct{}, err
	}
	resolved, err := git(ctx, repo, "rev-parse", name+"^{commit}")
	if err != nil {
		return GitAct{}, err
	}
	return GitAct{Target: name, Revision: resolved}, nil
}

// GitMerge merges one revision and resolves the receiving branch head.
func (p *planeImpl) GitMerge(ctx context.Context, req GitMergeRequest) (GitAct, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitAct{}, err
	}
	return gitMergeAt(ctx, repo, req)
}

func gitMergeAt(ctx context.Context, repo string, req GitMergeRequest) (GitAct, error) {
	ref := strings.TrimSpace(req.Ref)
	if err := validateRevision(ref); err != nil {
		return GitAct{}, err
	}
	args := []string{"merge", "--no-edit"}
	if req.NoFastForward {
		args = append(args, "--no-ff")
	}
	if message := strings.TrimSpace(req.Message); message != "" {
		args = append(args, "-m", message)
	}
	args = append(args, "--", ref)
	if _, err := git(ctx, repo, args...); err != nil {
		// Keep the act atomic: a conflicted merge leaves the tree mid-merge, so
		// abort (best-effort) before surfacing the failure to the caller.
		_, _ = git(ctx, repo, "merge", "--abort")
		return GitAct{}, err
	}
	branch, err := currentBranch(ctx, repo)
	if err != nil {
		return GitAct{}, err
	}
	revision, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return GitAct{}, err
	}
	return GitAct{Branch: branch, Target: ref, Revision: revision}, nil
}

// GitRevert creates one canonical revert commit and resolves the new head.
func (p *planeImpl) GitRevert(ctx context.Context, req GitRevertRequest) (GitAct, error) {
	repo, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return GitAct{}, err
	}
	return gitRevertAt(ctx, repo, req)
}

func gitRevertAt(ctx context.Context, repo string, req GitRevertRequest) (GitAct, error) {
	revision := strings.TrimSpace(req.Revision)
	if err := validateRevision(revision); err != nil {
		return GitAct{}, err
	}
	reverted, err := git(ctx, repo, "rev-parse", revision+"^{commit}")
	if err != nil {
		return GitAct{}, err
	}
	if _, err := git(ctx, repo, "revert", "--no-edit", "--", revision); err != nil {
		// Keep the act atomic: abort a conflicted/partial revert instead of
		// leaving the sequencer mid-operation for the next act to trip over.
		_, _ = git(ctx, repo, "revert", "--abort")
		return GitAct{}, err
	}
	branch, err := currentBranch(ctx, repo)
	if err != nil {
		return GitAct{}, err
	}
	head, err := git(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return GitAct{}, err
	}
	return GitAct{Branch: branch, Target: reverted, Revision: head}, nil
}

// MaterializeRepositorySnapshot keeps repository transport, Git command
// construction, and ambient-configuration policy inside Codefly. The caller
// supplies identities and relative cache locations; it never implements Git.
func (p *planeImpl) MaterializeRepositorySnapshot(
	ctx context.Context,
	req MaterializeRepositorySnapshotRequest,
) (MaterializedRepositorySnapshot, error) {
	root, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return MaterializedRepositorySnapshot{}, err
	}
	return materializeRepositorySnapshotAt(ctx, root, req)
}

func materializeRepositorySnapshotAt(
	ctx context.Context,
	root string,
	req MaterializeRepositorySnapshotRequest,
) (MaterializedRepositorySnapshot, error) {
	prepared, err := prepareRepositoryRevisionAt(ctx, root, req.RepositoryURL, req.CacheDirectory, req.Revision, req.FetchIdentity, req.RemoteAccess)
	if err != nil {
		return MaterializedRepositorySnapshot{}, err
	}
	snapshotDirectory, err := validateRepositoryRelativeDirectory("snapshot directory", req.SnapshotDirectory)
	if err != nil {
		return MaterializedRepositorySnapshot{}, err
	}
	if repositoryDirectoriesOverlap(prepared.cacheDirectory, snapshotDirectory) {
		return MaterializedRepositorySnapshot{}, fmt.Errorf("repository cache and snapshot directories must not overlap")
	}
	snapshotPath := filepath.Join(root, snapshotDirectory)
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return MaterializedRepositorySnapshot{}, fmt.Errorf("prepare repository snapshot parent: %w", err)
	}
	if _, err := gitWithEnvironment(ctx, prepared.cachePath, prepared.environment, "worktree", "prune"); err != nil {
		return MaterializedRepositorySnapshot{}, fmt.Errorf("prune repository snapshots before materialization: %w", err)
	}
	registered, err := repositoryWorktreeRegistered(ctx, prepared.cachePath, snapshotPath, prepared.environment)
	if err != nil {
		return MaterializedRepositorySnapshot{}, err
	}
	if registered {
		existingRevision, err := gitWithEnvironment(ctx, snapshotPath, prepared.environment, "rev-parse", "HEAD^{commit}")
		if err != nil {
			return MaterializedRepositorySnapshot{}, fmt.Errorf("resolve existing repository snapshot: %w", err)
		}
		if existingRevision != prepared.revision {
			return MaterializedRepositorySnapshot{}, fmt.Errorf("repository snapshot already resolves %s, want %s", existingRevision, prepared.revision)
		}
		return inspectMaterializedRepositorySnapshot(ctx, snapshotPath, prepared.revision, snapshotDirectory)
	}
	if _, err := gitWithEnvironment(ctx, prepared.cachePath, prepared.environment, "worktree", "add", "--detach", snapshotPath, prepared.revision); err != nil {
		return MaterializedRepositorySnapshot{}, fmt.Errorf("create repository snapshot: %w", err)
	}
	result, err := inspectMaterializedRepositorySnapshot(ctx, snapshotPath, prepared.revision, snapshotDirectory)
	if err == nil {
		return result, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, removeErr := gitWithEnvironment(cleanupCtx, prepared.cachePath, prepared.environment, "worktree", "remove", "--force", snapshotPath)
	_, pruneErr := gitWithEnvironment(cleanupCtx, prepared.cachePath, prepared.environment, "worktree", "prune")
	return MaterializedRepositorySnapshot{}, errors.Join(
		err,
		wrapRepositorySnapshotCleanupError("remove unmeasured repository snapshot", removeErr),
		wrapRepositorySnapshotCleanupError("prune unmeasured repository snapshot", pruneErr),
	)
}

func inspectMaterializedRepositorySnapshot(
	ctx context.Context,
	snapshotPath, revision, snapshotDirectory string,
) (MaterializedRepositorySnapshot, error) {
	tree, err := resources.InspectStorageTree(ctx, snapshotPath)
	if err != nil {
		return MaterializedRepositorySnapshot{}, fmt.Errorf("measure repository snapshot storage: %w", err)
	}
	if tree.RequiredBytes == 0 || tree.EntryCount == 0 {
		return MaterializedRepositorySnapshot{}, errors.New("measure repository snapshot storage: materialized tree is empty")
	}
	return MaterializedRepositorySnapshot{
		Revision: revision, SnapshotDirectory: snapshotDirectory,
		EquivalentSnapshotBytes: tree.RequiredBytes,
	}, nil
}

func wrapRepositorySnapshotCleanupError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

type preparedRepositoryRevision struct {
	cacheDirectory string
	cachePath      string
	revision       string
	defaultBranch  string
	environment    []string
}

func prepareRepositoryRevisionAt(
	ctx context.Context,
	root, requestedURL, requestedCacheDirectory, requestedRevision, requestedFetchIdentity string,
	access RepositoryRemoteAccess,
) (preparedRepositoryRevision, error) {
	repositoryURL := strings.TrimSpace(requestedURL)
	environment, err := repositoryRemoteEnvironment(access, repositoryURL)
	if err != nil {
		return preparedRepositoryRevision{}, err
	}
	cacheDirectory, err := validateRepositoryRelativeDirectory("cache directory", requestedCacheDirectory)
	if err != nil {
		return preparedRepositoryRevision{}, err
	}
	revision := strings.TrimSpace(requestedRevision)
	if revision == "" {
		revision = "HEAD"
	}
	if err := validateRevision(revision); err != nil {
		return preparedRepositoryRevision{}, err
	}
	fetchIdentity := strings.TrimSpace(requestedFetchIdentity)
	if !isRepositoryIdentity(fetchIdentity) {
		return preparedRepositoryRevision{}, fmt.Errorf("repository fetch identity %q is not a stable token", fetchIdentity)
	}
	cachePath := filepath.Join(root, cacheDirectory)
	if _, err := gitWithEnvironment(ctx, cachePath, environment, "rev-parse", "--git-dir"); err != nil {
		// rev-parse can fail for reasons other than "not a repository" — a
		// cancelled context or a missing/broken git binary among them. Treating
		// those as "incomplete projection" would delete a perfectly good cache.
		// Never remove anything when the context is already done.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return preparedRepositoryRevision{}, ctxErr
		}
		// The cache directory is a Codefly-owned projection named explicitly by
		// the caller. A cancelled clone or terminated process can leave a
		// non-repository directory behind; remove that incomplete projection and
		// retry the canonical clone instead of wedging every later session.
		// removeIncompleteRepositoryCache refuses to delete a real repository.
		if statErr := removeIncompleteRepositoryCache(cachePath); statErr != nil {
			return preparedRepositoryRevision{}, statErr
		}
		if _, err := gitWithEnvironment(ctx, root, environment, "clone", "--", repositoryURL, cacheDirectory); err != nil {
			return preparedRepositoryRevision{}, fmt.Errorf("clone repository: %w", err)
		}
	}
	configuredURL, err := ensureRepositoryOrigin(ctx, cachePath, environment, repositoryURL)
	if err != nil {
		return preparedRepositoryRevision{}, err
	}
	if configuredURL != repositoryURL {
		return preparedRepositoryRevision{}, fmt.Errorf("repository cache origin %q does not match requested source", configuredURL)
	}
	defaultBranch := ""
	if advertised, branchErr := gitWithEnvironment(ctx, cachePath, environment, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); branchErr == nil {
		defaultBranch = strings.TrimPrefix(advertised, "origin/")
	}
	shallow, err := gitWithEnvironment(ctx, cachePath, environment, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return preparedRepositoryRevision{}, fmt.Errorf("inspect repository depth: %w", err)
	}
	if shallow == "true" {
		if _, err := gitWithEnvironment(ctx, cachePath, environment, "fetch", "--unshallow", "origin"); err != nil {
			return preparedRepositoryRevision{}, fmt.Errorf("unshallow repository: %w", err)
		}
	}

	temporaryRef := "refs/mind/fetch/" + fetchIdentity
	if _, err := gitWithEnvironment(
		ctx,
		cachePath,
		environment,
		"fetch",
		"--no-write-fetch-head",
		"--no-tags",
		"origin",
		revision+":"+temporaryRef,
	); err != nil {
		return preparedRepositoryRevision{}, fmt.Errorf("fetch repository revision: %w", err)
	}
	cleanupTemporaryRef := func() error {
		_, cleanupErr := gitWithEnvironment(ctx, cachePath, environment, "update-ref", "-d", temporaryRef)
		return cleanupErr
	}
	resolved, err := gitWithEnvironment(ctx, cachePath, environment, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return preparedRepositoryRevision{}, errors.Join(fmt.Errorf("resolve repository revision: %w", err), cleanupTemporaryRef())
	}
	if !isHexRevision(resolved) {
		return preparedRepositoryRevision{}, errors.Join(fmt.Errorf("resolved repository revision %q is not a full object ID", resolved), cleanupTemporaryRef())
	}
	const durableHeadRef = "refs/heads/mind/materialized"
	if _, err := gitWithEnvironment(ctx, cachePath, environment, "update-ref", durableHeadRef, resolved); err != nil {
		return preparedRepositoryRevision{}, errors.Join(fmt.Errorf("publish repository cache head: %w", err), cleanupTemporaryRef())
	}
	if _, err := gitWithEnvironment(ctx, cachePath, environment, "symbolic-ref", "HEAD", durableHeadRef); err != nil {
		return preparedRepositoryRevision{}, errors.Join(fmt.Errorf("select repository cache head: %w", err), cleanupTemporaryRef())
	}
	if err := cleanupTemporaryRef(); err != nil {
		return preparedRepositoryRevision{}, fmt.Errorf("release repository fetch ref: %w", err)
	}
	return preparedRepositoryRevision{
		cacheDirectory: cacheDirectory, cachePath: cachePath, revision: resolved,
		defaultBranch: defaultBranch, environment: environment,
	}, nil
}

// ensureRepositoryOrigin repairs only the absent-origin case in a Codefly-owned
// cache projection. The request URL is authoritative when no origin exists;
// an existing origin is returned unchanged so the caller can reject a source
// mismatch instead of silently retargeting project state.
func ensureRepositoryOrigin(ctx context.Context, cachePath string, environment []string, repositoryURL string) (string, error) {
	remotes, err := gitWithEnvironment(ctx, cachePath, environment, "remote")
	if err != nil {
		return "", fmt.Errorf("list repository remotes: %w", err)
	}
	hasOrigin := false
	for _, remote := range strings.Split(remotes, "\n") {
		if strings.TrimSpace(remote) == "origin" {
			hasOrigin = true
			break
		}
	}
	if !hasOrigin {
		if _, addErr := gitWithEnvironment(ctx, cachePath, environment, "remote", "add", "origin", repositoryURL); addErr != nil {
			return "", fmt.Errorf("repair missing repository origin: %w", addErr)
		}
	}
	configuredURL, err := gitWithEnvironment(ctx, cachePath, environment, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("resolve repository origin: %w", err)
	}
	return configuredURL, nil
}

func removeIncompleteRepositoryCache(cachePath string) error {
	info, err := os.Lstat(cachePath)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("inspect incomplete repository cache: %w", err)
	}
	// Only remove a directory that is NOT already a Git repository. If Git
	// metadata is present, rev-parse failed for a reason other than a missing
	// repository (a broken environment, or a corrupt-but-owned checkout), and
	// deleting a real cache would be destructive. Fail loudly instead.
	if info.IsDir() && repositoryCacheHasGitMetadata(cachePath) {
		return fmt.Errorf("repository cache %q looks like a Git repository but could not be inspected; refusing to remove it", cachePath)
	}
	if err := os.RemoveAll(cachePath); err != nil {
		return fmt.Errorf("remove incomplete repository cache: %w", err)
	}
	return nil
}

// repositoryCacheHasGitMetadata reports whether cachePath has the structural
// metadata of a Git repository, using a filesystem probe rather than a git
// command (which can itself fail for the same environmental reasons that made
// rev-parse fail). An interrupted clone can create a .git directory and its
// object scaffolding before publishing .git/HEAD; that is incomplete,
// replaceable cache state rather than a repository that must be preserved.
// A .git file may be a linked-worktree pointer, so preserve it conservatively.
// Bare repositories are recognized by HEAD alongside an objects directory.
func repositoryCacheHasGitMetadata(cachePath string) bool {
	gitMetadata := filepath.Join(cachePath, ".git")
	if info, err := os.Lstat(gitMetadata); err == nil {
		if !info.IsDir() {
			return true
		}
		_, headErr := os.Lstat(filepath.Join(gitMetadata, "HEAD"))
		return headErr == nil
	}
	_, headErr := os.Lstat(filepath.Join(cachePath, "HEAD"))
	_, objectsErr := os.Lstat(filepath.Join(cachePath, "objects"))
	return headErr == nil && objectsErr == nil
}

// PrepareRepositoryCheckout resolves one revision through the typed remote
// boundary, then resets the cache worktree to a clean mutable checkout.
func (p *planeImpl) PrepareRepositoryCheckout(
	ctx context.Context,
	req PrepareRepositoryCheckoutRequest,
) (PreparedRepositoryCheckout, error) {
	root, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return PreparedRepositoryCheckout{}, err
	}
	prepared, err := prepareRepositoryRevisionAt(ctx, root, req.RepositoryURL, req.CacheDirectory, req.Revision, req.FetchIdentity, req.RemoteAccess)
	if err != nil {
		return PreparedRepositoryCheckout{}, err
	}
	if _, err := gitWithEnvironment(ctx, prepared.cachePath, prepared.environment, "checkout", "--force", "--detach", prepared.revision); err != nil {
		return PreparedRepositoryCheckout{}, fmt.Errorf("checkout repository revision: %w", err)
	}
	if _, err := gitWithEnvironment(ctx, prepared.cachePath, prepared.environment, "clean", "-ffdx"); err != nil {
		return PreparedRepositoryCheckout{}, fmt.Errorf("clean repository checkout: %w", err)
	}
	return PreparedRepositoryCheckout{Revision: prepared.revision, DefaultBranch: prepared.defaultBranch}, nil
}

// ReleaseRepositorySnapshot removes a detached worktree and its administrative
// record through the same typed Codefly capability that created it.
func (p *planeImpl) ReleaseRepositorySnapshot(ctx context.Context, req ReleaseRepositorySnapshotRequest) error {
	root, err := p.gitDir(ctx, req.Dir)
	if err != nil {
		return err
	}
	cacheDirectory, err := validateRepositoryRelativeDirectory("cache directory", req.CacheDirectory)
	if err != nil {
		return err
	}
	snapshotDirectory, err := validateRepositoryRelativeDirectory("snapshot directory", req.SnapshotDirectory)
	if err != nil {
		return err
	}
	if repositoryDirectoriesOverlap(cacheDirectory, snapshotDirectory) {
		return fmt.Errorf("repository cache and snapshot directories must not overlap")
	}
	environment := isolatedRepositoryEnvironment()
	cachePath := filepath.Join(root, cacheDirectory)
	snapshotPath := filepath.Join(root, snapshotDirectory)
	registered, err := repositoryWorktreeRegistered(ctx, cachePath, snapshotPath, environment)
	if err != nil {
		return err
	}
	if registered {
		if _, err := gitWithEnvironment(ctx, cachePath, environment, "worktree", "remove", "--force", snapshotPath); err != nil {
			return fmt.Errorf("remove repository snapshot: %w", err)
		}
	} else if _, err := os.Lstat(snapshotPath); !os.IsNotExist(err) {
		if err == nil {
			return fmt.Errorf("repository snapshot path exists but is not registered as a worktree")
		}
		return fmt.Errorf("inspect unregistered repository snapshot: %w", err)
	}
	if _, err := gitWithEnvironment(ctx, cachePath, environment, "worktree", "prune"); err != nil {
		return fmt.Errorf("prune repository snapshots: %w", err)
	}
	return nil
}

func repositoryWorktreeRegistered(ctx context.Context, cachePath, snapshotPath string, environment []string) (bool, error) {
	output, err := gitWithEnvironment(ctx, cachePath, environment, "worktree", "list", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("list repository snapshots: %w", err)
	}
	want, err := canonicalRepositoryWorktreePath(snapshotPath)
	if err != nil {
		return false, fmt.Errorf("resolve repository snapshot path: %w", err)
	}
	for _, line := range strings.Split(output, "\n") {
		path, found := strings.CutPrefix(line, "worktree ")
		if !found {
			continue
		}
		candidate, err := canonicalRepositoryWorktreePath(strings.TrimSpace(path))
		if err == nil && candidate == want {
			return true, nil
		}
	}
	return false, nil
}

func canonicalRepositoryWorktreePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return resolved, nil
	}
	if os.IsNotExist(err) {
		return absolute, nil
	}
	return "", err
}

func repositoryRemoteEnvironment(access RepositoryRemoteAccess, repositoryURL string) ([]string, error) {
	scpLikeSSH := strings.HasPrefix(repositoryURL, "git@") && strings.Contains(repositoryURL, ":")
	parsed, err := url.Parse(repositoryURL)
	if (!scpLikeSSH && err != nil) || repositoryURL == "" || strings.HasPrefix(repositoryURL, "-") || strings.ContainsAny(repositoryURL, "\x00\r\n") {
		return nil, fmt.Errorf("invalid repository URL %q", repositoryURL)
	}
	switch access {
	case RepositoryRemoteAccessPublicHTTPS:
		if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("public repository source must be a credential-free HTTPS URL")
		}
		return isolatedRepositoryEnvironment(), nil
	case RepositoryRemoteAccessConfigured:
		if !scpLikeSSH {
			switch strings.ToLower(parsed.Scheme) {
			case "https":
				if parsed.User != nil {
					return nil, fmt.Errorf("configured HTTPS repository credentials must not be embedded in the URL")
				}
			case "ssh":
				if parsed.User != nil {
					if _, hasPassword := parsed.User.Password(); hasPassword {
						return nil, fmt.Errorf("configured SSH repository credentials must not be embedded in the URL")
					}
				}
			default:
				return nil, fmt.Errorf("configured repository source must declare an HTTPS or SSH transport")
			}
		}
		return nil, nil
	case RepositoryRemoteAccessLocalFile:
		if parsed.Scheme != "file" || (parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost")) ||
			parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, fmt.Errorf("local repository source must be a credential-free file URL")
		}
		return isolatedRepositoryEnvironment(), nil
	default:
		return nil, fmt.Errorf("repository remote access is required")
	}
}

func isolatedRepositoryEnvironment() []string {
	blocked := func(key string) bool {
		upper := strings.ToUpper(key)
		switch upper {
		case "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_COUNT",
			"GIT_CONFIG_PARAMETERS", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "GIT_SSH",
			"GIT_SSH_COMMAND", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
			"GIT_PROXY_COMMAND", "SSH_ASKPASS", "SSH_AUTH_SOCK", "GCM_INTERACTIVE":
			return true
		default:
			return strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_")
		}
	}
	environment := make([]string, 0, len(os.Environ())+8)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked(key) {
			environment = append(environment, entry)
		}
	}
	return append(environment,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=Never",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
	)
}

func validateRepositoryRelativeDirectory(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	clean := filepath.Clean(value)
	if value == "" || clean == "." || clean == ".." || filepath.IsAbs(value) || clean != value ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repository %s %q is not Gateway-relative", name, value)
	}
	return clean, nil
}

func repositoryDirectoriesOverlap(first, second string) bool {
	separator := string(filepath.Separator)
	return first == second || strings.HasPrefix(first, second+separator) || strings.HasPrefix(second, first+separator)
}

func isRepositoryIdentity(value string) bool {
	if value == "" || len(value) > 128 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") || strings.Contains(value, "..") {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return true
}

func isHexRevision(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func currentBranch(ctx context.Context, repo string) (string, error) {
	branch, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	return branch, nil
}

func validateBranchName(ctx context.Context, repo, branch string) error {
	if branch == "" {
		return fmt.Errorf("branch name is required")
	}
	if _, err := git(ctx, repo, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("invalid branch name %q: %w", branch, err)
	}
	return nil
}

func validateRevision(revision string) error {
	if revision == "" {
		return fmt.Errorf("git revision is required")
	}
	return validateGitAtom("revision", revision)
}

func validateGitAtom(name, value string) error {
	if strings.HasPrefix(value, "-") || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("invalid git %s %q", name, value)
	}
	return nil
}
