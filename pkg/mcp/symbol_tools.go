package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	coreservices "github.com/codefly-dev/core/agents/services"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/wool"
)

// registerSymbolTools adds LSP-backed symbol intelligence tools to the MCP server.
// These tools wrap the plugin's gRPC Code service (which internally uses gopls).
// Mind calls these via MCP to get semantic understanding without touching LSP directly.
func (s *Server) registerSymbolTools() {
	s.RegisterTool(Tool{
		Name:        "symbols/references",
		Description: "Find all references to a symbol at the given file:line:column position. Returns locations across the codebase where this symbol is used.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"file":    {Type: "string", Description: "Relative file path"},
				"line":    {Type: "string", Description: "1-based line number"},
				"column":  {Type: "string", Description: "1-based column number"},
			},
			Required: []string{"module", "service", "file", "line", "column"},
		},
	}, s.symbolReferences)

	s.RegisterTool(Tool{
		Name:        "symbols/definition",
		Description: "Go to the definition of a symbol at the given file:line:column position. Returns the location where the symbol is defined.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"file":    {Type: "string", Description: "Relative file path"},
				"line":    {Type: "string", Description: "1-based line number"},
				"column":  {Type: "string", Description: "1-based column number"},
			},
			Required: []string{"module", "service", "file", "line", "column"},
		},
	}, s.symbolDefinition)

	s.RegisterTool(Tool{
		Name:        "symbols/diagnostics",
		Description: "Get compiler/linter diagnostics for a file or the entire service. Returns structured errors with file, line, message, severity.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"file":    {Type: "string", Description: "Relative file path (optional, omit for all files)"},
			},
			Required: []string{"module", "service"},
		},
	}, s.symbolDiagnostics)

	s.RegisterTool(Tool{
		Name:        "symbols/hover",
		Description: "Get type information and documentation for a symbol at the given position.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"file":    {Type: "string", Description: "Relative file path"},
				"line":    {Type: "string", Description: "1-based line number"},
				"column":  {Type: "string", Description: "1-based column number"},
			},
			Required: []string{"module", "service", "file", "line", "column"},
		},
	}, s.symbolHover)

	s.RegisterTool(Tool{
		Name:        "symbols/impact",
		Description: "Compute the impact surface of modifying symbols. Given a list of symbols (file:line:column), find all other symbols that reference them, transitively up to a configurable depth. This is the key tool for surface-based conflict detection.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"module":  {Type: "string", Description: "Module name"},
				"service": {Type: "string", Description: "Service name"},
				"symbols": {Type: "string", Description: "JSON array of {file, line, column} objects identifying symbols to analyze"},
				"depth":   {Type: "string", Description: "Max transitive depth (default: 1, max: 3)"},
			},
			Required: []string{"module", "service", "symbols"},
		},
	}, s.symbolImpact)
}

// loadCodeAgent loads the code agent for a service.
func (s *Server) loadCodeAgent(ctx context.Context, moduleName, serviceName string) (*coreservices.CodeAgent, error) {
	_, instance, err := s.getServiceAndInstance(ctx, moduleName, serviceName)
	if err != nil {
		return nil, err
	}
	codeAgent, err := services.LoadCode(ctx, instance.Service)
	if err != nil {
		return nil, fmt.Errorf("cannot load code agent: %w", err)
	}
	return codeAgent, nil
}

