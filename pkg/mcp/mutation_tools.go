package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
)

// detachSysProcAttr returns the syscall attributes needed to detach a
// spawned subprocess from the parent's process group so signals delivered
// to the parent (e.g. MCP server shutdown) don't kill the child. Setpgid
// is supported on unix; on other platforms this is a no-op returning nil.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// synchronizedBuffer is an io.Writer whose snapshots are safe while a child
// process is still writing. bytes.Buffer itself cannot be read concurrently
// with exec.Cmd's stdout/stderr copy goroutines.
type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// registerMutationTools adds tools that modify the workspace (create services, add deps, etc.)
func (s *Server) registerMutationTools() {
	s.RegisterTool(Tool{
		Name:        "add_service",
		Description: "Create a new service in the workspace using a codefly agent template. Scaffolds all files non-interactively.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module": {Type: "string", Description: "Module to add the service to"},
				"name":   {Type: "string", Description: "Service name (kebab-case)"},
				"agent":  {Type: "string", Description: "Agent name (e.g. go-grpc, nextjs, postgres, vault)"},
			},
			Required: []string{"module", "name", "agent"},
		},
	}, s.addService)

	s.RegisterTool(Tool{
		Name:        "add_dependency",
		Description: "Add a service dependency to a service's service.codefly.yaml",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":     {Type: "string", Description: "Module containing the service"},
				"service":    {Type: "string", Description: "Service to add the dependency to"},
				"dependency": {Type: "string", Description: "Name of the service to depend on"},
			},
			Required: []string{"module", "service", "dependency"},
		},
	}, s.addDependency)

	s.RegisterTool(Tool{
		Name:        "generate_proto",
		Description: "Regenerate code from proto files using the codefly proto companion. Generates Go gRPC, REST gateway, OpenAPI, and TypeScript types.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module containing the service"},
				"service": {Type: "string", Description: "Service with proto files"},
			},
			Required: []string{"module", "service"},
		},
	}, s.generateProto)

	s.RegisterTool(Tool{
		Name:        "run_service",
		Description: "Run a service with all its dependencies (equivalent to 'codefly run service'). Returns when the service is ready.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module containing the service"},
				"service": {Type: "string", Description: "Service to run"},
				"debug":   {Type: "string", Description: "Enable debug mode (true/false, default false)"},
			},
			Required: []string{"module", "service"},
		},
	}, s.runService)

	s.RegisterTool(Tool{
		Name:        "test_service",
		Description: "Run tests for a service with all dependencies started (equivalent to 'codefly test service').",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module containing the service"},
				"service": {Type: "string", Description: "Service to test"},
			},
			Required: []string{"module", "service"},
		},
	}, s.testService)

	s.RegisterTool(Tool{
		Name:        "install_agent",
		Description: "Install or update a codefly agent (e.g. go-grpc, nextjs, postgres)",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"name":    {Type: "string", Description: "Agent name (e.g. go-grpc)"},
				"version": {Type: "string", Description: "Version to install (optional, defaults to latest)"},
			},
			Required: []string{"name"},
		},
	}, s.installAgent)
}

// isSafeAgentName allows only the conservative identifier set used by real
// agent names (letters, digits, hyphen, underscore, dot). Anything else is
// rejected so attacker-controlled input can never reach a shell or escape a
// glob/path.
func isSafeAgentName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return false
		}
	}
	return true
}

