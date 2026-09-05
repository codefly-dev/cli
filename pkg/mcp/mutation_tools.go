package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"sync"
	"time"

	"github.com/codefly-dev/cli/pkg/cli/communicate"
	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	runnersbase "github.com/codefly-dev/core/runners/base"
	"github.com/codefly-dev/core/services"
)

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
		Description: "Create a new service in a module by running the agent's Create flow (same as 'codefly add service'). Scaffolds the service directory and manifest non-interactively using the agent's declared defaults.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":      {Type: "string", Description: "Module to add the service to"},
				"name":        {Type: "string", Description: "Service name (kebab-case)"},
				"agent":       {Type: "string", Description: `Agent reference: "go-grpc", "codefly.dev/go-grpc", or "codefly.dev/go-grpc:0.0.16". Version defaults to latest (local cache first, then GitHub releases).`},
				"description": {Type: "string", Description: "Short service description written to service.codefly.yaml"},
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

	moduleName, serviceName, agentInput := args["module"], args["name"], args["agent"]
	if moduleName == "" || serviceName == "" || agentInput == "" {
		return []Content{TextContent("module, name, and agent are required")}, nil
	}
	// serviceName is joined into a filesystem path below — reject anything that
	// could traverse out of the services directory.
	if !isSafeAgentName(serviceName) {
		return []Content{TextContent(fmt.Sprintf("invalid service name %q", serviceName))}, nil
	}

	mod, err := ws.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}
	if mod.ExistsService(ctx, serviceName) {
		return []Content{TextContent(fmt.Sprintf("service %s already exists in module %s", serviceName, moduleName))}, nil
	}

	// Agent identity: accepts "go-grpc", "codefly.dev/go-grpc", or "codefly.dev/go-grpc:0.0.16".
	// ParseAgent applies the default publisher and "latest" when omitted.
	agent, err := resources.ParseAgent(ctx, resources.ServiceAgent, agentInput)
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("invalid agent %q: %v", agentInput, err))}, nil
	}
	// agent.Name/Publisher come from attacker-controllable MCP input; keep them
	// restricted to the safe identifier set even though ParseAgent validates shape.
	if !isSafeAgentName(agent.Name) || !isSafeAgentName(agent.Publisher) {
		return []Content{TextContent(fmt.Sprintf("invalid agent %q", agentInput))}, nil
	}
	if _, resolveErr := manager.ResolveLatest(ctx, agent); resolveErr != nil {
		return nil, fmt.Errorf("resolve agent %s: %w", agent.Identifier(), resolveErr)
	}
	downloaded, err := manager.Downloaded(ctx, agent)
	if err != nil {
		return nil, fmt.Errorf("check agent %s: %w", agent.Identifier(), err)
	}
	if !downloaded {
		if downloadErr := manager.Download(ctx, agent); downloadErr != nil {
			return nil, fmt.Errorf("download agent %s: %w", agent.Identifier(), downloadErr)
		}
	}
	agentProto, err := agent.Proto()
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", agent.Identifier(), err)
	}

	input := &actionsservice.AddService{
		Name:        serviceName,
		Agent:       agentProto,
		Description: args["description"],
	}
	output, err := services.Add(ctx, ws, mod, input, communicate.NewHeadlessPrompt())
	if err != nil {
		return nil, fmt.Errorf("add service %s/%s: %w", moduleName, serviceName, err)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service created but cannot be reloaded: %w", err)
	}

	result := map[string]any{
		"status":  "created",
		"module":  moduleName,
		"service": serviceName,
		"agent":   agent.Identifier(),
		"path":    svc.Dir(),
		"readme":  truncate(output.ReadMe, 4000),
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… (truncated)"
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
	var outBuf synchronizedBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	// Start through Core's authenticated process-group boundary. It publishes
	// the durable identity record before releasing the child, so a dead MCP
	// owner can be recovered without ever signaling a reused PID/group.
	group, err := runnersbase.StartTrackedProcessGroup(cmd)
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("run failed to start: %v", err))}, nil
	}
	// Reap the subprocess in a background goroutine so it doesn't become
	// a zombie if/when it exits. We don't wait — the caller just needs
	// to know it's launched.
	pid := cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		_ = group.RemoveIfDead()
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
