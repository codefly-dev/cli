package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/codefly-dev/core/resources"
)

// registerMutationTools adds tools that modify the workspace (create services, add deps, etc.)
func (s *Server) registerMutationTools() {
	s.RegisterTool(Tool{
		Name:        "add_service",
		Description: "Create a new service in the workspace using a codefly agent template. Scaffolds all files non-interactively.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module to add the service to"},
				"name":    {Type: "string", Description: "Service name (kebab-case)"},
				"agent":   {Type: "string", Description: "Agent name (e.g. go-grpc, nextjs, external-postgres, external-vault)"},
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
		Description: "Install or update a codefly agent (e.g. go-grpc, nextjs, external-postgres)",
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

	// Determine agent version from installed agents
	agentVersion := "0.0.1"
	agentDir := fmt.Sprintf("%s/.codefly/agents/services/codefly.dev/%s__*", os.Getenv("HOME"), agentName)
	matches, _ := exec.Command("sh", "-c", fmt.Sprintf("ls -d %s 2>/dev/null | tail -1", agentDir)).Output()
	if len(matches) > 0 {
		parts := strings.Split(strings.TrimSpace(string(matches)), "__")
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

	cmdArgs := []string{"run", "service"}
	if debug {
		cmdArgs = append(cmdArgs, "-d")
	}

	cmd := exec.CommandContext(ctx, "codefly", cmdArgs...)
	cmd.Dir = path.Join(svc.Dir(), "code")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return []Content{TextContent(fmt.Sprintf("run failed: %s\n%s", err, string(output)))}, nil
	}

	return []Content{TextContent(fmt.Sprintf("Service %s/%s running\n%s", moduleName, serviceName, string(output)))}, nil
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

	cmdArgs := []string{"install", "agent", "codefly.dev/" + agentName}
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