func (s *Server) addService(ctx context.Context, args map[string]string) ([]Content, error) {
	ws, err := s.requireWorkspace()
	if err != nil {
		return nil, err
	}

	moduleName := args["module"]
	serviceName := args["name"]
	agentName := args["agent"]

	if moduleName == "" || serviceName == "" || agentName == "" {
		return []Content{TextContent("module, name, and agent are required")}, nil
	}
	// serviceName is joined into a filesystem path below — reject anything that
	// could traverse out of the services directory.
	if !isSafeAgentName(serviceName) {
		return []Content{TextContent(fmt.Sprintf("invalid service name %q", serviceName))}, nil
	}

	// Find the module
	mod, err := ws.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	// Check service doesn't already exist
	existing, _ := mod.LoadServiceFromName(ctx, serviceName)
	if existing != nil {
		return []Content{TextContent(fmt.Sprintf("service %s already exists in module %s", serviceName, moduleName))}, nil
	}

	// Create service directory and service.codefly.yaml
	serviceDir := path.Join(mod.Dir(), "services", serviceName)
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create service directory: %w", err)
	}

	// Reject agent names with anything but the safe identifier set. agentName
	// is attacker-controllable (MCP tool argument); it used to be interpolated
	// into `sh -c "... %s ..."` — a command-injection hole. We also no longer
	// shell out: filepath.Glob does the lookup natively.
	if !isSafeAgentName(agentName) {
		return []Content{TextContent(fmt.Sprintf("invalid agent name %q", agentName))}, nil
	}

	// Determine agent version from installed agents (last match wins).
	agentVersion := "0.0.1"
	pattern := filepath.Join(os.Getenv("HOME"), ".codefly", "agents", "services", "codefly.dev", agentName+"__*")
	if dirs, globErr := filepath.Glob(pattern); globErr == nil && len(dirs) > 0 {
		last := dirs[len(dirs)-1]
		parts := strings.Split(filepath.Base(last), "__")
		if len(parts) == 2 {
			agentVersion = parts[1]
		}
	}

	// Write service.codefly.yaml
	svcYAML := fmt.Sprintf(`name: %s
version: 0.0.0
agent:
    kind: codefly:service
    name: %s
    version: %s
    publisher: codefly.dev
`, serviceName, agentName, agentVersion)

	yamlPath := path.Join(serviceDir, "service.codefly.yaml")
	if err := os.WriteFile(yamlPath, []byte(svcYAML), 0644); err != nil {
		return nil, fmt.Errorf("cannot write service.codefly.yaml: %w", err)
	}

	result := map[string]any{
		"status":  "created",
		"service": serviceName,
		"module":  moduleName,
		"agent":   agentName,
		"path":    serviceDir,
		"note":    "Service directory and service.codefly.yaml created. Run 'codefly run service' to scaffold the full template via the agent's Create flow.",
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) addDependency(ctx context.Context, args map[string]string) ([]Content, error) {
	ws, err := s.requireWorkspace()
	if err != nil {
		return nil, err
	}

	moduleName := args["module"]
	serviceName := args["service"]
	depName := args["dependency"]

	mod, err := ws.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	// Check dependency exists
	depSvc, _ := mod.LoadServiceFromName(ctx, depName)
	if depSvc == nil {
		return []Content{TextContent(fmt.Sprintf("dependency service %s not found in module %s", depName, moduleName))}, nil
	}

	// Check not already a dependency
	for _, existing := range svc.ServiceDependencies {
		if existing.Name == depName {
			return []Content{TextContent(fmt.Sprintf("service %s already depends on %s", serviceName, depName))}, nil
		}
	}

	// Add dependency
	svc.ServiceDependencies = append(svc.ServiceDependencies, &resources.ServiceDependency{
		Name: depName,
	})

	if err := svc.Save(ctx); err != nil {
		return nil, fmt.Errorf("failed to save service: %w", err)
	}

	result := map[string]any{
		"status":     "added",
		"service":    serviceName,
		"dependency": depName,
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) generateProto(ctx context.Context, args map[string]string) ([]Content, error) {
	ws, err := s.requireWorkspace()
	if err != nil {
		return nil, err
	}

	moduleName := args["module"]
	serviceName := args["service"]

	mod, err := ws.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	serviceDir := svc.Dir()
	protoDir := path.Join(serviceDir, "proto")
	outputDir := path.Join(serviceDir, "code/pkg/gen")

	// Run codefly generate proto
	cmd := exec.CommandContext(ctx, "codefly", "generate", "proto",
		"--proto", protoDir, "--output", outputDir)
	cmd.Dir = serviceDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("proto generation failed: %s\n%s", err, string(output)))}, nil
	}

	return []Content{TextContent(fmt.Sprintf("Proto generated successfully for %s/%s\n%s", moduleName, serviceName, string(output)))}, nil
}

