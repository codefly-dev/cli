package update

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"gopkg.in/yaml.v3"

	"github.com/codefly-dev/cli/pkg/cli"
)

// generatedHeader matches the machine-generated marker that go:generate-style
// tools stamp on files; generatedSource pulls out the "from <source>" clause
// when the marker names the file it was rendered from.
var (
	generatedHeader = regexp.MustCompile(`Code generated .* DO NOT EDIT\.`)
	generatedSource = regexp.MustCompile(`Code generated from (.+?)\. DO NOT EDIT\.`)
)

// updateServiceAgent bumps a service's agent to its latest compatible release
// with a surgical, node-preserving edit of agent.version. It never reserializes
// resources.Service, so unmodeled keys, comments, and formatting survive. It
// refuses to touch files carrying a "Code generated ... DO NOT EDIT" marker,
// pointing at the source to edit instead. Returns nil when nothing changed.
func updateServiceAgent(ctx context.Context, svc *resources.Service) (*services.AgentUpdate, error) {
	file := filepath.Join(svc.Dir(), resources.ServiceConfigurationName)
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", file, err)
	}
	if source, generated := generatedMarker(content); generated {
		if source != "" {
			cli.Warning("Skipping generated file %s; edit its source (%s) and regenerate instead", file, source)
		} else {
			cli.Warning("Skipping generated file %s carrying a DO NOT EDIT marker", file)
		}
		return nil, nil
	}
	from := svc.Agent.Version
	if _, err := manager.PinToLatestRelease(ctx, svc.Agent); err != nil {
		return nil, fmt.Errorf("cannot resolve latest agent version: %w", err)
	}
	if svc.Agent.Version == from {
		return nil, nil
	}
	updated, err := rewriteAgentVersion(content, svc.Agent.Version)
	if err != nil {
		return nil, fmt.Errorf("cannot update %s: %w", file, err)
	}
	if err := os.WriteFile(file, updated, 0o600); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", file, err)
	}
	return &services.AgentUpdate{Name: svc.Agent.Name, From: from, To: svc.Agent.Version}, nil
}

// generatedMarker reports the declared source and whether content carries a
// generated marker in its leading comment block.
func generatedMarker(content []byte) (source string, generated bool) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		if !generatedHeader.MatchString(line) {
			continue
		}
		if m := generatedSource.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
		return "", true
	}
	return "", false
}

// rewriteAgentVersion sets agent.version in place, leaving every other node —
// unmodeled keys, comments, and formatting — untouched.
func rewriteAgentVersion(content []byte, version string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("cannot parse service configuration: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("empty service configuration")
	}
	agent := mappingValue(doc.Content[0], "agent")
	if agent == nil {
		return nil, fmt.Errorf("service configuration has no agent block")
	}
	versionNode := mappingValue(agent, "version")
	if versionNode == nil {
		return nil, fmt.Errorf("agent block has no version")
	}
	versionNode.Value = version
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("cannot serialize service configuration: %w", err)
	}
	return out, nil
}

// mappingValue returns the value node for key in a YAML mapping, or nil.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
