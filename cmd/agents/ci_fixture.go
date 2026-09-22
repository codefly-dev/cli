package agents

import (
	"context"
	"fmt"
	"os/exec"
	"sort"

	"github.com/blang/semver"
	"github.com/codefly-dev/core/resources"
)

// The fixture declares its dependencies. Install their exact releases without
// replacing the locally built candidate or inheriting an operator's cache.
func installFixtureDependencies(ctx context.Context, executable, workspaceDir, home string, manifest *agentYAML) error {
	dependencies, err := fixtureDependencies(ctx, workspaceDir, manifest)
	if err != nil {
		return err
	}
	for _, agent := range dependencies {
		command := fixtureDependencyCommand(ctx, executable, workspaceDir, home, agent)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("install conformance dependency %s: %w\n%s", agent.Identifier(), err, boundedAgentCIOutput(output))
		}
	}
	return nil
}

func fixtureDependencyCommand(ctx context.Context, executable, workspaceDir, home string, agent *resources.Agent) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, "--timestamps=false", "agent", "install", agent.Identifier(), "--kind", string(agent.Kind)) //nolint:gosec // G204: re-exec this CLI with validated identity as argv, never a shell.
	command.Dir = workspaceDir
	command.Env = agentCIChildEnvironment(home, "CI=1", "CODEFLY_COLOR=never")
	return command
}

func fixtureDependencies(ctx context.Context, workspaceDir string, manifest *agentYAML) ([]*resources.Agent, error) {
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("load conformance fixture: %w", err)
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("load conformance fixture services: %w", err)
	}
	seen := make(map[string]bool)
	var dependencies []*resources.Agent
	foundCandidate := false
	for _, service := range services {
		agent := service.Agent
		if agent == nil {
			return nil, fmt.Errorf("conformance service %s has no agent", service.Name)
		}
		if agent.Kind == resources.ServiceAgent && agent.Publisher == manifest.Publisher && agent.Name == manifest.Name {
			if agent.Version != latestAgentVersion && agent.Version != manifest.Version {
				return nil, fmt.Errorf("conformance candidate %s must select latest or %s", agent.Identifier(), manifest.Version)
			}
			foundCandidate = true
			continue
		}
		if _, err := semver.Parse(agent.Version); err != nil {
			return nil, fmt.Errorf("conformance dependency %s requires an exact canonical version: %w", agent.Identifier(), err)
		}
		if !safeAgentComponent(agent.Publisher) || !safeAgentComponent(agent.Name) {
			return nil, fmt.Errorf("invalid conformance dependency identity %s", agent.Identifier())
		}
		key := string(agent.Kind) + ":" + agent.Identifier()
		if !seen[key] {
			seen[key] = true
			dependencies = append(dependencies, agent)
		}
	}
	if !foundCandidate {
		return nil, fmt.Errorf("conformance workspace does not load the candidate %s/%s", manifest.Publisher, manifest.Name)
	}
	sort.Slice(dependencies, func(i, j int) bool {
		return string(dependencies[i].Kind)+dependencies[i].Identifier() < string(dependencies[j].Kind)+dependencies[j].Identifier()
	})
	return dependencies, nil
}
