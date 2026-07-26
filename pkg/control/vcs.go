package control

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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

// git runs a git subcommand in dir and returns trimmed stdout (stderr folded in
// on failure for a useful error).
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
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
	return strings.TrimSpace(out.String()), nil
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
	branch, err := git(ctx, repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return GitStatus{}, err
	}
	porcelain, err := git(ctx, repo, "status", "--porcelain=v1")
	if err != nil {
		return GitStatus{}, err
	}
	status := GitStatus{Branch: branch}
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
