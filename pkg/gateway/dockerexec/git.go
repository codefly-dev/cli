package dockerexec

import (
	"context"
	"strconv"
	"strings"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (g *Gateway) GitStatus(ctx context.Context, req *gatewayv1.GitStatusRequest) (*gatewayv1.GitStatusResponse, error) {
	if req == nil {
		req = &gatewayv1.GitStatusRequest{}
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	branch, _, _, _ := g.base.run(ctx, "", 10, "git", "rev-parse", "--abbrev-ref", "HEAD")
	stdout, stderr, exitCode, err := g.base.run(ctx, "", 15, "git", "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, status.Errorf(codes.Internal, "git status: %s", strings.TrimSpace(stderr))
	}
	files := make([]*gatewayv1.GitFileStatus, 0)
	for _, line := range strings.Split(stdout, "\n") {
		fileStatus, filePath, staged, ok := parseGitStatusLine(line)
		if !ok {
			continue
		}
		files = append(files, &gatewayv1.GitFileStatus{Path: filePath, Status: fileStatus, Staged: staged})
	}
	return &gatewayv1.GitStatusResponse{Branch: strings.TrimSpace(branch), Files: files}, nil
}

func parseGitStatusLine(line string) (fileStatus, filePath string, staged, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", "", false, false
	}
	if len(line) >= 4 && line[2] == ' ' {
		fileStatus = strings.TrimSpace(line[:2])
		filePath = strings.TrimSpace(line[3:])
		staged = line[0] != ' ' && line[0] != '?'
		return fileStatus, filePath, staged, filePath != ""
	}
	if len(line) >= 3 && line[1] == ' ' {
		fileStatus = strings.TrimSpace(line[:1])
		filePath = strings.TrimSpace(line[2:])
		staged = line[0] != ' ' && line[0] != '?'
		return fileStatus, filePath, staged, filePath != ""
	}
	return "", "", false, false
}

func (g *Gateway) GitDiff(ctx context.Context, req *gatewayv1.GitDiffRequest) (*gatewayv1.GitDiffResponse, error) {
	if req == nil {
		req = &gatewayv1.GitDiffRequest{}
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	args := []string{"git", "diff"}
	if req.GetStaged() {
		args = append(args, "--cached")
	}
	if req.GetPath() != "" {
		args = append(args, "--", req.GetPath())
	}
	stdout, stderr, exitCode, err := g.base.run(ctx, "", 30, args...)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, status.Errorf(codes.Internal, "git diff: %s", strings.TrimSpace(stderr))
	}
	diff := joinUnifiedDiffs(stdout, g.gitDiffUntracked(ctx, req.GetPath()))
	return &gatewayv1.GitDiffResponse{Diff: diff}, nil
}

func (g *Gateway) gitDiffUntracked(ctx context.Context, filterPath string) string {
	args := []string{"git", "ls-files", "--others", "--exclude-standard", "--"}
	if filterPath != "" {
		args = append(args, filterPath)
	}
	stdout, _, _, _ := g.base.run(ctx, "", 15, args...)
	parts := make([]string, 0)
	for _, file := range strings.Split(strings.TrimSpace(stdout), "\n") {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		diff, _, _, _ := g.base.run(ctx, "", 30, "git", "diff", "--no-index", "--", "/dev/null", file)
		if strings.Contains(diff, "Binary files /dev/null and ") {
			continue
		}
		parts = append(parts, diff)
	}
	return joinUnifiedDiffs(parts...)
}

func joinUnifiedDiffs(parts ...string) string {
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	if len(result) == 0 {
		return ""
	}
	return strings.Join(result, "\n") + "\n"
}

func (g *Gateway) GitLog(ctx context.Context, req *gatewayv1.GitLogRequest) (*gatewayv1.GitLogResponse, error) {
	if req == nil {
		req = &gatewayv1.GitLogRequest{}
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	count := req.GetCount()
	if count <= 0 {
		count = 20
	}
	stdout, stderr, exitCode, err := g.base.run(ctx, "", 30,
		"git", "log", "--max-count", strconv.Itoa(int(count)), "--pretty=format:%h|%an|%ad|%s", "--date=iso")
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, status.Errorf(codes.Internal, "git log: %s", strings.TrimSpace(stderr))
	}
	commits := make([]*gatewayv1.GitCommitInfo, 0)
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		commits = append(commits, &gatewayv1.GitCommitInfo{
			ShortHash: parts[0], Author: parts[1], Date: parts[2], Message: parts[3],
		})
	}
	return &gatewayv1.GitLogResponse{Commits: commits}, nil
}
