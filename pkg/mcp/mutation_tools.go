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
	"time"
	"unicode/utf8"

	"github.com/codefly-dev/cli/pkg/cli/communicate"
	"github.com/codefly-dev/cli/pkg/control"
	actionsservice "github.com/codefly-dev/core/actions/service"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// trustedAgentPublisher is the only agent publisher add_service will resolve,
// download, and execute non-interactively. install_agent (below) hardcodes
// the same publisher for the same reason: unlike the interactive CLI, there
// is no human confirming the exact agent reference before it runs.
const trustedAgentPublisher = "codefly.dev"

// addServiceTimeout bounds agent resolution/download and the agent's own
// Create-flow scaffolding. The MCP server dispatches requests concurrently
// (see ServeIO), but a single add_service call still should not be allowed to
// hang indefinitely on a stalled network call or a slow agent.
const addServiceTimeout = 10 * time.Minute

// add_service field names and its schema's "string" property type. Each of
// these strings recurs often enough elsewhere in this package that
// golangci-lint's goconst check flags a bare literal wherever one of these
// lines changes.
const (
	fieldModule      = "module"
	fieldName        = "name"
	fieldAgent       = "agent"
	fieldDescription = "description"
	fieldPath        = "path"
	schemaTypeString = "string"
)

// flowIDArg is the shared "flow_id" tool argument name: the value run_service
// returns and flow_status/stop_flow accept to target that specific run.
const flowIDArg = "flow_id"

// moduleContainingServiceDesc and fieldDependency recur across tool schemas
// in this file; golangci-lint's goconst check flags a bare literal wherever
// one of these lines changes.
const (
	moduleContainingServiceDesc = "Module containing the service"
	fieldDependency             = "dependency"
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
	_ = s.RegisterTool(Tool{
		Name:        "add_service",
		Description: "Create a new service in a module by running the agent's Create flow (same as 'codefly add service'). Scaffolds the service directory and manifest non-interactively using the agent's declared defaults.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				fieldModule:      {Type: schemaTypeString, Description: "Module to add the service to"},
				fieldName:        {Type: schemaTypeString, Description: "Service name (kebab-case)"},
				fieldAgent:       {Type: schemaTypeString, Description: `Agent reference: "go-grpc", "codefly.dev/go-grpc", or "codefly.dev/go-grpc:0.0.16". Version defaults to latest (local cache first, then GitHub releases). Publisher must be "codefly.dev" (or omitted) — other publishers are rejected.`},
				fieldDescription: {Type: schemaTypeString, Description: "Short service description written to service.codefly.yaml"},
			},
			Required: []string{fieldModule, fieldName, fieldAgent},
		},
	}, s.addService)

	_ = s.RegisterTool(Tool{
		Name:        "add_dependency",
		Description: "Add a service dependency to a service's service.codefly.yaml",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":        {Type: "string", Description: moduleContainingServiceDesc},
				serviceSegment:  {Type: "string", Description: "Service to add the dependency to"},
				fieldDependency: {Type: "string", Description: "Name of the service to depend on"},
			},
			Required: []string{"module", "service", "dependency"},
		},
	}, s.addDependency)

	_ = s.RegisterTool(Tool{
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

	_ = s.RegisterTool(Tool{
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

	_ = s.RegisterTool(Tool{
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

	_ = s.RegisterTool(Tool{
		Name:        "flow_status",
		Description: "Report the state of a flow started by run_service (idle, starting, running, stopped, failed) and its services. Without flow_id, reports the most recently started run — pass the flow_id from run_service's response if more than one run may be active, otherwise you may see a different run's state.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				flowIDArg: {Type: "string", Description: "flow_id from a run_service response (optional; defaults to the most recently started run)"},
			},
		},
	}, s.flowStatus)

	_ = s.RegisterTool(Tool{
		Name:        "stop_flow",
		Description: "Stop a flow started by run_service. Without flow_id, stops the most recently started run — pass the flow_id from run_service's response if more than one run may be active, otherwise you may stop the wrong one. Set destroy=true to also remove stateful containers (databases lose data).",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				flowIDArg: {Type: "string", Description: "flow_id from a run_service response (optional; defaults to the most recently started run)"},
				"destroy": {Type: "string", Description: "Also remove stateful containers, e.g. databases (true/false, default false)"},
			},
		},
	}, s.stopFlow)

	_ = s.RegisterTool(Tool{
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
// glob/path. "." and ".." are rejected outright even though '.' is otherwise
// allowed: a value of exactly one of those is a path-traversal token when
// later joined into a filesystem path (e.g. agent.Publisher in
// manager.FindLocalLatest's filepath.Join), not a real agent/service name.
func isSafeAgentName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 100 {
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
	// Unlike the interactive CLI (which requires a human to type the exact
	// --agent value and confirm), this tool runs headlessly with no
	// confirmation step. ParseAgent/manager.Download impose no publisher
	// allow-list, so an unrestricted publisher would let any MCP caller point
	// this at an arbitrary GitHub org and have its release binary downloaded
	// and executed with full host authority. Pin to the same trusted
	// publisher install_agent already hardcodes below.
	if agent.Publisher != trustedAgentPublisher {
		return []Content{TextContent(fmt.Sprintf("agent publisher %q is not allowed for non-interactive service creation; only %q is permitted", agent.Publisher, trustedAgentPublisher))}, nil
	}

	// Resolution, download, and the agent's own Create flow can involve
	// network I/O and long-running scaffolding (package installs, docker
	// pulls); addServiceTimeout bounds a single call to a reasonable ceiling.
	addCtx, cancel := context.WithTimeout(ctx, addServiceTimeout)
	defer cancel()

	if _, resolveErr := manager.ResolveLatest(addCtx, agent); resolveErr != nil {
		return nil, fmt.Errorf("resolve agent %s: %w", agent.Identifier(), resolveErr)
	}
	downloaded, err := manager.Downloaded(addCtx, agent)
	if err != nil {
		return nil, fmt.Errorf("check agent %s: %w", agent.Identifier(), err)
	}
	if !downloaded {
		if downloadErr := manager.Download(addCtx, agent); downloadErr != nil {
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
	output, err := services.Add(addCtx, ws, mod, input, communicate.NewHeadlessPrompt())
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
		fieldPath: svc.Dir(),
		"readme":  truncate(output.ReadMe, 4000),
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// truncate cuts s to at most n bytes, walking back to the nearest UTF-8 rune
// boundary first so a multi-byte character straddling byte n is never split
// (a raw s[:n] would corrupt the final character into replacement bytes).
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n… (truncated)"
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
		flowIDArg: handle.FlowID,
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
	status, err := s.plane.FlowStatus(ctx, args[flowIDArg])
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
	stopped, err := s.plane.Stop(ctx, control.StopRequest{FlowID: args[flowIDArg], Destroy: args["destroy"] == "true"})
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
