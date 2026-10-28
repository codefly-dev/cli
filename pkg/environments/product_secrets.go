package environments

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/configurations"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// ProductSecretPolicy is the part of an environment's service-secrets a product
// decides rather than a cell: which of the product's own secret keys are minted
// at random (Generate), and which of them hold no value in that environment
// (MayBeEmpty). Both are statements about keys the product's services read,
// which no cell producer can know.
//
// The rest of service-secrets is a fact of the cell and stays with the
// coordinate contract: secret-store names the store the cell runs, defaults and
// a service's remote-keys, secret-store, template and refresh-interval say where
// and how that store files each value.
type ProductSecretPolicy struct {
	Generate   []EnvironmentSecretGenerator `yaml:"generate,omitempty"`
	MayBeEmpty []string                     `yaml:"may-be-empty,omitempty"`
}

// ProductSecretPolicyKeys are the service-secrets keys ProductSecretPolicy
// owns, as the workspace spells them.
var ProductSecretPolicyKeys = []string{"generate", "may-be-empty"}

// Empty reports whether the policy declares nothing.
func (policy *ProductSecretPolicy) Empty() bool {
	return policy == nil || (len(policy.Generate) == 0 && len(policy.MayBeEmpty) == 0)
}

// Validate checks the policy's structure, by the rules a workspace's
// service-secrets block is held to. A nil receiver declares nothing.
func (policy *ProductSecretPolicy) Validate() error {
	if policy == nil {
		return nil
	}
	for index := range policy.Generate {
		if err := policy.Generate[index].validate(index); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, key := range policy.MayBeEmpty {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("service-secrets may-be-empty: key names cannot be empty")
		}
		if seen[key] {
			return fmt.Errorf("service-secrets may-be-empty names %q twice", key)
		}
		seen[key] = true
	}
	return nil
}

// ValidateSecretPolicy holds a policy to the secret configurations env's
// profile declares, as the render reads them: a generator names a configuration
// group a composed service (or, for scope workspace, the workspace) declares as
// secret, and keys that group declares; a may-be-empty entry names a secret key
// some declared group produces. Anything else is refused by name: it would
// generate, or excuse, a value no rendered service reads.
// The resolver is a parameter rather than something this package builds: a
// module artifact's bytes are acquired from a composed module under the
// composition's own provenance rules, which live above pkg/environments. A
// caller that omits it reads a composition with a bound artifact as a refusal
// from Core — never as an absent value.
func ValidateSecretPolicy(ctx context.Context, workspace *resources.Workspace, env *Environment, policy *ProductSecretPolicy, resolvers ...configurations.ModuleArtifactResolver) error {
	if policy.Empty() {
		return nil
	}
	services, err := workspace.LoadServices(ctx)
	if err != nil {
		return err
	}
	composed := map[string]bool{}
	for _, service := range services {
		identity, identityErr := service.Identity()
		if identityErr != nil {
			return identityErr
		}
		composed[identity.Unique()] = true
	}
	reader, err := configurations.NewConfigurationLocalReader(ctx, workspace)
	for _, resolve := range resolvers {
		reader = reader.WithModuleArtifactResolver(resolve)
	}
	if err != nil {
		return err
	}
	if err := reader.Load(ctx, env.Runtime()); err != nil {
		return fmt.Errorf("read the configurations of environment %s to check its service-secrets against them: %w", env.Name, err)
	}
	declared := declaredSecretKeys(reader.Configurations())

	for index := range policy.Generate {
		generator := &policy.Generate[index]
		label := fmt.Sprintf("service-secrets generate[%d]", index)
		if generator.Scope == SecretGeneratorScopeWorkspace {
			if err := declared.cover(label, resources.ConfigurationWorkspace, "the workspace", generator); err != nil {
				return err
			}
			continue
		}
		if len(generator.Services) == 0 {
			if !declared.anyServiceDeclares(generator) {
				return fmt.Errorf("%s: no composed service declares secret configuration %q with keys %s in environment %s",
					label, generator.Configuration, strings.Join(generator.Keys, ", "), env.Name)
			}
			continue
		}
		for _, unique := range generator.Services {
			if !composed[unique] {
				return fmt.Errorf("%s names service %q, which the workspace does not compose", label, unique)
			}
			if err := declared.cover(label, unique, "service "+unique, generator); err != nil {
				return err
			}
		}
	}
	for _, key := range policy.MayBeEmpty {
		if !declared.stored[key] {
			return fmt.Errorf("service-secrets may-be-empty names %q, which no secret configuration of environment %s declares", key, env.Name)
		}
	}
	return nil
}

// secretKeys is every secret key a profile declares: per origin (a service
// unique, or resources.ConfigurationWorkspace) the keys of each group by group
// name, both in the spelling core derives keys with, and the stored keys they
// become.
type secretKeys struct {
	groups map[string]map[string]map[string]bool
	stored map[string]bool
}

func declaredSecretKeys(confs []*basev0.Configuration) *secretKeys {
	declared := &secretKeys{groups: map[string]map[string]map[string]bool{}, stored: map[string]bool{}}
	for _, conf := range confs {
		for _, info := range conf.Infos {
			for _, value := range info.ConfigurationValues {
				if !value.Secret {
					continue
				}
				if declared.groups[conf.Origin] == nil {
					declared.groups[conf.Origin] = map[string]map[string]bool{}
				}
				group := resources.NameToKey(info.Name)
				if declared.groups[conf.Origin][group] == nil {
					declared.groups[conf.Origin][group] = map[string]bool{}
				}
				declared.groups[conf.Origin][group][resources.NameToKey(value.Key)] = true
				if conf.Origin == resources.ConfigurationWorkspace {
					declared.stored[resources.WorkspaceSecretConfigurationPrefix+"__"+group+"__"+resources.NameToKey(value.Key)] = true
				} else {
					declared.stored[resources.ServiceSecretConfigurationKeyFromUnique(conf.Origin, info.Name, value.Key)] = true
				}
			}
		}
	}
	return declared
}

// cover refuses a generator whose configuration or keys origin does not declare.
func (declared *secretKeys) cover(label, origin, subject string, generator *EnvironmentSecretGenerator) error {
	keys, ok := declared.groups[origin][resources.NameToKey(generator.Configuration)]
	if !ok {
		return fmt.Errorf("%s: %s declares no secret configuration %q", label, subject, generator.Configuration)
	}
	for _, key := range generator.Keys {
		if !keys[resources.NameToKey(key)] {
			return fmt.Errorf("%s: secret configuration %q of %s declares no key %q", label, generator.Configuration, subject, key)
		}
	}
	return nil
}

// anyServiceDeclares reports whether some service declares the generator's
// configuration with every one of its keys.
func (declared *secretKeys) anyServiceDeclares(generator *EnvironmentSecretGenerator) bool {
	for origin := range declared.groups {
		if origin != resources.ConfigurationWorkspace && declared.cover("", origin, "", generator) == nil {
			return true
		}
	}
	return false
}
