package dockerexec

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// TestGatewayAlpineEndToEnd proves the container transport against the real
// Docker daemon and a BusyBox userland. This is intentionally not a mock: the
// portability regression it protects (`find -printf`) only appears in a real
// terminal container.
func TestGatewayAlpineEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("Docker CLI is required: %v", err)
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Fatalf("Docker daemon is required: %v", err)
	}
	output, err := exec.Command("docker", "run", "-d", "--rm", "alpine:3.21", "sleep", "3600").Output()
	if err != nil {
		t.Fatalf("start real Alpine container: %v", err)
	}
	containerID := strings.TrimSpace(string(output))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })
	for _, setup := range []string{
		"apk add --no-cache git >/dev/null",
		"mkdir -p /workspace/sub && cd /workspace && git init -q && git config user.email codefly@example.com && git config user.name Codefly",
		"printf 'hello world\\n' > /workspace/greet.txt && printf 'nested world\\n' > /workspace/sub/nested.txt",
		"cd /workspace && git add . && git commit -qm initial",
	} {
		if setupErr := exec.Command("docker", "exec", containerID, "sh", "-c", setup).Run(); setupErr != nil {
			t.Fatalf("container setup %q: %v", setup, setupErr)
		}
	}

	ctx := context.Background()
	gateway, err := New(ctx, Config{ContainerID: containerID, WorkDir: "/workspace", Service: "workspace"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	services, err := gateway.ListServices(ctx, &gatewayv1.ListServicesRequest{})
	if err != nil || len(services.GetServices()) != 1 || services.GetServices()[0].GetName() != "workspace" {
		t.Fatalf("ListServices = %+v, %v", services, err)
	}

	listed, err := gateway.ListFiles(ctx, &gatewayv1.ListFilesRequest{Service: "workspace", Recursive: true})
	if err != nil {
		t.Fatalf("ListFiles against BusyBox: %v", err)
	}
	for _, expected := range []string{"greet.txt", "sub", "sub/nested.txt"} {
		if !hasListedPath(listed, expected) {
			t.Fatalf("ListFiles omitted %q: %+v", expected, listed.GetFiles())
		}
	}
	if hasListedPath(listed, ".git") {
		t.Fatalf("ListFiles exposed pruned repository metadata: %+v", listed.GetFiles())
	}

	read, err := gateway.ReadFile(ctx, &gatewayv1.ReadFileRequest{Service: "workspace", Path: "greet.txt"})
	if err != nil || !read.GetExists() || read.GetContent() != "hello world\n" {
		t.Fatalf("ReadFile = %+v, %v", read, err)
	}
	if _, err := gateway.ReadFile(ctx, &gatewayv1.ReadFileRequest{Service: "workspace", Path: "../etc/passwd"}); err == nil {
		t.Fatal("ReadFile accepted a path outside the configured authority")
	}

	created, err := gateway.CreateFile(ctx, &gatewayv1.CreateFileRequest{
		Service: "workspace", Path: "new.txt", Content: "new world\n",
	})
	if err != nil || !created.GetSuccess() {
		t.Fatalf("CreateFile = %+v, %v", created, err)
	}
	duplicate, err := gateway.CreateFile(ctx, &gatewayv1.CreateFileRequest{
		Service: "workspace", Path: "new.txt", Content: "overwrite\n",
	})
	if err != nil || duplicate.GetSuccess() {
		t.Fatalf("duplicate CreateFile = %+v, %v", duplicate, err)
	}
	edit, err := gateway.ApplyEdit(ctx, &gatewayv1.ApplyEditRequest{
		Service: "workspace", File: "greet.txt", Find: "hello world", Replace: "hello codefly",
	})
	if err != nil || !edit.GetSuccess() {
		t.Fatalf("ApplyEdit = %+v, %v", edit, err)
	}

	search, err := gateway.Search(ctx, &gatewayv1.SearchRequest{
		Service: "workspace", Path: ".", Pattern: "codefly", Literal: true, MaxResults: 10,
	})
	if err != nil || len(search.GetMatches()) != 1 || search.GetMatches()[0].GetFile() != "greet.txt" {
		t.Fatalf("Search = %+v, %v", search, err)
	}

	statusResponse, err := gateway.GitStatus(ctx, &gatewayv1.GitStatusRequest{Service: "workspace"})
	if err != nil || !hasGitPath(statusResponse, "greet.txt") || !hasGitPath(statusResponse, "new.txt") {
		t.Fatalf("GitStatus = %+v, %v", statusResponse, err)
	}
	diff, err := gateway.GitDiff(ctx, &gatewayv1.GitDiffRequest{Service: "workspace"})
	if err != nil || !strings.Contains(diff.GetDiff(), "+hello codefly") || !strings.Contains(diff.GetDiff(), "new.txt") {
		t.Fatalf("GitDiff = %q, %v", diff.GetDiff(), err)
	}
	logResponse, err := gateway.GitLog(ctx, &gatewayv1.GitLogRequest{Service: "workspace", Count: 5})
	if err != nil || len(logResponse.GetCommits()) != 1 || !strings.Contains(logResponse.GetCommits()[0].GetMessage(), "initial") {
		t.Fatalf("GitLog = %+v, %v", logResponse, err)
	}

	run, err := gateway.RunCommand(ctx, &gatewayv1.RunCommandRequest{
		Service: "workspace", Command: "sh", Args: []string{"-c", "printf typed-gateway"},
		UnstructuredUse: &gatewayv1.UnstructuredUse{
			Intent: "exercise the container transport", WhyNoTool: "the fixture requires a diagnostic command",
			CodeUnitId: "docker-gateway-integration", ObjectiveId: "docker-gateway-integration",
			CommandClass: gatewayv1.CommandClass_COMMAND_CLASS_DIAGNOSTIC,
		},
	})
	if err != nil || run.GetExitCode() != 0 || run.GetStdout() != "typed-gateway" {
		t.Fatalf("RunCommand = %+v, %v", run, err)
	}
}

func hasListedPath(response *gatewayv1.ListFilesResponse, wanted string) bool {
	for _, file := range response.GetFiles() {
		if file.GetPath() == wanted {
			return true
		}
	}
	return false
}

func hasGitPath(response *gatewayv1.GitStatusResponse, wanted string) bool {
	for _, file := range response.GetFiles() {
		if file.GetPath() == wanted {
			return true
		}
	}
	return false
}
