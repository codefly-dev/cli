package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// registerTools sets up all available MCP tools
func (s *Server) registerTools() {
	// Workspace information tools
	s.RegisterTool(Tool{
		Name:        "workspace_info",
		Description: "Get information about the current codefly workspace including modules and services",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
		},
	}, s.workspaceInfo)

	s.RegisterTool(Tool{
		Name:        "list_modules",
		Description: "List all modules in the workspace",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
		},
	}, s.listModules)

	s.RegisterTool(Tool{
		Name:        "list_services",
		Description: "List services in a module or all services in the workspace",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module": {
					Type:        "string",
					Description: "Module name (optional, lists all if not provided)",
				},
			},
		},
	}, s.listServices)

	s.RegisterTool(Tool{
		Name:        "service_info",
		Description: "Get detailed information about a specific service",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module": {
					Type:        "string",
					Description: "Module containing the service",
				},
				"service": {
					Type:        "string",
					Description: "Service name",
				},
			},
			Required: []string{"module", "service"},
		},
	}, s.serviceInfo)

	s.RegisterTool(Tool{
		Name:        "service_dependencies",
		Description: "Get dependencies of a service",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module": {
					Type:        "string",
					Description: "Module containing the service",
				},
				"service": {
					Type:        "string",
					Description: "Service name",
				},
			},
			Required: []string{"module", "service"},
		},
	}, s.serviceDependencies)

	s.RegisterTool(Tool{
		Name:        "list_agents",
		Description: "List available service agents (templates for creating services)",
		InputSchema: InputSchema{
			Type:       "object",
			Properties: map[string]PropertySchema{},
		},
	}, s.listAgents)

	s.RegisterTool(Tool{
		Name:        "list_jobs",
		Description: "List jobs in a module or all jobs in the workspace",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module": {
					Type:        "string",
					Description: "Module name (optional, lists all if not provided)",
				},
			},
		},
	}, s.listJobs)

	// Per-service tools for Mind (design 013)
	s.RegisterTool(Tool{
		Name:        "describe",
		Description: "Get service metadata: name, type, language, file list (for Mind agent)",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
			},
			Required: []string{"module", "service"},
		},
	}, s.describe)

	s.RegisterTool(Tool{
		Name:        "read_file",
		Description: "Read a file from the service directory (path relative to service)",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"path":    {Type: "string", Description: "Relative path within the service"},
			},
			Required: []string{"module", "service", "path"},
		},
	}, s.readFile)

	s.RegisterTool(Tool{
		Name:        "write_file",
		Description: "Write content to a file in the service directory",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"path":    {Type: "string", Description: "Relative path within the service"},
				"content": {Type: "string", Description: "File content"},
			},
			Required: []string{"module", "service", "path", "content"},
		},
	}, s.writeFile)

	s.RegisterTool(Tool{
		Name:        "build",
		Description: "Build the service via the plugin (builder)",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
			},
			Required: []string{"module", "service"},
		},
	}, s.build)

	s.RegisterTool(Tool{
		Name:        "run_checks",
		Description: "Run a command in the service directory (e.g. go test ./...). Optional 'command' arg; default: go test ./...",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"command": {Type: "string", Description: "Command to run (default: go test ./...)"},
			},
			Required: []string{"module", "service"},
		},
	}, s.runChecks)

	s.RegisterTool(Tool{
		Name:        "stop",
		Description: "Stop the service runtime if running",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
			},
			Required: []string{"module", "service"},
		},
	}, s.stop)

	// Agent command tools — expose agent-registered commands to MCP
	s.RegisterTool(Tool{
		Name:        "list_service_commands",
		Description: "List commands available on a service agent (e.g. test, lint, screenshot, health)",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
			},
			Required: []string{"module", "service"},
		},
	}, s.listServiceCommands)

	s.RegisterTool(Tool{
		Name:        "run_service_command",
		Description: "Run a command on a service agent (e.g. test, lint, screenshot, health, proto)",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"command": {Type: "string", Description: "Command name"},
				"args":    {Type: "string", Description: "Command arguments (space-separated)"},
			},
			Required: []string{"module", "service", "command"},
		},
	}, s.runServiceCommand)
}

