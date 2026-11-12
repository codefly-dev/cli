package orchestration

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
)

// --- What an environment tells a service's agent about its server ---
//
// An environment that replaces a service with one it owns (managed-services)
// or deploys it as its module declares it (in-cluster-services) says so to the
// service's own agent, on Deploy, as one configuration information of the
// service's own configuration (codefly-dev/cli#1346). Before this, the agent
// rendered its in-cluster server for every environment, and the render retained
// only its bootstrap Job — a Job that then ran against a server that was never
// deployed, with a login the managed instance did not have.
//
// The two informations are the agent's vocabulary (codefly-dev/service-postgres,
// managed.go): their keys are named so core's sensitive-key classifier reads
// none of them as a credential — `auth-mode` and `instance-connection-name`,
// the entry's own spellings, are refused by a restricted render — and the
// values are the entry's, verbatim. Nothing here interprets them.
const (
	// ManagedServiceInformation carries the managed-services entry.
	ManagedServiceInformation = "managed-service"
	// InClusterServiceInformation says the environment lists the service in
	// in-cluster-services. It carries no value: the statement is the fact.
	InClusterServiceInformation = "in-cluster-service"

	managedLoginModeKey  = "login-mode"
	managedInstanceKey   = "instance"
	managedOwnerKey      = "owner-principal"
	managedRuntimeKey    = "runtime-principals"
	managedAuthModeValue = "external-identity"
)

// managedCarriedKeys maps each carried key of a managed-services entry to the
// name the agent reads it under.
var managedCarriedKeys = map[string]string{
	"auth-mode":                managedLoginModeKey,
	"instance-connection-name": managedInstanceKey,
}

// storeServerAgents are the service agents that deploy a database server when
// an environment lets them: the owner's rule (2026-11-12) — "for postgres
// service in codefly, we should have 'managed' vs non-managed, so when render a
// managed, we ONLY render and build the migration so we are SURE we don't
// deploy PG in k8s" — is held here as well as by the agent, so that it does
// not depend on which agent version a module pins (an agent older than
// service-postgres v0.0.149 renders the server for an environment that says
// nothing). The reference is the agent's own (agent.codefly.yaml: publisher,
// kind, name); the version is whatever the module pins.
var storeServerAgents = []resources.Agent{
	{Kind: resources.ServiceAgent, Publisher: "codefly.dev", Name: "postgres"},
}

// deploysAStoreServer reports whether the agent is one of storeServerAgents,
// by its reference — publisher, kind and name — never by what the service or
// the agent happens to be called.
func deploysAStoreServer(agent *resources.Agent) bool {
	if agent == nil {
		return false
	}
	return slices.ContainsFunc(storeServerAgents, func(known resources.Agent) bool {
		return agent.Publisher == known.Publisher && agent.Kind == known.Kind && agent.Name == known.Name
	})
}

