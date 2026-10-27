package run

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"

	"github.com/codefly-dev/cli/cmd/config"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/secretgen"
)

// provisionGeneratedSecrets writes, before a local run loads its
// configuration, every key the environment's service-secrets generators
// declare that the workspace's secret configuration does not hold yet: what
// `codefly deploy secrets` does for a cell's store, minted by the same
// generator, into the files `codefly config generate` writes. A key that holds
// a value is never rotated. An environment whose service-secrets names a store
// keeps its secrets there, so nothing is written for it locally.
func provisionGeneratedSecrets(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) ([]string, error) {
	secrets := env.ServiceSecrets
	if secrets == nil || secrets.Stored() || len(secrets.Generate) == 0 {
		return nil, nil
	}
	configProfile, err := env.ConfigurationProfileName()
	if err != nil {
		return nil, err
	}
	resource, err := env.Resource()
	if err != nil {
		return nil, err
	}
	profiles, err := resource.ConfigurationProfileNames()
	if err != nil {
		return nil, err
	}
	var written []string
	for index := range secrets.Generate {
		generator := &secrets.Generate[index]
		if generator.Scope == environments.SecretGeneratorScopeWorkspace {
			if err := refuseShadowingAnInheritedGroup(ctx, workspace, profiles, generator); err != nil {
				return nil, fmt.Errorf("service-secrets generate[%d]: %w", index, err)
			}
		}
		scopes, err := generatorScopes(ctx, workspace, generator)
		if err != nil {
			return nil, fmt.Errorf("service-secrets generate[%d]: %w", index, err)
		}
		format, size := generator.Encoding()
		for _, scope := range scopes {
			target := config.Target{ScopeDir: scope, Profile: configProfile, Name: generator.Configuration, Secret: true}
			keys, err := generateMissing(target, generator.Keys, format, size)
			if err != nil {
				return nil, fmt.Errorf("service-secrets generate[%d] %s: %w", index, generator.Configuration, err)
			}
			for _, key := range keys {
				written = append(written, generator.Configuration+"/"+key)
			}
		}
	}
	return written, nil
}

// refuseShadowingAnInheritedGroup refuses a workspace-scope generator whose
// group this workspace inherits from a workspace it composes and does not
// author itself. The generated file is an own group of that name, and an own
// group replaces the inherited one whole (core's workspace composition) — so
// writing it, or reading one an earlier run wrote, silently drops every key
// the composed workspace supplies: a workspace importing platform-core lost
// the hop's MODULES that way, and with it every hop route. Declaring the group
// in `configuration-extensions` makes the generated keys extend the inherited
// group instead, which is then the composition's own answer. A group the
// workspace authors (an own file holding a key the generator does not write)
// is its author's to replace.
func refuseShadowingAnInheritedGroup(ctx context.Context, workspace *resources.Workspace, profiles []string, generator *environments.EnvironmentSecretGenerator) error {
	name := generator.Configuration
	if _, extends := workspace.ConfigurationExtensions[name]; extends {
		return nil
	}
	inheritedFrom, err := composedWorkspaceDeclaring(ctx, workspace, profiles, name)
	if err != nil || inheritedFrom == "" {
		return err
	}
	own, err := configurations.LoadProfileConfigurations(ctx, workspace.Dir(), "configurations", profiles)
	if err != nil {
		return err
	}
	for _, info := range own.Infos {
		if info.GetName() != name {
			continue
		}
		for _, value := range info.GetConfigurationValues() {
			if !slices.ContainsFunc(generator.Keys, func(key string) bool { return resources.Match(strings.TrimSpace(key), value.GetKey()) }) {
				return nil
			}
		}
	}
	return fmt.Errorf("workspace %s inherits configuration %q from workspace %s and generates %s into it; a generated file is an own %q group, which replaces the inherited one whole and drops every key %s supplies: declare it under configuration-extensions in %s (`configuration-extensions: {%s: {}}`) so the generated keys extend the inherited group",
		workspace.Name, name, inheritedFrom, strings.Join(generator.Keys, ", "), name, inheritedFrom, resources.WorkspaceConfigurationName, name)
}

// composedWorkspaceDeclaring names the composed workspace, at any depth, whose
// own configurations declare the group, or "" when none does.
func composedWorkspaceDeclaring(ctx context.Context, workspace *resources.Workspace, profiles []string, name string) (string, error) {
	for _, child := range workspace.ComposedWorkspaces() {
		provided, err := configurations.LoadProfileConfigurations(ctx, child.Dir(), "configurations", profiles)
		if err != nil {
			return "", err
		}
		if slices.ContainsFunc(provided.Infos, func(info *basev0.ConfigurationInformation) bool { return info.GetName() == name }) {
			return child.Name, nil
		}
		if found, err := composedWorkspaceDeclaring(ctx, child, profiles, name); err != nil || found != "" {
			return found, err
		}
	}
	return "", nil
}

// generatorScopes is the directory each of the generator's configurations
// lives under: the workspace's, or each named service's.
func generatorScopes(ctx context.Context, workspace *resources.Workspace, generator *environments.EnvironmentSecretGenerator) ([]string, error) {
	if generator.Scope == environments.SecretGeneratorScopeWorkspace {
		return []string{workspace.Dir()}, nil
	}
	if len(generator.Services) == 0 {
		return nil, fmt.Errorf("a local run generates a service configuration only for the services the generator names")
	}
	scopes := make([]string, 0, len(generator.Services))
	for _, unique := range generator.Services {
		moduleName, serviceName := resources.SplitUnique(unique)
		module, err := workspace.LoadModuleFromName(ctx, moduleName)
		if err != nil {
			return nil, err
		}
		service, err := module.LoadServiceFromName(ctx, serviceName)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, service.Dir())
	}
	return scopes, nil
}

// generateMissing sets every key the target does not hold to a fresh value and
// returns the keys it set.
func generateMissing(target config.Target, keys []string, format secretgen.Format, size int) ([]string, error) {
	doc, err := config.Load(target)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if doc.Has(key) {
			continue
		}
		value, err := secretgen.Generate(format, size)
		if err != nil {
			return nil, err
		}
		if err := doc.Set(key, value); err != nil {
			return nil, err
		}
		written = append(written, key)
	}
	if len(written) == 0 {
		return nil, nil
	}
	return written, config.Write(target, doc)
}
