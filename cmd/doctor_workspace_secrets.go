package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"golang.org/x/sync/errgroup"
)

// codeSecretConfigurationRequired reports a secret configuration key a
// service's builder agent requires for this environment that the environment
// will not deliver. The agent refuses the render over exactly this key — at
// Deploy, after the service's image is built and pushed — so this is that
// refusal, moved to before the first build.
const codeSecretConfigurationRequired = "secret_configuration_required"

// codeSecretRequirementUnknown reports a service whose agent could not be
// asked at all: it would not load, or it failed the probe. The render drives
// the same agent, so it would fail on the same thing.
const codeSecretRequirementUnknown = "secret_requirement_unknown"

// codeSecretRequirementUndeclared reports a service whose agent answered but
// does not implement the declaration, so what its render requires of the host
// is not established. It is a warning and not a failure because an agent
// predating the contract is the fleet's normal state while it rolls out, and
// refusing every such service would stop delivery everywhere rather than
// report what is unknown. It is never silent: the service and its agent
// version are named, and the readiness summary says how many services it
// could not judge, so no verdict claims more than it checked.
const codeSecretRequirementUndeclared = "secret_requirement_undeclared"

// secretRequirementProbes bounds how many builder agents are started at once.
// Each probe is one process start and one read-only RPC; the bound keeps a
// 44-service workspace from starting 44 processes together while still
// finishing in a fraction of the build it precedes.
const secretRequirementProbes = 8

// serviceSecretRequirementProbe asks one service's builder agent what a render
// of the environment requires of the host. It is a variable so a readiness run
// can be exercised without an agent process; nothing but a test replaces it,
// and what it replaces is the agent the render itself drives.
var serviceSecretRequirementProbe = serviceBuildCapabilities

// checkServiceSecretRequirements asks every service in scope's builder agent
// which secret configuration groups a render of this environment requires, and
// reports every key the environment will not deliver — all of them, in one
// pass, so an operator fixes the set rather than discovering it one render at a
// time.
//
// This is the one check in `doctor workspace` that starts agents, and it does
// so deliberately. The requirement is the agent's own: service-postgres names
// four postgres primitives its manifests reference through non-optional
// secretKeyRefs, and refuses a render that reaches it without them. Nothing a
// host reads statically carries that set — a service's manifest does not state
// it, and restating it in the workspace would be a copy of the agent's
// contract, free to drift from the code that refuses. So the agent is asked,
// over the read-only BuildCapabilities probe, which runs no build, reaches no
// cluster and reads only the loaded service's own settings. It runs last,
// after everything a static read can refuse has already been reported.
//
// Nothing here goes quiet. A service whose agent cannot be asked fails; one
// whose agent cannot answer is named as unjudged. Recording either as checked
// is the false green the whole check removes.
func checkServiceSecretRequirements(
	ctx context.Context,
	ws *resources.Workspace,
	env *environments.Environment,
	scope []*resources.Service,
	delivered map[string][]string,
	report *workspaceReadinessReport,
) {
	const name = "service secret requirements"
	if len(scope) == 0 {
		return
	}
	// The requirement is a property of the secret-free contract: a credential
	// the manifests reference through a secretKeyRef the host must have filled.
	// A local apply carries the value inline, from the same configuration flow
	// that runs the service, so no agent is asked and the report says why.
	if env.Local() {
		report.add("", name, "ok",
			fmt.Sprintf("environment %s renders no secret-free manifests, so no agent requires a secret configuration of the host", env.Name),
			"")
		return
	}

	findings := make([][]workspaceDiagnostic, len(scope))
	group, probeCtx := errgroup.WithContext(ctx)
	group.SetLimit(secretRequirementProbes)
	for index := range scope {
		group.Go(func() error {
			findings[index] = probeServiceSecretRequirements(probeCtx, ws, env, scope[index], delivered[serviceUnique(scope[index])])
			return nil
		})
	}
	// Every probe records its own diagnostic, so the group never carries an
	// error; waiting is what makes the report complete.
	_ = group.Wait()

	judged := 0
	for _, diagnostics := range findings {
		// A service is judged when its agent answered under the contract: no
		// diagnostic at all, or missing keys, which is an answer. An agent that
		// could not be asked, or that cannot say, judged nothing.
		if len(diagnostics) == 0 || diagnostics[0].Code == codeSecretConfigurationRequired {
			judged++
		}
		for index := range diagnostics {
			report.Checks = append(report.Checks, diagnostics[index])
			if diagnostics[index].Status == checkStatusFail {
				report.Status = readinessStatusNotReady
			}
		}
	}
	report.add("", name, "ok",
		fmt.Sprintf("%d of %d service(s) asked their builder agent what a render of %s requires", judged, len(scope), env.Name),
		"")
}

