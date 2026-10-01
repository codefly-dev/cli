package prerelease

import (
	"fmt"
	"path"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// versionKey is the field name a codefly document spells a version with.
const versionKey = "version"

// scanCodeflyConfig finds every prerelease version a codefly document declares.
//
// It walks for the `version:` key at any depth rather than reading a known
// schema. The pins that broke live at different paths in different documents —
// agent.version in a service, modules[].version in a workspace, the document's
// own top-level version in an agent or module manifest — and a schema list would
// have to be extended every time a versioned field is added, silently missing
// the new one until somebody notices. The key is the invariant.
//
// A malformed document is not this gate's failure to report: the command that
// loads it fails on it properly, with the schema context this scan does not
// have. Reporting it here would turn an unrelated YAML error into "prerelease
// check failed".
func scanCodeflyConfig(file string, content []byte, options Options) ([]Finding, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, nil
	}
	var findings []Finding
	isWorkspace := path.Base(file) == resources.WorkspaceConfigurationName
	for _, root := range document.Content {
		walkConfig(file, root, "", isWorkspace, options, &findings)
	}
	return findings, nil
}

func walkConfig(file string, node *yaml.Node, keyPath string, isWorkspace bool, options Options, findings *[]Finding) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			walkConfig(file, child, keyPath, isWorkspace, options, findings)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			child := key.Value
			if keyPath != "" {
				child = keyPath + "." + key.Value
			}
			// The one carrier whose versions are not under a `version:` key: the
			// workspace's top-level agent-overrides block maps an agent identity
			// straight to the version every composed service on it runs.
			if isWorkspace && keyPath == "" && key.Value == resources.AgentOverridesKey && value.Kind == yaml.MappingNode {
				collectAgentOverrides(file, value, options, findings)
				continue
			}
			if key.Value == versionKey && value.Kind == yaml.ScalarNode {
				if finding, ok := configFinding(file, child, value, options); ok {
					*findings = append(*findings, finding)
				}
				continue
			}
			walkConfig(file, value, child, isWorkspace, options, findings)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			walkConfig(file, child, fmt.Sprintf("%s[%d]", keyPath, i), isWorkspace, options, findings)
		}
	}
}

func configFinding(file, keyPath string, value *yaml.Node, _ Options) (Finding, bool) {
	kind, prerelease := Classify(value.Value)
	if !prerelease {
		return Finding{}, false
	}
	return Finding{
		File:     file,
		Line:     value.Line,
		Key:      keyPath,
		Version:  value.Value,
		Kind:     kind,
		Carrier:  CarrierConfig,
		Blocking: true,
		Why:      "a prerelease in committed configuration reaches the default branch, and any tag cut from it",
		Remedy: []string{
			"pin a released version — `codefly agent versions <publisher>/<name>` lists what is published, `codefly update workspace` moves the pin",
			fmt.Sprintf("or, for a dev agent build this composition has to run, move it to `%s` in %s with a comment naming the issue it stands in for",
				resources.AgentOverridesKey, resources.WorkspaceConfigurationName),
			"a dev loop that does not have to be committed belongs in codefly.local.yaml, which is gitignored and outside this gate",
		},
	}, true
}

// collectAgentOverrides reads the workspace's agent-overrides entries. A labelled
// entry is permitted on the default branch and refused in release scope; an
// unlabelled one is refused either way, because the label is what tells the next
// reader what has to ship before the override can go.
func collectAgentOverrides(file string, block *yaml.Node, options Options, findings *[]Finding) {
	for i := 0; i+1 < len(block.Content); i += 2 {
		key, value := block.Content[i], block.Content[i+1]
		if value.Kind != yaml.ScalarNode {
			continue
		}
		kind, prerelease := Classify(value.Value)
		if !prerelease {
			continue
		}
		label := overrideLabel(key, value)
		finding := Finding{
			File:     file,
			Line:     value.Line,
			Key:      resources.AgentOverridesKey + "." + key.Value,
			Version:  value.Value,
			Kind:     kind,
			Carrier:  CarrierAgentOverride,
			Label:    label,
			Labelled: Labelled(label),
		}
		switch {
		case options.Release:
			// Labelling is not a way out of release scope, so it is not offered as
			// one: the only thing that clears this is the released agent.
			finding.Blocking = true
			finding.Why = "release scope: a tag cannot be cut over a dev override, however well labelled — this is the step docs/release.md asks for and nothing enforced"
			finding.Remedy = []string{
				"release the agent, move this entry to the released version, then delete the entry",
				"a module released after the agent fix pins the released agent itself; if one still does not, replace the dev version with the released one and say why in the commit",
			}
		case !finding.Labelled:
			finding.Blocking = true
			finding.Why = "an agent-overrides prerelease is permitted only with a label: a comment naming the issue it stands in for"
			finding.Remedy = []string{
				"label the entry: a comment above it naming the issue it stands in for and the condition for removal (`codefly doctor workspace` lists every override in force)",
				"or drop it and pin the released agent in the module that composes the service",
			}
		default:
			finding.Why = "labelled dev override, permitted on the default branch; it must be gone before a release"
			finding.Remedy = []string{
				"release the agent and drop the entry before anything here is tagged — `codefly ci prerelease --release` is the gate that will insist",
			}
		}
		*findings = append(*findings, finding)
	}
}

// overrideLabel is the comment attached to an agent-overrides entry. yaml.v3 puts
// a block comment above an entry on the key node's head comment and a trailing
// one on a line comment; both spellings are read, and the value's own line
// comment too, so a label is found wherever it was reasonably written.
func overrideLabel(key, value *yaml.Node) string {
	parts := make([]string, 0, 4)
	for _, comment := range []string{key.HeadComment, key.LineComment, value.LineComment, key.FootComment} {
		if strings.TrimSpace(comment) != "" {
			parts = append(parts, strings.TrimSpace(comment))
		}
	}
	return strings.Join(parts, "\n")
}