func (s *Server) runService(ctx context.Context, args map[string]string) ([]Content, error) {
	ws, err := s.requireWorkspace()
	if err != nil {
		return nil, err
	}

	moduleName := args["module"]
	serviceName := args["service"]
	debug := args["debug"] == "true"

	mod, err := ws.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	cmdArgs := []string{"run", "service", "--headless"}
	if debug {
		cmdArgs = append(cmdArgs, "-d")
	}

	// `codefly run service` is a long-running process — it does NOT return
	// once the service is up. Using exec.CommandContext + CombinedOutput
	// would block the MCP handler until the service is stopped, which
	// defeats the point (the AI tool would hang for the entire session).
	//
	// Instead: spawn detached, wait briefly for the subprocess to show
	// it's running, then return a handle. The caller can use status/logs
	// tools to check on it afterward. Process lifetime is bounded by the
	// user via the complementary stop tool.
	cmd := exec.Command("codefly", cmdArgs...)
	cmd.Dir = path.Join(svc.Dir(), "code")
	// Detach from the MCP server's lifetime — the service should outlive
	// the tool call. Set a new process group so signals to MCP don't
	// cascade into the spawned service.
	cmd.SysProcAttr = detachSysProcAttr()
	var outBuf synchronizedBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	if err := cmd.Start(); err != nil {
		return []Content{TextContent(fmt.Sprintf("run failed to start: %v", err))}, nil
	}
	// Reap the subprocess in a background goroutine so it doesn't become
	// a zombie if/when it exits. We don't wait — the caller just needs
	// to know it's launched.
	pid := cmd.Process.Pid
	// Track the detached CLI's pgroup. If the MCP-hosting CLI dies
	// ungracefully, the child codefly run subprocess would orphan
	// otherwise — now the next CLI startup's sweep sees the file,
	// detects the parent (this CLI) is dead, and reaps the subtree.
	argv := append([]string{"codefly"}, cmdArgs...)
	if perr := runnersbase.WritePgidFile(pid, cmd.Dir, argv); perr != nil {
		_ = perr // best-effort; MCP handler returns regardless
	}
	go func() {
		_ = cmd.Wait()
		_ = runnersbase.RemovePgidFile(pid)
	}()

	// Sample the initial output briefly so the caller gets a meaningful
	// confirmation (port binding, error, etc.) rather than an empty string.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(500 * time.Millisecond):
	}

	return []Content{TextContent(fmt.Sprintf(
		"Service %s/%s launched (pid=%d). Initial output:\n%s",
		moduleName, serviceName, pid, outBuf.String()))}, nil
}

func (s *Server) testService(ctx context.Context, args map[string]string) ([]Content, error) {
	ws, err := s.requireWorkspace()
	if err != nil {
		return nil, err
	}

	moduleName := args["module"]
	serviceName := args["service"]

	mod, err := ws.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	cmd := exec.CommandContext(ctx, "codefly", "test", "service")
	cmd.Dir = path.Join(svc.Dir(), "code")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("test failed: %s\n%s", err, string(output)))}, nil
	}

	return []Content{TextContent(string(output))}, nil
}

func (s *Server) installAgent(ctx context.Context, args map[string]string) ([]Content, error) {
	agentName := args["name"]
	version := args["version"]
	if !isSafeAgentName(agentName) {
		return []Content{TextContent(fmt.Sprintf("invalid agent name %q", agentName))}, nil
	}

	cmdArgs := []string{"agent", "install", "codefly.dev/" + agentName}
	if version != "" {
		cmdArgs = append(cmdArgs, "--version", version)
	}

	cmd := exec.CommandContext(ctx, "codefly", cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("install failed: %s\n%s", err, string(output)))}, nil
	}

	return []Content{TextContent(fmt.Sprintf("Agent %s installed\n%s", agentName, string(output)))}, nil
}
