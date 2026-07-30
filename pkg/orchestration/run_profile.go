package orchestration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

type RunProfile struct {
	ExcludeDependencies            []string `yaml:"exclude-dependencies,omitempty"`
	ExcludeWorkspaceConfigurations []string `yaml:"exclude-workspace-configurations,omitempty"`
}

type ResolvedRunProfile struct {
	ExcludedDependencies            []string
	ExcludedWorkspaceConfigurations []string
}

type workspaceRunProfiles struct {
	RunProfiles map[string]RunProfile `yaml:"run-profiles,omitempty"`
}

func ResolveRunProfile(ctx context.Context, workspace *resources.Workspace, name string, explicitDependencies []string) (ResolvedRunProfile, error) {
	var selected RunProfile
	name = strings.TrimSpace(name)
	if name != "" {
		profiles, err := loadRunProfiles(workspace)
		if err != nil {
			return ResolvedRunProfile{}, err
		}
		var ok bool
		selected, ok = profiles[name]
		if !ok {
			return ResolvedRunProfile{}, fmt.Errorf("unknown run profile %q", name)
		}
	}

	serviceInputs := make([]string, 0, len(selected.ExcludeDependencies)+len(explicitDependencies))
	serviceInputs = append(serviceInputs, selected.ExcludeDependencies...)
	for _, input := range explicitDependencies {
		serviceInputs = append(serviceInputs, strings.Split(input, ",")...)
	}
	excludedServices, err := resolveProfileServices(ctx, workspace, serviceInputs)
	if err != nil {
		return ResolvedRunProfile{}, err
	}
	excludedConfigurations, err := resolveProfileConfigurations(ctx, workspace, name, selected.ExcludeWorkspaceConfigurations)
	if err != nil {
		return ResolvedRunProfile{}, err
	}
	return ResolvedRunProfile{
		ExcludedDependencies:            excludedServices,
		ExcludedWorkspaceConfigurations: excludedConfigurations,
	}, nil
}

func loadRunProfiles(workspace *resources.Workspace) (map[string]RunProfile, error) {
	path := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read run profiles from %s: %w", path, err)
	}
	var contract workspaceRunProfiles
	if err := yaml.Unmarshal(content, &contract); err != nil {
		return nil, fmt.Errorf("decode run profiles from %s: %w", path, err)
	}
	return contract.RunProfiles, nil
}

func resolveProfileServices(ctx context.Context, workspace *resources.Workspace, inputs []string) ([]string, error) {
	seen := make(map[string]bool)
	for _, input := range inputs {
		reference := strings.TrimSpace(input)
		if reference == "" {
			continue
		}
		service, module, err := workspace.FindUniqueModuleServiceByName(ctx, reference)
		if err != nil {
			return nil, fmt.Errorf("resolve excluded dependency %q: %w", reference, err)
		}
		seen[module.Name+"/"+service.Name] = true
	}
	resolved := make([]string, 0, len(seen))
	for service := range seen {
		resolved = append(resolved, service)
	}
	sort.Strings(resolved)
	return resolved, nil
}

func resolveProfileConfigurations(ctx context.Context, workspace *resources.Workspace, profile string, inputs []string) ([]string, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("load services for run profile %q: %w", profile, err)
	}
	known := make(map[string]bool)
	for _, service := range services {
		for _, dependency := range service.WorkspaceConfigurationDependencies {
			known[dependency] = true
		}
	}
	seen := make(map[string]bool)
	for _, input := range inputs {
		reference := strings.TrimSpace(input)
		if !known[reference] {
			return nil, fmt.Errorf("run profile %q excludes unknown workspace configuration %q", profile, reference)
		}
		seen[reference] = true
	}
	resolved := make([]string, 0, len(seen))
	for configuration := range seen {
		resolved = append(resolved, configuration)
	}
	sort.Strings(resolved)
	return resolved, nil
}
