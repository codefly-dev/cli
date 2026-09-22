package environments

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
)

// FromRuntime admits the deployment declarations preserved by the Core loader.
// Decode a fresh value so concurrent invocations cannot share mutable maps.
func FromRuntime(runtime *resources.Environment) (*Environment, error) {
	if runtime == nil {
		return nil, fmt.Errorf("empty environment entry")
	}
	data, err := yaml.Marshal(runtime)
	if err != nil {
		return nil, err
	}
	var env Environment
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&env); err != nil {
		return nil, fmt.Errorf("environment %q: %w", runtime.Name, err)
	}
	if err := env.Validate(); err != nil {
		return nil, fmt.Errorf("environment %q: %w", runtime.Name, err)
	}
	return &env, nil
}

// Resource preserves the CLI document when a workspace is passed to Core.
// Use Runtime when invoking agents or loading runtime configuration.
func (env *Environment) Resource() (*resources.Environment, error) {
	data, err := yaml.Marshal(env)
	if err != nil {
		return nil, err
	}
	var resource resources.Environment
	if err := yaml.Unmarshal(data, &resource); err != nil {
		return nil, err
	}
	return &resource, nil
}

func FromWorkspace(workspace *resources.Workspace) ([]*Environment, error) {
	environments := make([]*Environment, 0, len(workspace.Environments))
	for _, resource := range workspace.Environments {
		env, err := FromRuntime(resource)
		if err != nil {
			return nil, err
		}
		environments = append(environments, env)
	}
	return environments, nil
}

func Select(workspace *resources.Workspace, name string) (*Environment, error) {
	if name == "local" {
		declared := false
		for _, env := range workspace.Environments {
			if env != nil && env.Name == name {
				declared = true
				break
			}
		}
		if !declared {
			return LocalEnvironment(), nil
		}
	}
	resource := workspace.FindEnvironment(name)
	if resource == nil {
		return nil, fmt.Errorf("workspace %q does not declare environment %q in %s", workspace.Name, name, resources.WorkspaceConfigurationName)
	}
	return FromRuntime(resource)
}

// WorkspaceGitops reads the CLI's workspace-wide delivery defaults.
func WorkspaceGitops(workspace *resources.Workspace) (*EnvironmentGitops, error) {
	node, exists := workspace.Extensions["gitops"]
	if !exists {
		return nil, nil
	}
	data, err := yaml.Marshal(&node)
	if err != nil {
		return nil, err
	}
	var gitops EnvironmentGitops
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&gitops); err != nil {
		return nil, fmt.Errorf("workspace gitops: %w", err)
	}
	return &gitops, nil
}

func (env *Environment) Validate() error {
	if _, err := env.Proto(); err != nil {
		return err
	}
	if err := env.ServiceSecrets.Validate(); err != nil {
		return err
	}
	if err := env.ServiceConfig.Validate(); err != nil {
		return err
	}
	if err := env.ServiceIdentity.Validate(); err != nil {
		return err
	}
	if err := env.validateServiceKeyCollisions(); err != nil {
		return err
	}
	for name, managed := range env.ManagedServices {
		if err := managed.Identity.validate(fmt.Sprintf("managed service %q", name)); err != nil {
			return err
		}
	}
	return env.ResourceQuota.Validate()
}

func validateResourcePathComponent(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%s name cannot be empty", kind)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("%s name %q must be a single path component", kind, name)
	}
	return nil
}

// ValidateWorkspace cross-checks environment declarations that name services
// against the workspace's actual service graph. Service-secret overrides,
// service-config values and service-identity entries are keyed by service name;
// a key that matches no loaded service is otherwise a silent no-op at projection
// time — the service keeps the default "<service>/<key>" remote paths, receives
// none of the declared values, and falls back to the environment-wide identity
// or none at all, so a typo'd name resolves the wrong secret, drops a value the
// workload needs, or authenticates as the wrong principal, with nothing catching
// it earlier.
// Loading the graph is why this is a pass separate from postLoad, mirroring
// ValidateServiceDependencies.
func ValidateWorkspace(ctx context.Context, workspace *resources.Workspace) error {
	environments, err := FromWorkspace(workspace)
	if err != nil {
		return err
	}
	w := wool.Get(ctx).In("Workspace::ValidateEnvironments", wool.NameField(workspace.Name))
	needsGraph := false
	for _, env := range environments {
		if env != nil && len(env.serviceScopedNames()) > 0 {
			needsGraph = true
			break
		}
	}
	if !needsGraph {
		return nil
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return w.Wrap(err)
	}
	known := make(map[string]int, len(services))
	for _, svc := range services {
		known[svc.Name]++
	}
	for _, env := range environments {
		if env == nil {
			continue
		}
		for block, names := range env.serviceScopedNames() {
			for _, name := range names {
				if _, ok := known[name]; !ok {
					return w.Wrap(fmt.Errorf("environment %q %s references unknown service %q", env.Name, block, name))
				}
				if known[name] > 1 {
					return fmt.Errorf("environment %q %s references ambiguous service %q", env.Name, block, name)
				}
			}
		}
	}
	return nil
}
