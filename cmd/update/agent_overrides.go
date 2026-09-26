package update

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"

	"github.com/codefly-dev/cli/pkg/composition"
)

// parseAgentOverrideFlags reads repeated --agent-override
// <publisher>/<name>=<version> values through core's own agent-overrides
// parser, so the command accepts exactly what a run accepts from the file: an
// agent identity, and an exact semantic version (prereleases included).
func parseAgentOverrideFlags(values []string) ([]resources.AgentOverride, error) {
	raw := make(map[string]string, len(values))
	for _, value := range values {
		key, version, found := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" || strings.TrimSpace(version) == "" {
			return nil, fmt.Errorf("--agent-override %q: spell it <publisher>/<name>=<version>, e.g. codefly.dev/go-grpc=0.1.47-dev.abc123def456", value)
		}
		if _, dup := raw[key]; dup {
			return nil, fmt.Errorf("--agent-override names %s twice", key)
		}
		raw[key] = version
	}
	return resources.ParseAgentOverrides(raw)
}

// pinAgentOverrides writes overrides into the workspace's committed
// agent-overrides block, keeping every entry it does not name. Before anything
// is written it proves each new entry holds up: the block as a whole must
// resolve (every key moves at least one composed service — the check every run
// applies), and each overridden agent must be published at that version and
// speak a compatible protocol. The file is written through the workspace's own
// saver, which carries top-level keys core does not own (agent-overrides
// among them) across the round trip.
func pinAgentOverrides(ctx context.Context, workspace *resources.Workspace, overrides []resources.AgentOverride) ([]composition.AgentOverrideUse, error) {
	existing, err := workspace.AgentOverrides()
	if err != nil {
		return nil, err
	}
	merged := map[string]string{}
	for _, override := range existing {
		merged[override.Key()] = override.Version
	}
	for _, override := range overrides {
		merged[override.Key()] = override.Version
	}
	all, err := resources.ParseAgentOverrides(merged)
	if err != nil {
		return nil, err
	}

	previous, had := workspace.Extensions[resources.AgentOverridesKey]
	if workspace.Extensions == nil {
		workspace.Extensions = map[string]resources.YAMLValue{}
	}
	workspace.Extensions[resources.AgentOverridesKey] = agentOverridesValue(all)
	restore := func() {
		if had {
			workspace.Extensions[resources.AgentOverridesKey] = previous
		} else {
			delete(workspace.Extensions, resources.AgentOverridesKey)
		}
	}

	uses, err := composition.ResolveAgentOverrides(ctx, workspace)
	if err != nil {
		restore()
		return nil, fmt.Errorf("%w (a service this workspace authors itself is pinned with `codefly update service <service> --agent-version <version>`)", err)
	}
	for _, override := range overrides {
		candidate, lookupErr := overriddenAgent(ctx, workspace, override)
		if lookupErr != nil {
			restore()
			return nil, lookupErr
		}
		if err = inspectAgent(ctx, candidate); err != nil {
			restore()
			return nil, fmt.Errorf("cannot override agent %s to %s: %w", override.Key(), override.Version, admissionError(candidate, err))
		}
	}
	if err = workspace.Save(ctx); err != nil {
		restore()
		return nil, fmt.Errorf("cannot save %s: %w", resources.WorkspaceConfigurationName, err)
	}
	return uses, nil
}

// overriddenAgent is the agent a composed service runs once override applies:
// its kind comes from a service that runs on it, since an override names only
// publisher and name.
func overriddenAgent(ctx context.Context, workspace *resources.Workspace, override resources.AgentOverride) (*resources.Agent, error) {
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot load workspace services: %w", err)
	}
	for _, service := range services {
		if override.Matches(service.Agent) {
			candidate := *service.Agent
			candidate.Version = override.Version
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no service in the workspace runs on agent %s", override.Key())
}

// agentOverridesValue renders overrides as the agent-overrides mapping, sorted
// by key (ParseAgentOverrides sorts), with every version a string scalar so a
// version is never re-read as a number.
func agentOverridesValue(overrides []resources.AgentOverride) resources.YAMLValue {
	node := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, override := range overrides {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: override.Key()},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: override.Version},
		)
	}
	return resources.YAMLValue{Node: node}
}