// withEnvironmentServiceDeclaration returns the service's configuration with
// the environment's declaration for it appended, or the configuration as it was
// when the environment declares nothing. The result is a copy.
//
// A service whose agent deploys a database server (storeServerAgents) is
// refused by name when the environment declares it in neither block: the CLI
// never lets an environment that forgot to say where the server lives hand the
// agent a Deploy it might answer with a server. The agent refuses the same
// request itself (codefly-dev/service-postgres, managed.go) — that is the
// second line of defence; this one holds for every agent version.
//
// A managed service authenticating as its own identity (auth-mode
// external-identity) is handed two more values the entry cannot carry because
// they are the environment's, not the module's: the owner principal — the
// service-identity declared for the managed service itself, which its bootstrap
// Job logs in as — and the runtime principals — the identities of the services
// of this render that depend on it at run time, which the Job grants access to.
// A managed service with no identity is refused by name: a Job with no identity
// cannot log in, and the refusal belongs here, where the environment is, not in
// the Job's logs.
func withEnvironmentServiceDeclaration(ctx context.Context, configuration *basev0.Configuration, identity *resources.ServiceIdentity, agent *resources.Agent, world *World) (*basev0.Configuration, error) {
	if world == nil || world.Env == nil || identity == nil {
		return configuration, nil
	}
	env := world.Env
	managed, declared := env.ManagedService(identity.Module, identity.Name)
	inCluster := env.InCluster(identity.Module, identity.Name)
	unique := identity.Unique()
	if !declared && !inCluster {
		if deploysAStoreServer(agent) {
			return nil, fmt.Errorf("environment %q declares %s in neither managed-services nor in-cluster-services, and its agent %s deploys a database server: declare the service under in-cluster-services (the server runs in the cluster, as the module declares it) or under managed-services (the environment owns the server, and the render carries the migration alone) on environment %q in workspace.codefly.yaml — a render never deploys a Postgres server by default", env.Name, unique, agent.Identifier(), env.Name)
		}
		return configuration, nil
	}
	if declared && inCluster {
		return nil, fmt.Errorf("environment %q declares %s both managed and in-cluster", env.Name, unique)
	}
	result := &basev0.Configuration{Origin: unique}
	if configuration != nil {
		result = proto.CloneOf(configuration)
	}
	if inCluster {
		result.Infos = append(result.Infos, &basev0.ConfigurationInformation{Name: InClusterServiceInformation})
		return result, nil
	}
	information := &basev0.ConfigurationInformation{Name: ManagedServiceInformation}
	carried := make([]string, 0, len(managedCarriedKeys))
	for key := range managedCarriedKeys {
		carried = append(carried, key)
	}
	sort.Strings(carried)
	values := map[string]string{}
	for _, key := range carried {
		value, present := managed.Carried[key]
		if !present {
			continue
		}
		var text string
		if err := value.Decode(&text); err != nil {
			return nil, fmt.Errorf("environment %q managed service %s: %s is not a string: %w", env.Name, unique, key, err)
		}
		values[managedCarriedKeys[key]] = text
		information.ConfigurationValues = append(information.ConfigurationValues, &basev0.ConfigurationValue{Key: managedCarriedKeys[key], Value: text})
	}
	if values[managedLoginModeKey] == managedAuthModeValue {
		owner := env.WorkloadIdentityFor(identity.Module, identity.Name)
		if owner == nil || strings.TrimSpace(owner.Principal) == "" {
			return nil, fmt.Errorf("environment %q manages %s as its own identity (auth-mode %s) but declares no service-identity for it: its bootstrap Job would have no principal to log in as", env.Name, unique, managedAuthModeValue)
		}
		principals, err := runtimePrincipals(ctx, world, identity, &managed)
		if err != nil {
			return nil, err
		}
		information.ConfigurationValues = append(information.ConfigurationValues,
			&basev0.ConfigurationValue{Key: managedOwnerKey, Value: owner.Principal},
			&basev0.ConfigurationValue{Key: managedRuntimeKey, Value: strings.Join(principals, ",")},
		)
	}
	result.Infos = append(result.Infos, information)
	return result, nil
}

// runtimePrincipals are the identities the services depending on a managed
// service at run time authenticate as, sorted and distinct. A dependent takes
// its own service-identity, or, absent one, the identity the managed service
// declares for its consumers — the resolution the render's identity projection
// applies to its pods (gitops.soleWorkloadIdentity) — and a dependent with
// neither is refused by name: it would reach the instance as nobody.
func runtimePrincipals(ctx context.Context, world *World, identity *resources.ServiceIdentity, managed *environments.EnvironmentManagedService) ([]string, error) {
	unique := identity.Unique()
	if world.Dependencies == nil {
		return nil, fmt.Errorf("managed service %s: the dependency graph is not loaded, so its runtime principals cannot be named", unique)
	}
	running, err := world.Dependencies.ForStage(resources.StageRun)
	if err != nil {
		return nil, fmt.Errorf("managed service %s: %w", unique, err)
	}
	dependents, err := running.DirectDependents(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("managed service %s: %w", unique, err)
	}
	var principals []string
	for _, dependent := range dependents {
		module, service, qualified := strings.Cut(dependent.Unique, "/")
		if !qualified {
			return nil, fmt.Errorf("managed service %s: dependent %q is not module-qualified", unique, dependent.Unique)
		}
		consumer := world.Env.WorkloadIdentityFor(module, service)
		if consumer == nil {
			consumer = managed.Identity
		}
		if consumer == nil || strings.TrimSpace(consumer.Principal) == "" {
			return nil, fmt.Errorf("environment %q: %s depends on managed service %s at run time but declares no identity to reach it as (service-identity, or the managed service's identity)", world.Env.Name, dependent.Unique, unique)
		}
		if !slices.Contains(principals, consumer.Principal) {
			principals = append(principals, consumer.Principal)
		}
	}
	sort.Strings(principals)
	return principals, nil
}
