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
	"github.com/codefly-dev/cli/pkg/agentkinds"
	"github.com/codefly-dev/cli/pkg/prerelease"
	runnablespkg "github.com/codefly-dev/cli/pkg/runnables"
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
	s.register(&Tool{
		Name:        "workspace_info",
		Description: "Get information about the current codefly workspace including modules and services",
		InputSchema: InputSchema{
			Type:       schemaTypeObject,
			Properties: map[string]PropertySchema{},
		},
	}, s.workspaceInfo)

	// Exposed deliberately: an agent writing an agent pin is exactly who trips
	// the prerelease gate, and this lets it check its own edit before a human
	// reviews the pull request. Read-only — it reads committed text and changes
	// nothing.
	s.register(&Tool{
		Name:        "check_prerelease_versions",
		Description: "Check the workspace repository for prerelease version pins that must not reach the default branch (semver prereleases like 0.1.48-dev.abc, Go pseudo-versions). Returns the same report as `codefly ci prerelease`. Pass release=true for the stricter scope a tag is cut in, where even a labelled agent-overrides dev pin is refused.",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				"release": {
					Type:        schemaTypeString,
					Description: "\"true\" to use the release scope, which permits no prerelease anywhere, not even a labelled agent-overrides entry",
				},
				"go_modules": {
					Type:        schemaTypeString,
					Description: "\"true\" to also refuse first-party Go pseudo-versions in go.mod instead of only reporting them",
				},
			},
		},
	}, s.checkPrereleaseVersions)

	s.register(&Tool{
		Name:        "list_modules",
		Description: "List all modules in the workspace",
		InputSchema: InputSchema{
			Type:       schemaTypeObject,
			Properties: map[string]PropertySchema{},
		},
	}, s.listModules)

	s.register(&Tool{
		Name:        "list_services",
		Description: "List services in a module or all services in the workspace",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule: {
					Type:        schemaTypeString,
					Description: "Module name (optional, lists all if not provided)",
				},
			},
		},
	}, s.listServices)

	s.register(&Tool{
		Name:        "service_info",
		Description: "Get detailed information about a specific service",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule: {
					Type:        schemaTypeString,
					Description: "Module containing the service",
				},
				fieldService: {
					Type:        schemaTypeString,
					Description: describeServiceName,
				},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.serviceInfo)

	s.register(&Tool{
		Name:        "service_dependencies",
		Description: "Get dependencies of a service",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule: {
					Type:        schemaTypeString,
					Description: "Module containing the service",
				},
				fieldService: {
					Type:        schemaTypeString,
					Description: describeServiceName,
				},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.serviceDependencies)

	s.register(&Tool{
		Name:        "list_agents",
		Description: "List agents known to this machine: agents pinned by workspace services plus agents installed in the local cache. Offline; for languages, protocols and capabilities call agent_info.",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				agentKindArg: {
					Type:        schemaTypeString,
					Description: "Filter by agent kind",
					Enum:        agentKindEnumValues,
				},
			},
		},
	}, s.listAgents)

	s.register(&Tool{
		Name:        "list_jobs",
		Description: "List jobs in a module or all jobs in the workspace",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule: {
					Type:        schemaTypeString,
					Description: "Module name (optional, lists all if not provided)",
				},
			},
		},
	}, s.listJobs)

	s.register(&Tool{
		Name:        "list_runnables",
		Description: "List runnables (typed finite operations) in a module or all runnables in the workspace, with their immutable module/name@version identity, pinned agent and execution bounds. Includes the operations a module derives from the gRPC methods its contracts mark, which carry the service facility and name their source method",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule: {
					Type:        schemaTypeString,
					Description: "Module to list runnables from (optional; lists every module when omitted)",
				},
			},
		},
	}, s.listRunnables)

	// Per-service tools for Mind (design 013)
	s.register(&Tool{
		Name:        "describe",
		Description: "Get service metadata: name, type, language, file list (for Mind agent)",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.describe)

	s.register(&Tool{
		Name:        "read_file",
		Description: "Read a file from the service directory (path relative to service)",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
				fieldPath:    {Type: schemaTypeString, Description: "Relative path within the service"},
			},
			Required: []string{fieldModule, fieldService, fieldPath},
		},
	}, s.readFile)

	s.register(&Tool{
		Name:        "write_file",
		Description: "Write content to a file in the service directory",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
				fieldPath:    {Type: schemaTypeString, Description: "Relative path within the service"},
				"content":    {Type: schemaTypeString, Description: "File content"},
			},
			Required: []string{fieldModule, fieldService, fieldPath, "content"},
		},
	}, s.writeFile)

	s.register(&Tool{
		Name:        "build",
		Description: "Build the service via the plugin (builder)",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.build)

	s.register(&Tool{
		Name:        "run_checks",
		Description: "Run a command in the service directory (e.g. go test ./...). Optional 'command' arg; default: go test ./...",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
				fieldCommand: {Type: schemaTypeString, Description: "Command to run (default: go test ./...)"},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.runChecks)

	s.register(&Tool{
		Name:        "stop",
		Description: "Stop the service runtime if running",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.stop)

	// Agent command tools — expose agent-registered commands to MCP
	s.register(&Tool{
		Name:        "list_service_commands",
		Description: "List commands available on a service agent (e.g. test, lint, screenshot, health)",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
			},
			Required: []string{fieldModule, fieldService},
		},
	}, s.listServiceCommands)

	s.register(&Tool{
		Name:        "run_service_command",
		Description: "Run a command on a service agent (e.g. test, lint, screenshot, health, proto)",
		InputSchema: InputSchema{
			Type: schemaTypeObject,
			Properties: map[string]PropertySchema{
				fieldModule:  {Type: schemaTypeString, Description: describeModuleName},
				fieldService: {Type: schemaTypeString, Description: describeServiceName},
				fieldCommand: {Type: schemaTypeString, Description: "Command name"},
				"args":       {Type: schemaTypeString, Description: "Command arguments (space-separated)"},
			},
			Required: []string{fieldModule, fieldService, fieldCommand},
		},
	}, s.runServiceCommand)
}

