package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/docs/runbooks"
)

// registerHelpTools exposes the codefly how-to runbooks (docs/runbooks) as an
// MCP tool so agents connected to `codefly mcp` can fetch step-by-step
// procedures without repository access.
func (s *Server) registerHelpTools() {
	s.RegisterTool(Tool{
		Name: "how_to",
		Description: "Get codefly how-to runbooks: step-by-step procedures for tasks such as " +
			"bumping the Go version, cutting a release, adding a CLI command, or rebuilding the " +
			"CLI and agents. Call with no arguments to list available topics; pass 'topic' to " +
			"get a full runbook.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]PropertySchema{
				"topic": {
					Type:        "string",
					Description: `Runbook topic name, e.g. "bump-go-version". Omit to list all topics.`,
				},
			},
		},
	}, s.howTo)
}

func (s *Server) howTo(_ context.Context, args map[string]string) ([]Content, error) {
	topic := strings.TrimSpace(args["topic"])
	if topic == "" {
		var b strings.Builder
		b.WriteString("codefly runbooks — call how_to with topic=<name> for the full procedure:\n\n")
		for _, r := range runbooks.List() {
			fmt.Fprintf(&b, "- %s — %s\n", r.Name, r.Summary)
		}
		return []Content{TextContent(b.String())}, nil
	}
	r, err := runbooks.Get(topic)
	if err != nil {
		return []Content{TextContent(fmt.Sprintf(
			"Unknown runbook %q. Available topics: %s",
			topic, strings.Join(runbooks.Names(), ", "),
		))}, nil
	}
	return []Content{TextContent(r.Content)}, nil
}
