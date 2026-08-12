package dockerexec

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const missingPathError = "missing path"

func (g *Gateway) ReadFile(ctx context.Context, req *gatewayv1.ReadFileRequest) (*gatewayv1.ReadFileResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "read file request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	file, err := g.base.resolve(req.GetPath())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	stdout, _, exitCode, err := g.base.run(ctx, "", 15, "cat", "--", file)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return &gatewayv1.ReadFileResponse{Exists: false}, nil
	}
	return &gatewayv1.ReadFileResponse{Exists: true, Content: stdout}, nil
}

func (g *Gateway) WriteFile(ctx context.Context, req *gatewayv1.WriteFileRequest) (*gatewayv1.WriteFileResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "write file request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetPath()) == "" {
		return &gatewayv1.WriteFileResponse{Success: false, Error: missingPathError}, nil
	}
	if err := g.write(ctx, req.GetPath(), req.GetContent()); err != nil {
		return &gatewayv1.WriteFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.WriteFileResponse{Success: true}, nil
}

func (g *Gateway) CreateFile(ctx context.Context, req *gatewayv1.CreateFileRequest) (*gatewayv1.CreateFileResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "create file request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetPath()) == "" {
		return &gatewayv1.CreateFileResponse{Success: false, Error: missingPathError}, nil
	}
	file, err := g.base.resolve(req.GetPath())
	if err != nil {
		return &gatewayv1.CreateFileResponse{Success: false, Error: err.Error()}, nil
	}
	_, _, exitCode, err := g.base.run(ctx, "", 5, "test", "!", "-e", file)
	if err != nil {
		return &gatewayv1.CreateFileResponse{Success: false, Error: err.Error()}, nil
	}
	if exitCode != 0 {
		return &gatewayv1.CreateFileResponse{Success: false, Error: "file already exists"}, nil
	}
	if err := g.write(ctx, req.GetPath(), req.GetContent()); err != nil {
		return &gatewayv1.CreateFileResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.CreateFileResponse{Success: true}, nil
}

func (g *Gateway) DeleteFile(ctx context.Context, req *gatewayv1.DeleteFileRequest) (*gatewayv1.DeleteFileResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "delete file request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetPath()) == "" {
		return &gatewayv1.DeleteFileResponse{Success: false, Error: missingPathError}, nil
	}
	file, err := g.base.resolve(req.GetPath())
	if err != nil {
		return &gatewayv1.DeleteFileResponse{Success: false, Error: err.Error()}, nil
	}
	_, stderr, exitCode, err := g.base.run(ctx, "", 10, "rm", "-f", "--", file)
	if err != nil {
		return &gatewayv1.DeleteFileResponse{Success: false, Error: err.Error()}, nil
	}
	if exitCode != 0 {
		return &gatewayv1.DeleteFileResponse{Success: false, Error: strings.TrimSpace(stderr)}, nil
	}
	return &gatewayv1.DeleteFileResponse{Success: true}, nil
}

func (g *Gateway) ApplyEdit(ctx context.Context, req *gatewayv1.ApplyEditRequest) (*gatewayv1.ApplyEditResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "apply edit request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	read, err := g.ReadFile(ctx, &gatewayv1.ReadFileRequest{Service: req.GetService(), Path: req.GetFile()})
	if err != nil || !read.GetExists() {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: "file not found"}, nil
	}
	count := strings.Count(read.GetContent(), req.GetFind())
	if count == 0 {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: "find text not present"}, nil
	}
	if count > 1 {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: fmt.Sprintf("find text appears %d times — narrow it", count)}, nil
	}
	updated := strings.Replace(read.GetContent(), req.GetFind(), req.GetReplace(), 1)
	if err := g.write(ctx, req.GetFile(), updated); err != nil {
		return &gatewayv1.ApplyEditResponse{Success: false, Error: err.Error()}, nil
	}
	return &gatewayv1.ApplyEditResponse{Success: true}, nil
}

