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
	"github.com/codefly-dev/core/shared"
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

// agentUpdate records a resolved agent version bump for reporting.
type agentUpdate struct {
	Name string
	From string
	To   string
}

// updateServiceAgent bumps a service's agent to its latest compatible release
// with a surgical, text-preserving edit of the single agent.version token. It
// never reserializes resources.Service, so unmodeled keys, comments, and
// formatting survive byte-for-byte. It refuses to touch files carrying a
// "Code generated ... DO NOT EDIT" marker, pointing at the source to edit
// instead. Returns nil when nothing changed.
func updateServiceAgent(ctx context.Context, svc *resources.Service) (*agentUpdate, error) {
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
	// Atomic write (temp + fsync + rename) so a crash mid-write cannot leave a
	// truncated service.codefly.yaml — the very data integrity this command
	// exists to protect.
	if err := shared.WriteFileAtomic(ctx, file, updated, 0o600); err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", file, err)
	}
	return &agentUpdate{Name: svc.Agent.Name, From: from, To: svc.Agent.Version}, nil
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

// rewriteAgentVersion replaces only the agent.version scalar, editing the one
// source line it lives on and leaving every other byte — indentation, comments,
// unmodeled keys, quoting — exactly as written. The document is parsed only to
// locate the version node; it is never re-serialized (which would reflow the
// whole file to yaml.v3's canonical style).
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
	old := versionNode.Value
	if old == version {
		return content, nil
	}

	lines := bytes.Split(content, []byte("\n"))
	idx := versionNode.Line - 1
	if idx < 0 || idx >= len(lines) {
		return nil, fmt.Errorf("agent version node points outside the file")
	}
	line := lines[idx]
	// Column is 1-based and marks where the scalar token starts; search from
	// there so a quoted value ("0.0.22") and any coincidental earlier match are
	// both handled correctly.
	col := versionNode.Column - 1
	if col < 0 || col > len(line) {
		return nil, fmt.Errorf("agent version node points outside its line")
	}
	rel := bytes.Index(line[col:], []byte(old))
	if rel < 0 {
		return nil, fmt.Errorf("agent version value %q not found on its line", old)
	}
	at := col + rel
	var edited []byte
	edited = append(edited, line[:at]...)
	edited = append(edited, version...)
	edited = append(edited, line[at+len(old):]...)
	lines[idx] = edited
	return bytes.Join(lines, []byte("\n")), nil
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
