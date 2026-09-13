package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver"
	blangsemver "github.com/blang/semver"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// agentKindArg is the JSON/schema key used for an agent's kind, shared by the
// list_agents filter argument, service_info/describe's agent object, and the
// list_agents entry shape.
const agentKindArg = "kind"

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
		Description: "List agents known to this machine: agents pinned by workspace services plus agents installed in the local cache. Offline; for languages, protocols and capabilities call agent_info.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				agentKindArg: {
					Type:        "string",
					Description: "Filter by agent kind",
					Enum:        agentKindEnumValues,
				},
			},
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

	if err := s.RegisterTool(Tool{
		Name:        "list_runnables",
		Description: "List runnables (typed finite operations) in a module or all runnables in the workspace, with their immutable module/name@version identity, pinned agent and execution bounds",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				fieldModule: {
					Type:        "string",
					Description: "Module to list runnables from (optional; lists every module when omitted)",
				},
			},
		},
	}, s.listRunnables); err != nil {
		panic(fmt.Errorf("register list_runnables tool: %w", err))
	}

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
// workspaceInfo summarizes the workspace. Delegates enumeration to the control
// plane (Phase-3 adapter); this handler only shapes the JSON.
func (s *Server) workspaceInfo(ctx context.Context, args map[string]string) ([]Content, error) {
	inv, err := s.plane.Inventory(ctx)
	if err != nil {
		return []Content{TextContent("No workspace loaded. Run this command from a codefly workspace directory.")}, nil
	}
	services, err := s.plane.Services(ctx, "")
	if err != nil {
		return nil, err
	}
	svcList := make([]map[string]string, 0, len(services))
	for _, svc := range services {
		svcList = append(svcList, map[string]string{
			"module":  svc.Module,
			"service": svc.Name,
			"agent":   svc.Agent,
		})
	}
	modules := inv.Modules
	if modules == nil {
		modules = []string{}
	}
	info := map[string]any{
		"name":        inv.Workspace,
		"description": inv.Description,
		"modules":     modules,
		"services":    svcList,
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listModules returns all modules in the workspace (via the control plane).
func (s *Server) listModules(ctx context.Context, args map[string]string) ([]Content, error) {
	modules, err := s.plane.Modules(ctx)
	if err != nil {
		return []Content{TextContent("No workspace loaded.")}, nil
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

// listServices returns services (via the control plane), optionally filtered by
// module.
func (s *Server) listServices(ctx context.Context, args map[string]string) ([]Content, error) {
	services, err := s.plane.Services(ctx, args["module"])
	if err != nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}
	result := make([]map[string]any, 0, len(services))
	for _, svc := range services {
		endpoints := make([]map[string]string, 0, len(svc.Endpoints))
		for _, ep := range svc.Endpoints {
			endpoints = append(endpoints, map[string]string{
				"name":       ep.Name,
				"api":        ep.API,
				"visibility": ep.Visibility,
			})
		}
		result = append(result, map[string]any{
			"name":        svc.Name,
			"module":      svc.Module,
			"description": svc.Description,
			"agent":       svc.Agent,
			"version":     svc.Version,
			"endpoints":   endpoints,
		})
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

// agentListEntry is one entry of the list_agents result: an agent identity
// (publisher, name, kind) unioned across workspace pins and the local cache.
type agentListEntry struct {
	Name              string   `json:"name"`
	Publisher         string   `json:"publisher"`
	Kind              string   `json:"kind"`
	InstalledVersions []string `json:"installed_versions"`
	PinnedVersions    []string `json:"pinned_versions"`
	PinnedBy          []string `json:"pinned_by"`
}

// listAgentsKindByArg maps the "kind" argument (list_agents' filter, or
// agent_info's kind override) to the corresponding resources.AgentKind, and
// agentKindEnumValues is the matching ordered vocabulary advertised by both
// tools' schemas. Both come from core's agent-kind registry: a kind core
// registers is one these tools can filter on, with no second list to update.
var (
	listAgentsKindByArg = map[string]resources.AgentKind{}
	agentKindEnumValues []string
)

func init() {
	registry := resources.AgentKindRegistry()
	for i := range registry {
		registration := &registry[i]
		arg := strings.TrimPrefix(string(registration.Resource), "codefly:")
		listAgentsKindByArg[arg] = registration.Resource
		agentKindEnumValues = append(agentKindEnumValues, arg)
	}
}

// legacyServiceAgentKinds recognizes the pre-migration agent.kind spelling
// used throughout every service.codefly.yaml in this codebase (agent: kind:
// runtime::service/builder::service/code::service) — none of which
// resources.AgentKindRegistrationFor recognizes as an alias of ServiceAgent
// (it only knows "codefly:service:runtime" etc). Without this, a pinned
// service's raw kind can never be canonicalized to resources.ServiceAgent,
// so it never lines up with the canonical kind recorded for the same agent
// found in the local cache, and list_agents shows two rows for one agent
// instead of merging them (and the kind filter drops every pinned agent).
var legacyServiceAgentKinds = map[string]bool{
	"runtime::service": true,
	"builder::service": true,
	"code::service":    true,
}

// canonicalAgentKind normalizes a raw agent.kind value (from a workspace pin
// or the cache's resources.AgentKindRegistry) to the canonical
// resources.AgentKind string used as both the list_agents merge key and the
// kind filter's comparison value. It falls back to the raw value when it
// cannot be resolved, so an unrecognized kind still groups consistently with
// itself instead of being dropped.
func canonicalAgentKind(raw string) string {
	if reg, err := resources.AgentKindRegistrationFor(resources.AgentKind(raw)); err == nil {
		return string(reg.Resource)
	}
	if legacyServiceAgentKinds[raw] {
		return string(resources.ServiceAgent)
	}
	return raw
}

// listAgents unions agents pinned by workspace services with agents installed
// in the local cache. It makes no network calls.
func (s *Server) listAgents(ctx context.Context, args map[string]string) ([]Content, error) {
	type agentKey struct {
		publisher string
		name      string
		kind      string
	}
	entries := make(map[agentKey]*agentListEntry)
	getEntry := func(publisher, name, kind string) *agentListEntry {
		key := agentKey{publisher, name, kind}
		entry, ok := entries[key]
		if !ok {
			entry = &agentListEntry{
				Name:              name,
				Publisher:         publisher,
				Kind:              kind,
				InstalledVersions: []string{},
				PinnedVersions:    []string{},
				PinnedBy:          []string{},
			}
			entries[key] = entry
		}
		return entry
	}

	if s.workspace != nil {
		modules, err := s.workspace.LoadModules(ctx)
		if err != nil {
			return nil, err
		}
		for _, mod := range modules {
			runnables, err := mod.LoadRunnables(ctx)
			if err != nil {
				return nil, fmt.Errorf("cannot load runnables of module %s: %w", mod.Name, err)
			}
			for _, runnable := range runnables {
				entry := getEntry(runnable.Agent.Publisher, runnable.Agent.Name, canonicalAgentKind(string(runnable.Agent.Kind)))
				entry.PinnedBy = append(entry.PinnedBy, mod.Name+"/"+runnable.Name)
				entry.PinnedVersions = append(entry.PinnedVersions, runnable.Agent.Version)
			}
			services, _ := mod.LoadServices(ctx)
			for _, svc := range services {
				if svc.Agent == nil {
					continue
				}
				entry := getEntry(svc.Agent.Publisher, svc.Agent.Name, canonicalAgentKind(string(svc.Agent.Kind)))
				entry.PinnedBy = append(entry.PinnedBy, mod.Name+"/"+svc.Name)
				entry.PinnedVersions = append(entry.PinnedVersions, svc.Agent.Version)
			}
		}
	}

	registry := resources.AgentKindRegistry()
	for i := range registry {
		reg := &registry[i]
		if !reg.Operations.List || reg.InstallSubdirectory == "" {
			continue
		}
		base := filepath.Join(resources.AgentBase(ctx), "agents", reg.InstallSubdirectory)
		publishers, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, pub := range publishers {
			publisherDir := filepath.Join(base, pub.Name())
			installs, err := os.ReadDir(publisherDir)
			if err != nil {
				continue
			}
			for _, install := range installs {
				name, version, ok := strings.Cut(install.Name(), "__")
				if !ok {
					continue
				}
				if _, err := blangsemver.Parse(version); err != nil {
					continue
				}
				entry := getEntry(pub.Name(), name, canonicalAgentKind(string(reg.Resource)))
				entry.InstalledVersions = append(entry.InstalledVersions, version)
			}
		}
	}

	var kindFilter string
	if raw := args[agentKindArg]; raw != "" {
		mapped, ok := listAgentsKindByArg[raw]
		if !ok {
			data, _ := json.MarshalIndent([]agentListEntry{}, "", "  ")
			return []Content{TextContent(string(data))}, nil
		}
		kindFilter = string(mapped)
	}

	result := make([]agentListEntry, 0, len(entries))
	for _, entry := range entries {
		if kindFilter != "" && entry.Kind != kindFilter {
			continue
		}
		sortVersionsDescending(entry.InstalledVersions)
		sortVersionsDescending(entry.PinnedVersions)
		result = append(result, *entry)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Publisher != result[j].Publisher {
			return result[i].Publisher < result[j].Publisher
		}
		return result[i].Name < result[j].Name
	})

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// sortVersionsDescending sorts versions newest-first by semver. Versions come
// from two different sources with different rigor: cache directory names are
// pre-validated strict semver (MAJOR.MINOR.PATCH), but a workspace pin's
// agent.version is a free-form YAML string (e.g. "1.10", missing its patch
// component) — blang/semver rejects that outright. Comparing per-pair
// ("if both parse, semver-compare; else string-compare") is not a
// transitive order: whether a given pair falls back to string comparison
// depends on that pair's own parseability, so e.g. ["2.0.0", "1.10",
// "1.9.0"] can come out with 1.10 sorted below 1.9.0 even though 1.10 is
// the newer version. Using Masterminds/semver (which accepts a missing
// minor/patch) as a lenient fallback, and computing each element's sort key
// once up front, makes every pairwise comparison consistent: version-like
// strings always sort by their real numeric value, and only strings that
// aren't version-shaped at all fall back to a lexical order among
// themselves (ranked below every version-shaped string, not interleaved
// with them).
func sortVersionsDescending(versions []string) {
	type key struct {
		parsed *semver.Version
		raw    string
	}
	keys := make([]key, len(versions))
	for i, v := range versions {
		parsed, err := semver.NewVersion(v)
		if err != nil {
			parsed = nil
		}
		keys[i] = key{parsed: parsed, raw: v}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.parsed != nil && b.parsed != nil {
			return a.parsed.GreaterThan(b.parsed)
		}
		if (a.parsed != nil) != (b.parsed != nil) {
			return a.parsed != nil
		}
		return a.raw > b.raw
	})
	for i, k := range keys {
		versions[i] = k.raw
	}
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

// listRunnables reports the workspace's runnables. A runnable whose
// declaration does not load fails the call rather than shortening the list:
// a caller about to build or install what it finds must not be told a broken
// runnable is absent.
func (s *Server) listRunnables(ctx context.Context, args map[string]string) ([]Content, error) {
	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}

	var modules []*resources.Module
	var err error
	if name := args[fieldModule]; name != "" {
		var module *resources.Module
		module, err = s.workspace.LoadModuleFromName(ctx, name)
		if err == nil {
			modules = []*resources.Module{module}
		}
	} else {
		modules, err = s.workspace.LoadModules(ctx)
	}
	if err != nil {
		return nil, err
	}

	result := make([]map[string]any, 0)
	for _, m := range modules {
		runnables, err := m.LoadRunnables(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot load runnables of module %s: %w", m.Name, err)
		}
		for _, runnable := range runnables {
			facilities := make([]string, 0, len(runnable.Execution.Facilities))
			for _, facility := range runnable.Execution.Facilities {
				facilities = append(facilities, string(facility))
			}
			result = append(result, map[string]any{
				"name":        runnable.Name,
				"module":      m.Name,
				"version":     runnable.Version,
				"description": runnable.Description,
				"agent":       runnable.Agent.Identifier(),
				"protocol":    runnable.Contract.Protocol,
				"execution": map[string]any{
					"facilities":       facilities,
					"timeout":          runnable.Execution.Timeout,
					"cancellation":     string(runnable.Execution.Cancellation),
					"recovery":         string(runnable.Execution.Recovery),
					"max_input_bytes":  runnable.Execution.MaxInputBytes(),
					"max_output_bytes": runnable.Execution.MaxOutputBytes(),
				},
			})
		}
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}