// ListFiles uses the POSIX find + stat surface available in both BusyBox and
// GNU userlands. The previous GNU-only `find -printf` implementation made the
// documented Alpine terminal environment look empty.
func (g *Gateway) ListFiles(ctx context.Context, req *gatewayv1.ListFilesRequest) (*gatewayv1.ListFilesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "list files request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	root, err := g.base.resolve(req.GetPath())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	const delimiter = "\x1f"
	args := []string{"find", root, "-mindepth", "1"}
	if !req.GetRecursive() {
		args = append(args, "-maxdepth", "1")
	}
	args = appendFindDirectoryPrune(args)
	args = append(args, "-exec", "stat", "-c", "%n"+delimiter+"%F"+delimiter+"%s", "{}", "+")
	stdout, stderr, exitCode, err := g.base.run(ctx, "", 30, args...)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, status.Errorf(codes.Internal, "list files: %s", strings.TrimSpace(stderr))
	}
	extensions := make(map[string]struct{}, len(req.GetExtensions()))
	for _, extension := range req.GetExtensions() {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		extensions[extension] = struct{}{}
	}
	files := make([]*gatewayv1.FileInfo, 0)
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		parts := strings.SplitN(line, delimiter, 3)
		if len(parts) != 3 {
			continue
		}
		isDirectory := parts[1] == "directory"
		if !isDirectory && len(extensions) > 0 {
			if _, ok := extensions[path.Ext(parts[0])]; !ok {
				continue
			}
		}
		size, _ := strconv.ParseInt(parts[2], 10, 64)
		files = append(files, &gatewayv1.FileInfo{
			Path: g.base.relative(parts[0]), IsDirectory: isDirectory, SizeBytes: size,
		})
	}
	return &gatewayv1.ListFilesResponse{Files: files}, nil
}

func (g *Gateway) Search(ctx context.Context, req *gatewayv1.SearchRequest) (*gatewayv1.SearchResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "search request is required")
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	target, err := g.base.resolve(req.GetPath())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Drive grep through a portable find expression instead of recursive grep:
	// BusyBox has no --exclude-dir, and searching .git/object storage or build
	// outputs both pollutes results and wastes the objective's context budget.
	args := appendFindDirectoryPrune([]string{"find", target})
	args = append(args, "-type", "f", "-exec", "grep", "-Hn")
	if req.GetCaseInsensitive() {
		args = append(args, "-i")
	}
	if req.GetLiteral() {
		args = append(args, "-F")
	} else {
		args = append(args, "-E")
	}
	args = append(args, "--", req.GetPattern(), "{}", "+")
	stdout, stderr, exitCode, err := g.base.run(ctx, "", 30, args...)
	if err != nil {
		return nil, err
	}
	if exitCode > 1 {
		return nil, status.Errorf(codes.Internal, "search: %s", strings.TrimSpace(stderr))
	}
	limit := int(req.GetMaxResults())
	if limit <= 0 {
		limit = 100
	}
	matches := make([]*gatewayv1.SearchMatch, 0)
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		lineNumber, parseErr := strconv.ParseInt(parts[1], 10, 32)
		if parseErr != nil {
			continue
		}
		matches = append(matches, &gatewayv1.SearchMatch{
			File: g.base.relative(parts[0]), Line: int32(lineNumber), Text: parts[2],
		})
		if len(matches) >= limit {
			break
		}
	}
	return &gatewayv1.SearchResponse{Matches: matches, Truncated: len(matches) >= limit}, nil
}

var skippedContainerDirectories = []string{
	".git", ".hg", ".svn", "node_modules", "vendor", "__pycache__", "dist", "build", "target", ".cache",
}

// appendFindDirectoryPrune adds one BusyBox/GNU-compatible prune branch and a
// trailing -o so the caller can append its real action. Pruning prevents large
// repository metadata and generated trees from being traversed at all.
func appendFindDirectoryPrune(args []string) []string {
	args = append(args, "(")
	for index, directory := range skippedContainerDirectories {
		if index > 0 {
			args = append(args, "-o")
		}
		args = append(args, "-name", directory)
	}
	return append(args, ")", "-prune", "-o")
}

func (g *Gateway) write(ctx context.Context, requestedPath, content string) error {
	file, err := g.base.resolve(requestedPath)
	if err != nil {
		return err
	}
	_, stderr, exitCode, mkdirErr := g.base.run(ctx, "", 10, "mkdir", "-p", "--", path.Dir(file))
	if mkdirErr != nil {
		return mkdirErr
	} else if exitCode != 0 {
		return fmt.Errorf("create parent directory: %s", strings.TrimSpace(stderr))
	}
	_, writeStderr, writeExitCode, writeErr := g.base.runStdin(ctx, "", 30, []byte(content), "sh", "-c", `cat > "$1"`, "codefly-write", file)
	if writeErr != nil {
		return writeErr
	}
	if writeExitCode != 0 {
		return fmt.Errorf("write file: %s", strings.TrimSpace(writeStderr))
	}
	return nil
}
