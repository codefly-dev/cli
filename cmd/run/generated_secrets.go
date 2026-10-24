package run

import (
	"context"
	"fmt"
	"strings"

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
	profile, err := env.ConfigurationProfileName()
	if err != nil {
		return nil, err
	}
	var written []string
	for index, generator := range secrets.Generate {
		scopes, err := generatorScopes(ctx, workspace, generator)
		if err != nil {
			return nil, fmt.Errorf("service-secrets generate[%d]: %w", index, err)
		}
		format, size := generator.Encoding()
		for _, scope := range scopes {
			target := config.Target{ScopeDir: scope, Profile: profile, Name: generator.Configuration, Secret: true}
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

// generatorScopes is the directory each of the generator's configurations
// lives under: the workspace's, or each named service's.
func generatorScopes(ctx context.Context, workspace *resources.Workspace, generator environments.EnvironmentSecretGenerator) ([]string, error) {
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