// probeServiceSecretRequirements asks one service's agent and returns what it
// learned, as diagnostics. It returns nothing only when the agent answered
// under the contract and nothing is missing.
func probeServiceSecretRequirements(
	ctx context.Context,
	ws *resources.Workspace,
	env *environments.Environment,
	svc *resources.Service,
	delivered []string,
) []workspaceDiagnostic {
	unique := serviceUnique(svc)
	name := "secret requirements of " + unique

	capabilities, err := serviceSecretRequirementProbe(ctx, ws, env, svc)
	if err != nil {
		return []workspaceDiagnostic{{
			Code:   codeSecretRequirementUnknown,
			Name:   name,
			Status: checkStatusFail,
			Message: fmt.Sprintf("cannot ask %s which secret configurations a render of %s requires: %v",
				unique, env.Name, err),
			Remediation: fmt.Sprintf("the render drives the same agent, so it fails on this too: fix the module %s comes from, then re-run", unique),
		}}
	}
	missing, err := coreservices.MissingServiceSecretConfigurations(
		unique,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
		capabilities,
		delivered,
	)
	if err != nil {
		return []workspaceDiagnostic{{
			Code:   codeSecretRequirementUndeclared,
			Name:   name,
			Status: checkStatusWarn,
			Message: fmt.Sprintf("%s does not declare which secret configurations its render requires, so none of them is checked for %s: %v",
				agentLabel(svc), unique, err),
			Remediation: fmt.Sprintf("release %s with the declaration; until then a missing secret configuration of %s surfaces as the agent's own refusal, inside the render", agentLabel(svc), unique),
		}}
	}

	diagnostics := make([]workspaceDiagnostic, 0, len(missing))
	for _, entry := range missing {
		diagnostics = append(diagnostics, workspaceDiagnostic{
			Code:   codeSecretConfigurationRequired,
			Name:   name,
			Status: checkStatusFail,
			Message: fmt.Sprintf("a render of %s requires secret configuration %q of %s with %s, and the environment delivers none: the agent refuses the render over %s, after the service's image is built",
				env.Name, entry.Configuration, unique, namedKeys(entry.Keys), entry.EnvironmentKeys[0]),
			Remediation: fmt.Sprintf("codefly config generate %s %s --service %s --env %s",
				entry.Configuration, strings.Join(entry.Keys, " "), unique, env.Name),
		})
	}
	return diagnostics
}

// agentLabel names the agent a service runs, so a warning about a declaration
// an agent does not implement names the release that has to carry it.
func agentLabel(svc *resources.Service) string {
	if svc.Agent == nil {
		return "the agent of " + serviceUnique(svc)
	}
	return fmt.Sprintf("agent %s/%s %s", svc.Agent.Publisher, svc.Agent.Name, svc.Agent.Version)
}

// namedKeys spells the missing keys the way the remediation spells them.
func namedKeys(keys []string) string {
	if len(keys) == 1 {
		return "key " + keys[0]
	}
	return "keys " + strings.Join(keys, ", ")
}

// unjudgedSecretRequirements counts the services whose render requirement the
// report could not establish. The readiness summary carries it, so a verdict of
// ready never reads as "every requirement was checked".
func (report *workspaceReadinessReport) unjudgedSecretRequirements() int {
	count := 0
	for index := range report.Checks {
		if report.Checks[index].Code == codeSecretRequirementUndeclared {
			count++
		}
	}
	return count
}

// readinessQualifier is what a verdict of ready must not leave out: the
// services whose agent could not say what its render requires. It is empty when
// everything in scope was judged.
func readinessQualifier(report *workspaceReadinessReport) string {
	unjudged := report.unjudgedSecretRequirements()
	if unjudged == 0 {
		return ""
	}
	return fmt.Sprintf(" %d service(s) were not checked against what their agent's render requires — see the ⚠ items above.", unjudged)
}

