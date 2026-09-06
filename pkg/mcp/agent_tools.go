package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/agents/manager"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
)

// registerAgentTools adds tools that report an agent's real manifest.
func (s *Server) registerAgentTools() {
	s.RegisterTool(Tool{
		Name:        "agent_info",
		Description: "Get an agent's real manifest: capabilities, protocols, languages, supported backends, toolchains, validation contract, configuration docs, techniques and README (from GetAgentInformation).",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"agent": {
					Type:        "string",
					Description: `Agent reference: "go-grpc", "codefly.dev/go-grpc", or "codefly.dev/go-grpc:0.0.16". "latest" resolves from the local cache first, then GitHub releases.`,
				},
				agentKindArg: {
					Type:        "string",
					Description: "Agent kind (default: service) — must match the kind list_agents reported for this agent",
					Enum:        agentKindEnumValues,
				},
				"include_prompts": {
					Type:        "string",
					Description: `Include techniques[].prompt in the output when "true" (omitted by default)`,
				},
			},
			Required: []string{"agent"},
		},
	}, s.agentInfo)
}

// agentInfo loads the named agent and returns its GetAgentInformation manifest as JSON.
func (s *Server) agentInfo(ctx context.Context, args map[string]string) ([]Content, error) {
	kind := resources.ServiceAgent
	if raw := args[agentKindArg]; raw != "" {
		mapped, ok := listAgentsKindByArg[raw]
		if !ok {
			return nil, fmt.Errorf("unknown agent kind %q", raw)
		}
		kind = mapped
	}

	conf, err := resources.ParseAgent(ctx, kind, args["agent"])
	if err != nil {
		return nil, fmt.Errorf("cannot parse agent: %w", err)
	}
	// conf.Version is attacker-reachable (an MCP tool argument) and, unlike
	// Name/Publisher, carries no length or charset constraint in the Agent
	// proto. resources.Agent.Path() builds the on-disk binary path by string
	// concatenation ("<publisher>/<name>__<version>") before path.Join, so an
	// unvalidated "../../etc/passwd"-style version escapes the agent cache
	// directory entirely and manager.Load then exec.Command()s whatever it
	// finds there.
	if !isSafeAgentName(conf.Name) || !isSafeAgentName(conf.Publisher) || !isSafeAgentName(conf.Version) {
		return nil, fmt.Errorf("invalid agent reference %q", args["agent"])
	}

	if conf.Version == "latest" {
		if _, resolveErr := manager.ResolveLatest(ctx, conf); resolveErr != nil {
			return nil, fmt.Errorf("cannot resolve latest version: %w", resolveErr)
		}
	}

	// Load under a cache key namespaced to this tool, never a bare
	// conf.Unique() or a service identity: ServiceCacheKey falls back to
	// Agent.Unique() when a service has no identity, so sharing that key
	// space risks a pure introspection call evicting (via the cleanup below)
	// a connection a running service still depends on. The dedicated key
	// also means agent_info's own process is torn down right after the call
	// instead of staying resident for the rest of the (long-lived) MCP
	// server's life for every distinct agent ever inspected.
	cacheKey := "mcp-agent-info::" + conf.Unique()
	defer services.ClearAgent(cacheKey)

	loaded, err := services.LoadAgent(ctx, conf, cacheKey)
	if err != nil {
		return nil, fmt.Errorf("cannot load agent: %w", err)
	}

	info, err := loaded.GetAgentInformation(ctx, &agentv0.AgentInformationRequest{})
	if err != nil {
		return nil, fmt.Errorf("cannot get agent information: %w", err)
	}

	result := mapAgentInformation(conf.Identifier(), info, args["include_prompts"] == "true")
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

type agentInfoToolchain struct {
	Type    string `json:"type"`
	Version string `json:"version"`
}

type agentInfoValidationOp struct {
	Supported bool     `json:"supported"`
	Scopes    []string `json:"scopes,omitempty"`
}

type agentInfoTestSuite struct {
	Name           string `json:"name"`
	DependencyMode string `json:"dependency_mode"`
	Default        bool   `json:"default"`
}

type agentInfoTestValidation struct {
	Supported bool                 `json:"supported"`
	Scopes    []string             `json:"scopes,omitempty"`
	Suites    []agentInfoTestSuite `json:"suites,omitempty"`
}

type agentInfoValidation struct {
	Lint          *agentInfoValidationOp   `json:"lint"`
	Compile       *agentInfoValidationOp   `json:"compile"`
	Test          *agentInfoTestValidation `json:"test"`
	Audit         *agentInfoValidationOp   `json:"audit"`
	ArtifactBuild *agentInfoValidationOp   `json:"artifact_build"`
	Sbom          *agentInfoValidationOp   `json:"sbom"`
	Sync          *agentInfoValidationOp   `json:"sync"`
	SourcePackage *agentInfoValidationOp   `json:"source_package"`
}

type agentInfoConfigField struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type agentInfoConfigDetail struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Fields      []agentInfoConfigField `json:"fields"`
}

