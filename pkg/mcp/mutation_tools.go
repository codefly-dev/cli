package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/control"
	"github.com/codefly-dev/core/resources"
)

// currentCLI resolves the codefly binary running this MCP server, so
// subprocess tools invoke the exact build in use rather than whatever
// `codefly` happens to resolve to on PATH.
func currentCLI() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve codefly binary: %w", err)
	}
	return filepath.EvalSymlinks(exe)
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
		Description: "Run a service with its dependency graph in-process (same orchestration as 'codefly run service --headless'). Returns once the flow is running unless wait=false.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":          {Type: "string", Description: "Module containing the service"},
				"service":         {Type: "string", Description: "Service to run"},
				"runtime_context": {Type: "string", Description: "Runtime context: native, nix, container, or free"},
				"profile":         {Type: "string", Description: "Named run profile from workspace.codefly.yaml"},
				"wait":            {Type: "string", Description: "Block until the flow is running (true/false, default true)"},
				"timeout_seconds": {Type: "string", Description: "Max seconds to wait for readiness (default 300)"},
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
				"module":          {Type: "string", Description: "Module containing the service"},
				"service":         {Type: "string", Description: "Service to test"},
				"suite":           {Type: "string", Description: "Test suite to run (optional)"},
				"filter":          {Type: "string", Description: "Test filter (optional)"},
				"runtime_context": {Type: "string", Description: "Runtime context: native, nix, container, or free"},
			},
			Required: []string{"module", "service"},
		},
	}, s.testService)

	s.RegisterTool(Tool{
		Name:        "flow_status",
		Description: "Report the state of a flow started by run_service (idle, starting, running, stopped, failed) and its services. Without flow_id, reports the most recently started run — pass the flow_id from run_service's response if more than one run may be active, otherwise you may see a different run's state.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"flow_id": {Type: "string", Description: "flow_id from a run_service response (optional; defaults to the most recently started run)"},
			},
		},
	}, s.flowStatus)

	s.RegisterTool(Tool{
		Name:        "stop_flow",
		Description: "Stop a flow started by run_service. Without flow_id, stops the most recently started run — pass the flow_id from run_service's response if more than one run may be active, otherwise you may stop the wrong one. Set destroy=true to also remove stateful containers (databases lose data).",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"flow_id": {Type: "string", Description: "flow_id from a run_service response (optional; defaults to the most recently started run)"},
				"destroy": {Type: "string", Description: "Also remove stateful containers, e.g. databases (true/false, default false)"},
			},
		},
	}, s.stopFlow)

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

	cli, err := currentCLI()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, cli, "generate", "proto",
		"--proto", protoDir, "--output", outputDir)
	cmd.Dir = serviceDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("proto generation failed: %s\n%s", err, string(output)))}, nil
	}

	return []Content{TextContent(fmt.Sprintf("Proto generated successfully for %s/%s\n%s", moduleName, serviceName, string(output)))}, nil
}