// serviceBuildCapabilities loads one service's builder agent and asks it, for
// this environment, what it supports and requires. Builder.Load primes the
// agent with the service's identity, which is what makes the answer this
// service's own settings rather than the agent's defaults.
func serviceBuildCapabilities(
	ctx context.Context,
	ws *resources.Workspace,
	env *environments.Environment,
	svc *resources.Service,
) (*builderv0.BuildCapabilitiesResponse, error) {
	identity, err := svc.Identity()
	if err != nil {
		return nil, fmt.Errorf("cannot read its identity: %w", err)
	}
	module, err := ws.LoadModuleFromName(ctx, identity.Module)
	if err != nil {
		return nil, fmt.Errorf("cannot load module %q: %w", identity.Module, err)
	}
	instance, err := services.Load(ctx, ws, module, svc)
	if err != nil {
		return nil, fmt.Errorf("cannot load its agent: %w", err)
	}
	if err := instance.LoadBuilder(ctx); err != nil {
		return nil, fmt.Errorf("cannot load its builder agent: %w", err)
	}
	if _, err := instance.Builder.Load(ctx); err != nil {
		return nil, fmt.Errorf("its builder agent does not load the service: %w", err)
	}
	return instance.Builder.Builder.BuildCapabilities(ctx, &builderv0.BuildCapabilitiesRequest{Environment: env.Name})
}

// deliveredServiceSecretKeys is every service-secret configuration key this
// environment will hand each service in scope, in the spelling the rendered
// manifests and the agents' refusals both use.
//
// Two sources deliver one, and only two:
//
//   - A secret configuration the service carries for this environment's
//     profile. checkConfigurationSources has already read them; the keys come
//     from that read rather than a second one, so the doctor judges the files
//     it reported on.
//   - A service-scoped generator the environment declares, when the environment
//     keeps its secrets in files rather than a store: a run mints those keys
//     into exactly these files before it loads configuration
//     (cmd/run.provisionGeneratedSecrets), so a cold checkout that has not run
//     yet is correctly not reported as broken.
//
// An environment whose service-secrets names a store does NOT deliver a
// declaration that way: its generators mint values into that store, and a
// rendered ExternalSecret is projected from the non-optional secretKeyRefs the
// manifests already carry — which exist only because the service declared the
// key. A remote-keys entry is the same: it says where a key resolves, never
// that it exists. So for a stored environment the declaration is the service's
// own secret configuration, and requiring it is the check rather than an
// over-reach.
func deliveredServiceSecretKeys(env *environments.Environment, scope []*resources.Service, declared []scopedConfiguration) map[string][]string {
	uniques := make([]string, 0, len(scope))
	for _, svc := range scope {
		uniques = append(uniques, serviceUnique(svc))
	}
	delivered := make(map[string][]string, len(scope))
	for _, scoped := range declared {
		if scoped.origin == resources.ConfigurationWorkspace {
			continue
		}
		for _, value := range scoped.info.GetConfigurationValues() {
			if !value.GetSecret() {
				continue
			}
			delivered[scoped.origin] = append(delivered[scoped.origin],
				resources.ServiceSecretConfigurationKeyFromUnique(scoped.origin, scoped.info.GetName(), value.GetKey()))
		}
	}
	if env.ServiceSecrets != nil && !env.ServiceSecrets.Stored() {
		for index := range env.ServiceSecrets.Generate {
			generator := &env.ServiceSecrets.Generate[index]
			if generator.Scope != environments.SecretGeneratorScopeService {
				continue
			}
			for _, key := range generator.StoredKeys(uniques) {
				if unique, ok := secretKeyOwner(key, uniques); ok {
					delivered[unique] = append(delivered[unique], key)
				}
			}
		}
	}
	for unique := range delivered {
		sort.Strings(delivered[unique])
	}
	return delivered
}

// secretKeyOwner names the service a stored key belongs to, by the prefix core
// derives from its unique. Matching the prefix rather than re-deriving the key
// keeps one spelling of it in the CLI.
func secretKeyOwner(key string, uniques []string) (string, bool) {
	for _, unique := range uniques {
		if strings.HasPrefix(key, resources.ServiceSecretConfigurationEnvironmentKeyPrefixFromUnique(unique)+"__") {
			return unique, true
		}
	}
	return "", false
}