// workspaceInfo returns information about the current workspace
// workspaceInfo summarizes the workspace. Delegates enumeration to the control
// plane (Phase-3 adapter); this handler only shapes the JSON.
// checkPrereleaseVersions runs the prerelease gate over the workspace's own
// repository. It is the same scan `codefly ci prerelease` performs, so an agent
// and the pull request it opens cannot disagree about whether a pin is acceptable.
func (s *Server) checkPrereleaseVersions(_ context.Context, args map[string]string) ([]Content, error) {
	if s.workspace == nil {
		return []Content{TextContent("No workspace loaded. Run this from a codefly workspace directory.")}, nil
	}
	result, err := prerelease.Scan(s.workspace.Dir(), prerelease.Options{
		Release:   strings.EqualFold(strings.TrimSpace(args["release"]), "true"),
		GoModules: strings.EqualFold(strings.TrimSpace(args["go_modules"]), "true"),
	})
	if err != nil {
		return nil, err
	}
	payload, err := result.JSON()
	if err != nil {
		return nil, err
	}
	return []Content{TextContent(string(payload))}, nil
}

func (s *Server) workspaceInfo(ctx context.Context, _ map[string]string) ([]Content, error) {
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
			fieldModule:  svc.Module,
			fieldService: svc.Name,
			fieldAgent:   svc.Agent,
		})
	}
	modules := inv.Modules
	if modules == nil {
		modules = []string{}
	}
	info := map[string]any{
		fieldName:        inv.Workspace,
		fieldDescription: inv.Description,
		"modules":        modules,
		"services":       svcList,
	}
	data, _ := json.MarshalIndent(info, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listModules returns all modules in the workspace (via the control plane).
func (s *Server) listModules(ctx context.Context, _ map[string]string) ([]Content, error) {
	modules, err := s.plane.Modules(ctx)
	if err != nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}
	result := make([]map[string]any, 0, len(modules))
	for _, m := range modules {
		result = append(result, map[string]any{
			fieldName:        m.Name,
			fieldDescription: m.Description,
		})
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// listServices returns services (via the control plane), optionally filtered by
// module.
func (s *Server) listServices(ctx context.Context, args map[string]string) ([]Content, error) {
	services, err := s.plane.Services(ctx, args[fieldModule])
	if err != nil {
		return []Content{TextContent("No workspace loaded.")}, nil
	}
	result := make([]map[string]any, 0, len(services))
	for _, svc := range services {
		endpoints := make([]map[string]string, 0, len(svc.Endpoints))
		for _, ep := range svc.Endpoints {
			endpoints = append(endpoints, map[string]string{
				fieldName:       ep.Name,
				fieldAPI:        ep.API,
				fieldVisibility: ep.Visibility,
			})
		}
		result = append(result, map[string]any{
			fieldName:        svc.Name,
			fieldModule:      svc.Module,
			fieldDescription: svc.Description,
			fieldAgent:       svc.Agent,
			fieldVersion:     svc.Version,
			"endpoints":      endpoints,
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

	moduleName := args[fieldModule]
	serviceName := args[fieldService]

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
		fieldName:        svc.Name,
		fieldModule:      moduleName,
		fieldDescription: svc.Description,
		fieldVersion:     svc.Version,
		fieldAgent: map[string]string{
			fieldName:    svc.Agent.Name,
			"kind":       string(svc.Agent.Kind),
			"publisher":  svc.Agent.Publisher,
			fieldVersion: svc.Agent.Version,
		},
	}

	// Endpoints
	endpoints := make([]map[string]any, 0)
	for _, ep := range svc.Endpoints {
		endpoints = append(endpoints, map[string]any{
			fieldName:       ep.Name,
			fieldAPI:        ep.API,
			fieldVisibility: ep.Visibility,
		})
	}
	info["endpoints"] = endpoints

	// Dependencies
	deps := make([]map[string]any, 0)
	for _, dep := range svc.ServiceDependencies {
		depInfo := map[string]any{
			fieldName:   dep.Name,
			fieldModule: dep.Module,
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

	moduleName := args[fieldModule]
	serviceName := args[fieldService]

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
			fieldName:   dep.Name,
			fieldModule: dep.Module,
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

// agentKindEnumValues is the ordered kind vocabulary advertised by both
// list_agents' and agent_info's schemas. It comes from pkg/agentkinds, which
// owns the short-form ↔ registered-kind mapping for the whole CLI: a kind core
// registers is one these tools can filter on, with no second list to update
// and no second copy of the "codefly:" convention to drift.
var agentKindEnumValues = agentkinds.Vocabulary()

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
		mapped, err := agentkinds.Resolve(raw)
		if err != nil {
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

	moduleFilter := args[fieldModule]
	result := make([]map[string]any, 0)

	for _, m := range modules {
		if moduleFilter != "" && m.Name != moduleFilter {
			continue
		}

		jobs, err := m.LoadJobs(ctx)
		if err != nil {
			w.Warn("failed to load jobs", wool.Field(fieldModule, m.Name), wool.ErrField(err))
			continue
		}

		for _, job := range jobs {
			jobInfo := map[string]any{
				fieldName:        job.Name,
				fieldModule:      m.Name,
				fieldDescription: job.Description,
				fieldVersion:     job.Version,
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
				jobInfo[fieldAgent] = job.Agent.Name
			}

			// Add service dependencies
			if len(job.ServiceDependencies) > 0 {
				deps := make([]map[string]string, 0)
				for _, dep := range job.ServiceDependencies {
					deps = append(deps, map[string]string{
						fieldName:   dep.Name,
						fieldModule: dep.Module,
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

	result := make([]runnablespkg.Identity, 0)
	for _, m := range modules {
		runnables, err := m.LoadRunnables(ctx)
		if err != nil {
			return nil, fmt.Errorf("cannot load runnables of module %s: %w", m.Name, err)
		}
		for _, runnable := range runnables {
			result = append(result, runnablespkg.NewIdentity(runnable))
		}
		derived, err := runnablespkg.LoadDerivedOperations(m.Dir())
		if err != nil {
			return nil, fmt.Errorf("cannot load the derived runnables of module %s: %w", m.Name, err)
		}
		for i := range derived {
			result = append(result, derived[i].Identity())
		}
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}