// runService starts service and its dependency graph through the control
// plane. The flow is registered under s.runCtx — a context scoped to the
// server's own lifetime and cancelled only by Close — rather than ctx: ctx is
// shared by every request this server handles (ServeIO dispatches them
// concurrently; see its comment) and, below, this handler derives a further
// waitCtx from it that it cancels on return. The flow must not be tied to
// either — it has to keep running after this call returns, stopping only
// when the server itself does.
func (s *Server) runService(ctx context.Context, args map[string]string) ([]Content, error) {
	if _, err := s.requireWorkspace(); err != nil {
		return nil, err
	}
	if args["module"] == "" || args["service"] == "" {
		return []Content{TextContent("module and service are required")}, nil
	}
	ref := serviceRef(args)
	wait := args["wait"] != "false"
	handle, err := s.plane.Run(s.runCtx, control.RunRequest{
		Service:        ref,
		RuntimeContext: args["runtime_context"],
		Profile:        args["profile"],
		Headless:       true,
	})
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("run %s failed: %v", ref, err))}, nil
	}

	if wait {
		timeout := 300 * time.Second
		if raw := args["timeout_seconds"]; raw != "" {
			if secs, convErr := strconv.Atoi(raw); convErr == nil && secs > 0 {
				timeout = time.Duration(secs) * time.Second
			}
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := s.waitFlowRunning(waitCtx, handle.FlowID); err != nil {
			return nil, err
		}
	}

	// Scoped to handle.FlowID, not "whatever is active": Run keys flows by
	// service, so a second run_service call for a different service can be
	// registered while this one is still starting/running. Without the ID,
	// this would report that other flow's state instead of this run's.
	status, _ := s.plane.FlowStatus(ctx, handle.FlowID)
	result := map[string]any{
		"flow_id": handle.FlowID,
		"state":   string(status.State),
		"note":    "Use flow_status/stop_flow with flow_id=\"" + handle.FlowID + "\" to target this run specifically if others are active.",
	}
	if status.Error != "" {
		result["error"] = status.Error
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// waitFlowRunning polls s.plane.FlowStatus(ctx, flowID) until that flow is
// running, it fails or stops, or ctx is done. The flow itself keeps running
// under s.runCtx regardless of how this wait ends — cancelling ctx only stops
// the poll.
func (s *Server) waitFlowRunning(ctx context.Context, flowID string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := s.plane.FlowStatus(ctx, flowID)
		if err != nil {
			return err
		}
		if status.State == control.FlowRunning || status.State == control.FlowFailed || status.State == control.FlowStopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Server) testService(ctx context.Context, args map[string]string) ([]Content, error) {
	if _, err := s.requireWorkspace(); err != nil {
		return nil, err
	}
	if args["module"] == "" || args["service"] == "" {
		return []Content{TextContent("module and service are required")}, nil
	}
	result, err := s.plane.Test(ctx, control.TestRequest{
		Service:        serviceRef(args),
		Suite:          args["suite"],
		Filter:         args["filter"],
		RuntimeContext: args["runtime_context"],
	})
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("test failed to run: %v", err))}, nil
	}
	if !result.Passed {
		return []Content{TextContent("FAILED\n" + result.Output)}, nil
	}
	return []Content{TextContent("PASSED\n" + result.Output)}, nil
}

// flowStatus reports the state of the flow started by run_service.
func (s *Server) flowStatus(ctx context.Context, args map[string]string) ([]Content, error) {
	// flow_id scopes the query to one run (its flow_id, from run_service's
	// response) instead of "whichever flow is currently active" — needed as
	// soon as more than one run_service call is in flight at once.
	status, err := s.plane.FlowStatus(ctx, args["flow_id"])
	if err != nil {
		return nil, fmt.Errorf("flow status: %w", err)
	}
	type serviceStatus struct {
		Name    string `json:"name"`
		State   string `json:"state"`
		Healthy bool   `json:"healthy"`
	}
	services := make([]serviceStatus, 0, len(status.Services))
	for _, svc := range status.Services {
		services = append(services, serviceStatus{Name: svc.Name, State: string(svc.State), Healthy: svc.Healthy})
	}
	result := map[string]any{
		"state":    string(status.State),
		"services": services,
	}
	if status.Error != "" {
		result["error"] = status.Error
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// stopFlow stops the flow started by run_service.
func (s *Server) stopFlow(ctx context.Context, args map[string]string) ([]Content, error) {
	// Stop reports whether it found and stopped anything, checked and cleared
	// atomically under the flow registry's own lock. A separate FlowStatus
	// check beforehand would race: the flow can exit on its own in the gap
	// between that check and this call, which would otherwise make stop_flow
	// falsely report "stopped" for a flow that had already ended.
	stopped, err := s.plane.Stop(ctx, control.StopRequest{FlowID: args["flow_id"], Destroy: args["destroy"] == "true"})
	if err != nil {
		return nil, fmt.Errorf("stop flow: %w", err)
	}
	if !stopped {
		return []Content{TextContent("nothing running")}, nil
	}
	return []Content{TextContent("stopped")}, nil
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

	cli, err := currentCLI()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, cli, cmdArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("install failed: %s\n%s", err, string(output)))}, nil
	}

	return []Content{TextContent(fmt.Sprintf("Agent %s installed\n%s", agentName, string(output)))}, nil
}