type agentInfoTechnique struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Prompt      string   `json:"prompt,omitempty"`
}

// mapAgentInformation renders an AgentInformation proto as the agent_info JSON shape.
func mapAgentInformation(agentRef string, info *agentv0.AgentInformation, includePrompts bool) map[string]any {
	capabilities := make([]string, 0, len(info.GetCapabilities()))
	for _, c := range info.GetCapabilities() {
		capabilities = append(capabilities, strings.ToLower(c.GetType().String()))
	}
	protocols := make([]string, 0, len(info.GetProtocols()))
	for _, p := range info.GetProtocols() {
		protocols = append(protocols, strings.ToLower(p.GetType().String()))
	}
	languages := make([]string, 0, len(info.GetLanguages()))
	for _, l := range info.GetLanguages() {
		languages = append(languages, strings.ToLower(l.GetType().String()))
	}
	backends := make([]string, 0, len(info.GetSupportedBackends()))
	for _, b := range info.GetSupportedBackends() {
		backends = append(backends, strings.ToLower(b.GetType().String()))
	}
	toolchains := make([]agentInfoToolchain, 0, len(info.GetToolchains()))
	for _, t := range info.GetToolchains() {
		toolchains = append(toolchains, agentInfoToolchain{
			Type:    strings.ToLower(t.GetType().String()),
			Version: t.GetVersion(),
		})
	}
	configDetails := make([]agentInfoConfigDetail, 0, len(info.GetConfigurationDetails()))
	for _, cd := range info.GetConfigurationDetails() {
		fields := make([]agentInfoConfigField, 0, len(cd.GetFields()))
		for _, f := range cd.GetFields() {
			fields = append(fields, agentInfoConfigField{Name: f.GetName(), Description: f.GetDescription()})
		}
		configDetails = append(configDetails, agentInfoConfigDetail{
			Name:        cd.GetName(),
			Description: cd.GetDescription(),
			Fields:      fields,
		})
	}
	techniques := make([]agentInfoTechnique, 0, len(info.GetTechniques()))
	for _, t := range info.GetTechniques() {
		technique := agentInfoTechnique{
			ID:          t.GetId(),
			Name:        t.GetName(),
			Description: t.GetDescription(),
			Tags:        t.GetTags(),
		}
		if technique.Tags == nil {
			technique.Tags = []string{}
		}
		if includePrompts {
			technique.Prompt = t.GetPrompt()
		}
		techniques = append(techniques, technique)
	}

	var validation *agentInfoValidation
	if v := info.GetValidation(); v != nil {
		validation = &agentInfoValidation{
			Lint:          mapValidationOp(v.GetLint()),
			Compile:       mapValidationOp(v.GetCompile()),
			Test:          mapTestValidation(v.GetTest()),
			Audit:         mapValidationOp(v.GetAudit()),
			ArtifactBuild: mapValidationOp(v.GetArtifactBuild()),
			Sbom:          mapValidationOp(v.GetSbom()),
			Sync:          mapValidationOp(v.GetSync()),
			SourcePackage: mapValidationOp(v.GetSourcePackage()),
		}
	}

	return map[string]any{
		"agent":                 agentRef,
		"capabilities":          capabilities,
		"protocols":             protocols,
		"languages":             languages,
		"supported_backends":    backends,
		"toolchains":            toolchains,
		"validation":            validation,
		"configuration_details": configDetails,
		"techniques":            techniques,
		"readme":                info.GetReadMe(),
	}
}

func mapValidationOp(op *agentv0.ValidationOperationCapability) *agentInfoValidationOp {
	scopes := make([]string, 0, len(op.GetScopes()))
	for _, scope := range op.GetScopes() {
		scopes = append(scopes, validationScopeString(scope))
	}
	return &agentInfoValidationOp{Supported: op.GetSupported(), Scopes: scopes}
}

func mapTestValidation(t *agentv0.TestValidationCapability) *agentInfoTestValidation {
	scopes := make([]string, 0, len(t.GetScopes()))
	for _, scope := range t.GetScopes() {
		scopes = append(scopes, validationScopeString(scope))
	}
	suites := make([]agentInfoTestSuite, 0, len(t.GetSuites()))
	for _, suite := range t.GetSuites() {
		suites = append(suites, agentInfoTestSuite{
			Name:           suite.GetName(),
			DependencyMode: testDependencyModeString(suite.GetDependencyMode()),
			Default:        suite.GetDefaultSuite(),
		})
	}
	return &agentInfoTestValidation{Supported: t.GetSupported(), Scopes: scopes, Suites: suites}
}

func validationScopeString(scope agentv0.ValidationScope) string {
	return strings.ToLower(strings.TrimPrefix(scope.String(), "VALIDATION_SCOPE_"))
}

func testDependencyModeString(mode agentv0.TestDependencyMode) string {
	return strings.ToLower(strings.TrimPrefix(mode.String(), "TEST_DEPENDENCY_MODE_"))
}
