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

// GitCommit stages req.Paths (all currently-staged changes when empty), commits
// with req.Message, and returns the new commit.
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
	if len(req.Paths) > 0 {
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