// workspaceInfo returns information about the current workspace
func (s *Server) workspaceInfo(ctx context.Context, args map[string]string) ([]Content, error) {
	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded. Run this command from a codefly workspace directory.")}, nil
	}

	info := map[string]any{
		"name":        s.workspace.Name,
		"description": s.workspace.Description,
		"modules":     []string{},
		"services":    []map[string]string{},
	}

	// Get modules
	modules, err := s.workspace.LoadModules(ctx)
	if err == nil {
		moduleNames := make([]string, 0, len(modules))
		for _, m := range modules {
			moduleNames = append(moduleNames, m.Name)
		}
		info["modules"] = moduleNames

		// Get services
		services := make([]map[string]string, 0)
		for _, m := range modules {
			svcs, err := m.LoadServices(ctx)
			if err != nil {
				continue
			}
			for _, svc := range svcs {
				services = append(services, map[string]string{
					"module":  m.Name,
					"service": svc.Name,
					"agent":   svc.Agent.Name,
				})
			}
		}
		info["services"] = services
	}

	data, _ := json.MarshalIndent(info, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listModules returns all modules in the workspace
func (s *Server) listModules(ctx context.Context, args map[string]string) ([]Content, error) {
	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}

	modules, err := s.workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0, len(modules))
	for _, m := range modules {
		result = append(result, map[string]any{
			"name":        m.Name,
			"description": m.Description,
		})
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listServices returns services, optionally filtered by module
func (s *Server) listServices(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.listServices")

	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}

	modules, err := s.workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}

	moduleFilter := args["module"]
	result := make([]map[string]any, 0)

	for _, m := range modules {
		if moduleFilter != "" && m.Name != moduleFilter {
			continue
		}

		svcs, err := m.LoadServices(ctx)
		if err != nil {
			w.Warn("failed to load services", wool.Field("module", m.Name), wool.ErrField(err))
			continue
		}

		for _, svc := range svcs {
			svcInfo := map[string]any{
				"name":        svc.Name,
				"module":      m.Name,
				"description": svc.Description,
				"agent":       svc.Agent.Name,
				"version":     svc.Version,
			}

			// Add endpoints
			endpoints := make([]map[string]string, 0)
			for _, ep := range svc.Endpoints {
				endpoints = append(endpoints, map[string]string{
					"name":       ep.Name,
					"api":        ep.API,
					"visibility": ep.Visibility,
				})
			}
			svcInfo["endpoints"] = endpoints

			result = append(result, svcInfo)
		}
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// serviceInfo returns detailed information about a specific service
func (s *Server) serviceInfo(ctx context.Context, args map[string]string) ([]Content, error) {
	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}

	moduleName := args["module"]
	serviceName := args["service"]

	if moduleName == "" || serviceName == "" {
		return []Content{TextContent("Both 'module' and 'service' arguments are required.")}, nil
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	info := map[string]any{
		"name":        svc.Name,
		"module":      moduleName,
		"description": svc.Description,
		"version":     svc.Version,
		"agent": map[string]string{
			"name":      svc.Agent.Name,
			"kind":      string(svc.Agent.Kind),
			"publisher": svc.Agent.Publisher,
			"version":   svc.Agent.Version,
		},
	}

	// Endpoints
	endpoints := make([]map[string]any, 0)
	for _, ep := range svc.Endpoints {
		endpoints = append(endpoints, map[string]any{
			"name":       ep.Name,
			"api":        ep.API,
			"visibility": ep.Visibility,
		})
	}
	info["endpoints"] = endpoints

	// Dependencies
	deps := make([]map[string]any, 0)
	for _, dep := range svc.ServiceDependencies {
		depInfo := map[string]any{
			"name":   dep.Name,
			"module": dep.Module,
		}
		if len(dep.Endpoints) > 0 {
			depInfo["endpoints"] = dep.Endpoints
		}
		deps = append(deps, depInfo)
	}
	info["dependencies"] = deps

	data, _ := json.MarshalIndent(info, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// serviceDependencies returns the dependencies of a service
func (s *Server) serviceDependencies(ctx context.Context, args map[string]string) ([]Content, error) {
	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}

	moduleName := args["module"]
	serviceName := args["service"]

	if moduleName == "" || serviceName == "" {
		return []Content{TextContent("Both 'module' and 'service' arguments are required.")}, nil
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	deps := make([]map[string]any, 0)
	for _, dep := range svc.ServiceDependencies {
		depInfo := map[string]any{
			"name":      dep.Name,
			"module":    dep.Module,
			"endpoints": dep.Endpoints,
		}
		deps = append(deps, depInfo)
	}

	data, _ := json.MarshalIndent(deps, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listAgents returns available service agents
func (s *Server) listAgents(ctx context.Context, args map[string]string) ([]Content, error) {
	// These are the known agents from the agents directory
	agents := []map[string]any{
		{"name": "go-grpc", "description": "Go gRPC service", "languages": []string{"Go"}, "protocols": []string{"gRPC", "REST"}},
		{"name": "python-grpc", "description": "Python gRPC service", "languages": []string{"Python"}, "protocols": []string{"gRPC"}},
		{"name": "python-fastapi", "description": "Python FastAPI service", "languages": []string{"Python"}, "protocols": []string{"REST"}},
		{"name": "nextjs", "description": "Next.js frontend application", "languages": []string{"TypeScript", "JavaScript"}, "protocols": []string{"HTTP"}},
		{"name": "rails", "description": "Ruby on Rails application", "languages": []string{"Ruby"}, "protocols": []string{"REST"}},
		{"name": "krakend", "description": "KrakenD API Gateway", "languages": []string{}, "protocols": []string{"REST", "gRPC"}},
		{"name": "postgres", "description": "PostgreSQL database", "languages": []string{}, "protocols": []string{"TCP"}},
		{"name": "mysql", "description": "MySQL database", "languages": []string{}, "protocols": []string{"TCP"}},
		{"name": "redis", "description": "Redis cache/database", "languages": []string{}, "protocols": []string{"TCP"}},
		{"name": "minio", "description": "MinIO object storage", "languages": []string{}, "protocols": []string{"HTTP"}},
	}

	data, _ := json.MarshalIndent(agents, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listJobs returns jobs, optionally filtered by module
func (s *Server) listJobs(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.listJobs")

	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}

	modules, err := s.workspace.LoadModules(ctx)
	if err != nil {
		return nil, err
	}

	moduleFilter := args["module"]
	result := make([]map[string]any, 0)

	for _, m := range modules {
		if moduleFilter != "" && m.Name != moduleFilter {
			continue
		}

		jobs, err := m.LoadJobs(ctx)
		if err != nil {
			w.Warn("failed to load jobs", wool.Field("module", m.Name), wool.ErrField(err))
			continue
		}

		for _, job := range jobs {
			jobInfo := map[string]any{
				"name":        job.Name,
				"module":      m.Name,
				"description": job.Description,
				"version":     job.Version,
			}

			// Add execution info
			if job.Execution != nil {
				jobInfo["execution"] = map[string]any{
					"type":     string(job.Execution.Type),
					"schedule": job.Execution.Schedule,
					"timeout":  job.Execution.Timeout,
					"retries":  job.Execution.Retries,
				}
			}

			// Add agent info
			if job.Agent != nil {
				jobInfo["agent"] = job.Agent.Name
			}

			// Add service dependencies
			if len(job.ServiceDependencies) > 0 {
				deps := make([]map[string]string, 0)
				for _, dep := range job.ServiceDependencies {
					deps = append(deps, map[string]string{
						"name":   dep.Name,
						"module": dep.Module,
					})
				}
				jobInfo["service_dependencies"] = deps
			}

			result = append(result, jobInfo)
		}
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// Helper to get workspace for tools
func (s *Server) requireWorkspace() (*resources.Workspace, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("no workspace loaded - run from a codefly workspace directory")
	}
	return s.workspace, nil
}
