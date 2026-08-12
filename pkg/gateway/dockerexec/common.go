// Package dockerexec implements Codefly's typed Gateway contract against an
// already-running Docker container.
//
// ARCHITECTURE: this package is an execution-layer transport. Products such as
// Mind select it as a Gateway and submit typed file, Git, and command requests;
// they never construct Docker or project-tool commands themselves. The Docker
// CLI and the target container remain entirely behind the Codefly boundary.
package dockerexec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type base struct {
	containerID  string
	workDir      string
	authorityDir string
	service      string
}

func newBase(config Config) (base, error) {
	containerID := strings.TrimSpace(config.ContainerID)
	if containerID == "" {
		return base{}, fmt.Errorf("docker execution gateway: container id is required")
	}
	workDir := path.Clean(strings.TrimSpace(config.WorkDir))
	if workDir == "." || workDir == "" {
		workDir = "/"
	}
	if !path.IsAbs(workDir) {
		return base{}, fmt.Errorf("docker execution gateway: work directory must be absolute: %q", config.WorkDir)
	}
	authorityDir := path.Clean(strings.TrimSpace(config.AuthorityDir))
	if authorityDir == "." || authorityDir == "" {
		authorityDir = workDir
	}
	if !path.IsAbs(authorityDir) {
		return base{}, fmt.Errorf("docker execution gateway: authority directory must be absolute: %q", config.AuthorityDir)
	}
	if !within(authorityDir, workDir) {
		return base{}, fmt.Errorf("docker execution gateway: work directory %q is outside authority directory %q", workDir, authorityDir)
	}
	return base{
		containerID:  containerID,
		workDir:      workDir,
		authorityDir: authorityDir,
		service:      strings.TrimSpace(config.Service),
	}, nil
}

func (b base) validateService(service string) error {
	service = strings.TrimSpace(service)
	if b.service == "" || service == "" || service == b.service {
		return nil
	}
	return status.Errorf(codes.NotFound, "service %q not found in container Gateway", service)
}

// resolve maps an RPC path to a container-absolute path and proves that it is
// within the configured execution authority. Relative paths are rooted at the
// configured workspace directory; absolute paths are useful for isolated
// terminal environments whose authority root is explicitly set to "/".
func (b base) resolve(requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	resolved := b.workDir
	if requested != "" && requested != "." {
		if path.IsAbs(requested) {
			resolved = path.Clean(requested)
		} else {
			resolved = path.Clean(path.Join(b.workDir, requested))
		}
	}
	if !within(b.authorityDir, resolved) {
		return "", fmt.Errorf("path %q escapes container Gateway authority %q", requested, b.authorityDir)
	}
	return resolved, nil
}

func within(root, candidate string) bool {
	root = path.Clean(root)
	candidate = path.Clean(candidate)
	if root == "/" || candidate == root {
		return true
	}
	return strings.HasPrefix(candidate, strings.TrimSuffix(root, "/")+"/")
}

func (b base) relative(containerPath string) string {
	clean := path.Clean(containerPath)
	if clean == b.workDir {
		return "."
	}
	prefix := strings.TrimSuffix(b.workDir, "/") + "/"
	if strings.HasPrefix(clean, prefix) {
		return strings.TrimPrefix(clean, prefix)
	}
	return clean
}

func (b base) run(ctx context.Context, cwd string, timeoutSeconds int, argv ...string) (string, string, int, error) {
	return b.runStdin(ctx, cwd, timeoutSeconds, nil, argv...)
}

// runStdin executes exact argv in the target container. The optional stdin is
// a finite payload: Docker closes the stream at EOF before output is returned.
func (b base) runStdin(ctx context.Context, cwd string, timeoutSeconds int, stdin []byte, argv ...string) (string, string, int, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return "", "", -1, fmt.Errorf("docker execution gateway: command is required")
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	if cwd == "" {
		cwd = b.workDir
	}
	resolvedCWD, err := b.resolve(cwd)
	if err != nil {
		return "", "", -1, err
	}

	args := []string{"exec"}
	if len(stdin) > 0 {
		args = append(args, "-i")
	}
	args = append(args, "-w", resolvedCWD, b.containerID)
	args = append(args, argv...)

	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, "docker", args...)
	if len(stdin) > 0 {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if commandCtx.Err() != nil {
		return stdout.String(), stderr.String(), -1, fmt.Errorf("docker execution gateway: %w", commandCtx.Err())
	}
	exitCode := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
		err = nil
	}
	if err != nil {
		return stdout.String(), stderr.String(), -1, fmt.Errorf("docker execution gateway: %w", err)
	}
	return stdout.String(), stderr.String(), exitCode, nil
}
