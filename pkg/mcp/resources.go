package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codefly-dev/wool"
	"gopkg.in/yaml.v3"
)

// registerResources sets up all available MCP resources
func (s *Server) registerResources() {
	s.RegisterResource(Resource{
		URI:         "codefly://workspace",
		Name:        "Workspace Configuration",
		Description: "The workspace.codefly.yaml configuration file",
		MimeType:    "application/x-yaml",
	}, s.workspaceResource)

	// Dynamic resources for modules and services are handled in readResource
	// by pattern matching the URI
}

// workspaceResource returns the workspace configuration
func (s *Server) workspaceResource(ctx context.Context) ([]ResourceContents, error) {
	w := wool.Get(ctx).In("mcp.workspaceResource")

	if s.workspace == nil {
		return []ResourceContents{{
			URI:      "codefly://workspace",
			MimeType: "text/plain",
			Text:     "No workspace loaded",
		}}, nil
	}

	// Marshal workspace to YAML
	data, err := yaml.Marshal(s.workspace)
	if err != nil {
		w.Error("failed to marshal workspace", wool.ErrField(err))
		return nil, err
	}

	return []ResourceContents{{
		URI:      "codefly://workspace",
		MimeType: "application/x-yaml",
		Text:     string(data),
	}}, nil
}

// moduleResource returns a module configuration
func (s *Server) moduleResource(ctx context.Context, moduleName string) ([]ResourceContents, error) {
	w := wool.Get(ctx).In("mcp.moduleResource")

	if s.workspace == nil {
		return nil, fmt.Errorf("no workspace loaded")
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	data, err := yaml.Marshal(mod)
	if err != nil {
		w.Error("failed to marshal module", wool.ErrField(err))
		return nil, err
	}

	return []ResourceContents{{
		URI:      fmt.Sprintf("codefly://module/%s", moduleName),
		MimeType: "application/x-yaml",
		Text:     string(data),
	}}, nil
}

// serviceResource returns a service configuration
func (s *Server) serviceResource(ctx context.Context, moduleName, serviceName string) ([]ResourceContents, error) {
	w := wool.Get(ctx).In("mcp.serviceResource")

	if s.workspace == nil {
		return nil, fmt.Errorf("no workspace loaded")
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	data, err := yaml.Marshal(svc)
	if err != nil {
		w.Error("failed to marshal service", wool.ErrField(err))
		return nil, err
	}

	return []ResourceContents{{
		URI:      fmt.Sprintf("codefly://service/%s/%s", moduleName, serviceName),
		MimeType: "application/x-yaml",
		Text:     string(data),
	}}, nil
}

// endpointsResource returns endpoint information for a service
func (s *Server) endpointsResource(ctx context.Context, moduleName, serviceName string) ([]ResourceContents, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("no workspace loaded")
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("module not found: %s", moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("service not found: %s/%s", moduleName, serviceName)
	}

	endpoints := make([]map[string]any, 0)
	for _, ep := range svc.Endpoints {
		endpoints = append(endpoints, map[string]any{
			"name":       ep.Name,
			"api":        ep.API,
			"visibility": ep.Visibility,
		})
	}

	data, _ := json.MarshalIndent(endpoints, "", "  ")

	return []ResourceContents{{
		URI:      fmt.Sprintf("codefly://endpoints/%s/%s", moduleName, serviceName),
		MimeType: "application/json",
		Text:     string(data),
	}}, nil
}