func (s *Server) symbolReferences(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.symbols/references")

	line, col, err := parseLineCol(args)
	if err != nil {
		return nil, err
	}

	agent, err := s.loadCodeAgent(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}

	resp, err := agent.FindReferences(ctx, &codev0.FindReferencesRequest{
		File:   args["file"],
		Line:   int32(line),
		Column: int32(col),
	})
	if err != nil {
		return nil, fmt.Errorf("find_references: %w", err)
	}

	refs := make([]map[string]any, 0, len(resp.Locations))
	for _, loc := range resp.Locations {
		refs = append(refs, locationToMap(loc))
	}

	w.Debug("references", wool.Field("count", len(refs)))
	data, _ := json.MarshalIndent(refs, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) symbolDefinition(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.symbols/definition")

	line, col, err := parseLineCol(args)
	if err != nil {
		return nil, err
	}

	agent, err := s.loadCodeAgent(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}

	resp, err := agent.GoToDefinition(ctx, &codev0.GoToDefinitionRequest{
		File:   args["file"],
		Line:   int32(line),
		Column: int32(col),
	})
	if err != nil {
		return nil, fmt.Errorf("go_to_definition: %w", err)
	}

	defs := make([]map[string]any, 0, len(resp.Locations))
	for _, loc := range resp.Locations {
		defs = append(defs, locationToMap(loc))
	}

	w.Debug("definition", wool.Field("count", len(defs)))
	data, _ := json.MarshalIndent(defs, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) symbolDiagnostics(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.symbols/diagnostics")

	agent, err := s.loadCodeAgent(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}

	resp, err := agent.GetDiagnostics(ctx, &codev0.GetDiagnosticsRequest{
		File: args["file"],
	})
	if err != nil {
		return nil, fmt.Errorf("get_diagnostics: %w", err)
	}

	diags := make([]map[string]any, 0, len(resp.Diagnostics))
	for _, d := range resp.Diagnostics {
		entry := map[string]any{
			"file":       d.File,
			"line":       d.Line,
			"column":     d.Column,
			"end_line":   d.EndLine,
			"end_column": d.EndColumn,
			"message":    d.Message,
			"severity":   d.Severity.String(),
		}
		if d.Source != "" {
			entry["source"] = d.Source
		}
		if d.Code != "" {
			entry["code"] = d.Code
		}
		diags = append(diags, entry)
	}

	w.Debug("diagnostics", wool.Field("count", len(diags)))
	data, _ := json.MarshalIndent(diags, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

func (s *Server) symbolHover(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.symbols/hover")

	line, col, err := parseLineCol(args)
	if err != nil {
		return nil, err
	}

	agent, err := s.loadCodeAgent(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}

	resp, err := agent.GetHoverInfo(ctx, &codev0.GetHoverInfoRequest{
		File:   args["file"],
		Line:   int32(line),
		Column: int32(col),
	})
	if err != nil {
		return nil, fmt.Errorf("get_hover_info: %w", err)
	}

	result := map[string]any{
		"content": resp.Content,
	}
	if resp.Language != "" {
		result["language"] = resp.Language
	}

	w.Debug("hover", wool.Field("has_content", resp.Content != ""))
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// symbolImpact computes the transitive impact surface of modifying a set of symbols.
// For each input symbol, it finds all references, then optionally finds references
// to those references (up to the specified depth).
func (s *Server) symbolImpact(ctx context.Context, args map[string]string) ([]Content, error) {
	w := wool.Get(ctx).In("mcp.symbols/impact")

	agent, err := s.loadCodeAgent(ctx, args["module"], args["service"])
	if err != nil {
		return nil, err
	}

	type symbolPos struct {
		File   string `json:"file"`
		Line   int32  `json:"line"`
		Column int32  `json:"column"`
	}

	var symbols []symbolPos
	if err := json.Unmarshal([]byte(args["symbols"]), &symbols); err != nil {
		return nil, fmt.Errorf("invalid symbols JSON: %w", err)
	}

	maxDepth := 1
	if d := args["depth"]; d != "" {
		fmt.Sscanf(d, "%d", &maxDepth)
	}
	if maxDepth > 3 {
		maxDepth = 3
	}

	// BFS: expand references at each depth level.
	type locKey struct {
		File string
		Line int32
	}
	seen := make(map[locKey]bool)
	current := make([]symbolPos, len(symbols))
	copy(current, symbols)

	// Mark input symbols as seen.
	for _, s := range symbols {
		seen[locKey{s.File, s.Line}] = true
	}

	type impactEntry struct {
		File   string `json:"file"`
		Line   int32  `json:"line"`
		Column int32  `json:"column"`
		Depth  int    `json:"depth"`
	}
	var impact []impactEntry

	for depth := 0; depth < maxDepth; depth++ {
		var next []symbolPos
		for _, sym := range current {
			resp, err := agent.FindReferences(ctx, &codev0.FindReferencesRequest{
				File:   sym.File,
				Line:   sym.Line,
				Column: sym.Column,
			})
			if err != nil {
				w.Warn("references failed", wool.Field("file", sym.File), wool.ErrField(err))
				continue
			}
			for _, loc := range resp.Locations {
				key := locKey{loc.File, loc.Line}
				if seen[key] {
					continue
				}
				seen[key] = true
				impact = append(impact, impactEntry{
					File:   loc.File,
					Line:   loc.Line,
					Column: loc.Column,
					Depth:  depth + 1,
				})
				next = append(next, symbolPos{File: loc.File, Line: loc.Line, Column: loc.Column})
			}
		}
		current = next
	}

	result := map[string]any{
		"input_symbols": len(symbols),
		"impact_count":  len(impact),
		"max_depth":     maxDepth,
		"impact":        impact,
	}

	w.Debug("impact", wool.Field("input", len(symbols)), wool.Field("impact", len(impact)))
	data, _ := json.MarshalIndent(result, "", "  ")
	return []Content{TextContent(string(data))}, nil
}

// parseLineCol extracts line and column from MCP args (string values).
func parseLineCol(args map[string]string) (int, int, error) {
	var line, col int
	if _, err := fmt.Sscanf(args["line"], "%d", &line); err != nil {
		return 0, 0, fmt.Errorf("invalid line: %s", args["line"])
	}
	if _, err := fmt.Sscanf(args["column"], "%d", &col); err != nil {
		return 0, 0, fmt.Errorf("invalid column: %s", args["column"])
	}
	return line, col, nil
}

// locationToMap converts a proto Location to a JSON-friendly map.
func locationToMap(loc *codev0.Location) map[string]any {
	if loc == nil {
		return map[string]any{}
	}
	return map[string]any{
		"file":       loc.File,
		"line":       loc.Line,
		"column":     loc.Column,
		"end_line":   loc.EndLine,
		"end_column": loc.EndColumn,
	}
}
