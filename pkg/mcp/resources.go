package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
)

const (
	workspaceURI     = "codefly://workspace"
	moduleTemplate   = "codefly://module/{module}"
	serviceTemplate  = "codefly://service/{module}/{service}"
	endpointTemplate = "codefly://endpoints/{module}/{service}"
)

// errResourceNotFound is wrapped by handlers when a requested module or
// service is missing, so the dispatcher can tell a not-found from a genuine
// internal error without string-matching messages.
var errResourceNotFound = errors.New("resource not found")

// registerResources sets up all available MCP resources
func (s *Server) registerResources() {
	s.RegisterResource(Resource{
		URI:         workspaceURI,
		Name:        "Workspace Configuration",
		Description: "The workspace.codefly.yaml configuration file",
		MimeType:    "application/x-yaml",
	}, s.workspaceResource)

	s.resourceTemplates = []ResourceTemplate{
		{URITemplate: moduleTemplate, Name: "Module Configuration", Description: "module.codefly.yaml for one module", MimeType: "application/x-yaml"},
		{URITemplate: serviceTemplate, Name: "Service Configuration", Description: "service.codefly.yaml for one service", MimeType: "application/x-yaml"},
		{URITemplate: endpointTemplate, Name: "Service Endpoints", Description: "Declared endpoints (name, api, visibility) for one service", MimeType: "application/json"},
	}
}

// resolveResource maps a concrete URI to a handler. Static URIs win; otherwise
// the codefly:// path is split and matched against the three templates.
// ok=false means "not found" (the caller answers ResourceNotFound).
func (s *Server) resolveResource(uri string) (ResourceHandler, bool) {
	if h, ok := s.resources[uri]; ok {
		return h, true
	}
	rest, ok := strings.CutPrefix(uri, "codefly://")
	if !ok {
		return nil, false
	}
	parts := strings.Split(rest, "/")
	for _, p := range parts {
		if p == "" {
			return nil, false
		}
	}
	switch {
	case parts[0] == "module" && len(parts) == 2:
		name := parts[1]
		return func(ctx context.Context) ([]ResourceContents, error) { return s.moduleResource(ctx, name) }, true
	case parts[0] == "service" && len(parts) == 3:
		mod, svc := parts[1], parts[2]
		return func(ctx context.Context) ([]ResourceContents, error) { return s.serviceResource(ctx, mod, svc) }, true
	case parts[0] == "endpoints" && len(parts) == 3:
		mod, svc := parts[1], parts[2]
		return func(ctx context.Context) ([]ResourceContents, error) { return s.endpointsResource(ctx, mod, svc) }, true
	}
	return nil, false
}

// listConcreteResources enumerates the static resources plus, when a
// workspace is loaded, one resource per module and per service.
func (s *Server) listConcreteResources(ctx context.Context) []Resource {
	w := wool.Get(ctx).In("mcp.listConcreteResources")

	out := append([]Resource{}, s.resDefs...)
	if s.workspace == nil {
		return out
	}

	mods, err := s.workspace.LoadModules(ctx)
	if err != nil {
		w.Debug("failed to load modules", wool.ErrField(err))
		return out
	}

	for _, mod := range mods {
		out = append(out, Resource{
			URI:         fmt.Sprintf("codefly://module/%s", mod.Name),
			Name:        fmt.Sprintf("Module %s", mod.Name),
			Description: "module.codefly.yaml for one module",
			MimeType:    "application/x-yaml",
		})

		svcs, err := mod.LoadServices(ctx)
		if err != nil {
			w.Debug("failed to load services", wool.Field("module", mod.Name), wool.ErrField(err))
			continue
		}
		for _, svc := range svcs {
			out = append(out,
				Resource{
					URI:         fmt.Sprintf("codefly://service/%s/%s", mod.Name, svc.Name),
					Name:        fmt.Sprintf("Service %s/%s", mod.Name, svc.Name),
					Description: "service.codefly.yaml for one service",
					MimeType:    "application/x-yaml",
				},
				Resource{
					URI:         fmt.Sprintf("codefly://endpoints/%s/%s", mod.Name, svc.Name),
					Name:        fmt.Sprintf("Endpoints %s/%s", mod.Name, svc.Name),
					Description: "Declared endpoints (name, api, visibility) for one service",
					MimeType:    "application/json",
				},
			)
		}
	}

	return out
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
		return nil, fmt.Errorf("%w: %s", errResourceNotFound, moduleName)
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errResourceNotFound, moduleName)
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
		return nil, fmt.Errorf("%w: %s/%s", errResourceNotFound, moduleName, serviceName)
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errResourceNotFound, moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s", errResourceNotFound, moduleName, serviceName)
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
		return nil, fmt.Errorf("%w: %s/%s", errResourceNotFound, moduleName, serviceName)
	}

	mod, err := s.workspace.LoadModuleFromName(ctx, moduleName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", errResourceNotFound, moduleName)
	}

	svc, err := mod.LoadServiceFromName(ctx, serviceName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/%s", errResourceNotFound, moduleName, serviceName)
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
