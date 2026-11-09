package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	coreservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

type secretRequirementProbe func(context.Context, *resources.Workspace, *environments.Environment, *resources.Service) (*builderv0.BuildCapabilitiesResponse, error)

func installSecretRequirementProbe(t *testing.T, probe secretRequirementProbe) {
	t.Helper()
	previous := serviceSecretRequirementProbe
	serviceSecretRequirementProbe = probe
	t.Cleanup(func() { serviceSecretRequirementProbe = previous })
}

// probeRequiring answers as an agent that implements the declaration and
// requires the named group with the named keys. With no group it requires
// nothing, which is the answer a test unrelated to this check needs.
func probeRequiring(required ...*builderv0.RequiredSecretConfiguration) secretRequirementProbe {
	return func(context.Context, *resources.Workspace, *environments.Environment, *resources.Service) (*builderv0.BuildCapabilitiesResponse, error) {
		return &builderv0.BuildCapabilitiesResponse{
			ExecutionContracts:           []string{coreservices.SecretRequirementsContract},
			RequiredSecretConfigurations: required,
		}, nil
	}
}

const hostedWorkspaceYAML = testWorkspaceYAML + `environments:
    - name: local
    - name: blue
      configuration-profiles: ["blue"]
      namespace: demo-blue
      service-secrets:
          secret-store:
              name: cell-secrets
              kind: ClusterSecretStore
`

func postgresRequirement() *builderv0.RequiredSecretConfiguration {
	return &builderv0.RequiredSecretConfiguration{
		Configuration: "postgres",
		Keys:          []string{"POSTGRES_USER", "POSTGRES_PASSWORD"},
		Profiles: []builderv0.KubernetesOutputProfile{
			builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1,
		},
	}
}

// Tonight's failure: a store composed onto a cell for the first time declares
// no secret configuration for it, the doctor said ready, and the postgres agent
// refused the render 57 minutes of image builds later. The readiness run now
// refuses first, over the key the agent refuses over, and hands back the
// command that fixes it.
func TestASecretConfigurationNoServiceDeclaresIsRefusedBeforeAnyBuild(t *testing.T) {
	dir := singleServiceWorkspace(t, hostedWorkspaceYAML, nil, nil)
	installSecretRequirementProbe(t, probeRequiring(postgresRequirement()))

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "blue"})
	require.Equal(t, readinessStatusNotReady, report.Status, reportJSON(t, report))
	diagnostic := requireCode(t, report, codeSecretConfigurationRequired, checkStatusFail)
	require.Contains(t, diagnostic.Message, `secret configuration "postgres" of backend/api`)
	require.Contains(t, diagnostic.Message, "keys POSTGRES_PASSWORD, POSTGRES_USER")
	require.Contains(t, diagnostic.Message,
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__BACKEND__API__POSTGRES__POSTGRES_PASSWORD")
	require.Equal(t,
		"codefly config generate postgres POSTGRES_PASSWORD POSTGRES_USER --service backend/api --env blue",
		diagnostic.Remediation)
}

// The whole set in one pass: two groups, several keys, one readiness run. The
// render would have surfaced them one per render.
func TestEveryMissingSecretConfigurationIsReportedInOnePass(t *testing.T) {
	dir := singleServiceWorkspace(t, hostedWorkspaceYAML, nil, nil)
	installSecretRequirementProbe(t, probeRequiring(
		postgresRequirement(),
		&builderv0.RequiredSecretConfiguration{Configuration: "hop-signing", Keys: []string{"SEED"}},
	))

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "blue"})
	require.Len(t, findDiagnostics(report, codeSecretConfigurationRequired), 2)
}

// The service's own secret configuration for the environment's profile is the
// declaration, so a service that carries it is ready.
func TestADeclaredSecretConfigurationClearsTheCheck(t *testing.T) {
	dir := singleServiceWorkspace(t, hostedWorkspaceYAML, nil, map[string]string{
		"modules/backend/services/api/configurations/blue/postgres.secret.env": "POSTGRES_USER=owner\nPOSTGRES_PASSWORD=secret\n",
	})
	installSecretRequirementProbe(t, probeRequiring(postgresRequirement()))

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "blue"})
	requireNoCode(t, report, codeSecretConfigurationRequired)
	require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
}

// An agent that cannot be asked is a failure, not silence: the render drives
// the same agent and fails on the same thing.
func TestAnAgentThatCannotBeAskedFailsTheReport(t *testing.T) {
	dir := singleServiceWorkspace(t, hostedWorkspaceYAML, nil, nil)
	installSecretRequirementProbe(t, func(context.Context, *resources.Workspace, *environments.Environment, *resources.Service) (*builderv0.BuildCapabilitiesResponse, error) {
		return nil, errors.New("agent artifact is not materialized")
	})

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "blue"})
	require.Equal(t, readinessStatusNotReady, report.Status, reportJSON(t, report))
	diagnostic := requireCode(t, report, codeSecretRequirementUnknown, checkStatusFail)
	require.Contains(t, diagnostic.Message, "cannot ask backend/api")
	require.Contains(t, diagnostic.Message, "agent artifact is not materialized")
}

// An agent that answers without the declaration is named, with its version, and
// counted in the verdict — never recorded as checked. This is the one outcome
// that leaves the workspace ready, because an agent predating the contract is
// the fleet's normal state while it rolls out; what must not happen is a
// verdict that reads as if the requirement had been established.
func TestAnAgentWithoutTheDeclarationIsNamedAndCounted(t *testing.T) {
	dir := singleServiceWorkspace(t, hostedWorkspaceYAML, nil, nil)
	installSecretRequirementProbe(t, func(context.Context, *resources.Workspace, *environments.Environment, *resources.Service) (*builderv0.BuildCapabilitiesResponse, error) {
		return &builderv0.BuildCapabilitiesResponse{BuildxSelection: true}, nil
	})

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "blue"})
	require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
	diagnostic := requireCode(t, report, codeSecretRequirementUndeclared, "warn")
	require.Contains(t, diagnostic.Message, "agent codefly.ai/go-grpc 0.0.16")
	require.Contains(t, diagnostic.Message, "backend/api")
	require.Equal(t, 1, report.unjudgedSecretRequirements())
	require.Contains(t, readinessQualifier(report), "1 service(s) were not checked")
	require.Contains(t, findCheck(report, "service secret requirements").Message, "0 of 1 service(s)")
}

// A verdict of ready says how much it judged, so "ready" never has to be taken
// on faith.
func TestTheSummaryStatesHowManyServicesWereJudged(t *testing.T) {
	dir := singleServiceWorkspace(t, hostedWorkspaceYAML, nil, nil)
	installSecretRequirementProbe(t, probeRequiring())

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "blue"})
	require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
	require.Contains(t, findCheck(report, "service secret requirements").Message, "1 of 1 service(s)")
	require.Empty(t, readinessQualifier(report))
}

// A local environment carries secret values inline, so no agent is asked and
// the report says why rather than reporting a check it did not run.
func TestALocalEnvironmentAsksNoAgentAndSaysSo(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, nil)
	installSecretRequirementProbe(t, func(context.Context, *resources.Workspace, *environments.Environment, *resources.Service) (*builderv0.BuildCapabilitiesResponse, error) {
		t.Fatal("a local environment must ask no builder agent")
		return nil, nil
	})

	report := runReadinessWithInstalledProbe(t, workspaceReadinessOptions{dir: dir, env: "local"})
	require.Contains(t, findCheck(report, "service secret requirements").Message, "renders no secret-free manifests")
}

func secretValues(keys ...string) *basev0.ConfigurationInformation {
	info := &basev0.ConfigurationInformation{Name: "postgres"}
	for _, key := range keys {
		info.ConfigurationValues = append(info.ConfigurationValues, &basev0.ConfigurationValue{Key: key, Secret: true})
	}
	return info
}

func serviceNamed(t *testing.T, module, name string) *resources.Service {
	t.Helper()
	svc := &resources.Service{Name: name, Version: "0.0.0"}
	svc.WithModule(module)
	return svc
}

// A stored environment's generator mints a value into the cell's store, never a
// declaration: the manifests reference a key only because the service declared
// it, so the file is still what the render needs and requiring it is the check.
func TestAStoredEnvironmentsGeneratorIsNotADeclaration(t *testing.T) {
	env := &environments.Environment{
		Name: "blue",
		ServiceSecrets: &environments.EnvironmentServiceSecrets{
			SecretStore: environments.EnvironmentSecretStoreReference{Name: "cell-secrets", Kind: "ClusterSecretStore"},
			Generate: []environments.EnvironmentSecretGenerator{{
				Scope: environments.SecretGeneratorScopeService, Configuration: "postgres",
				Services: []string{"org-directory/store"}, Keys: []string{"POSTGRES_USER"},
			}},
		},
	}
	scope := []*resources.Service{serviceNamed(t, "org-directory", "store")}
	require.Empty(t, deliveredServiceSecretKeys(env, scope, nil))
}

// A file-backed environment's generator IS a declaration: the run writes those
// keys into the service's own secret configuration before it loads any, so a
// cold checkout that has not run yet must not be reported as broken.
func TestAFileBackedEnvironmentsGeneratorIsADeclaration(t *testing.T) {
	env := &environments.Environment{
		Name: "local",
		ServiceSecrets: &environments.EnvironmentServiceSecrets{
			Generate: []environments.EnvironmentSecretGenerator{{
				Scope: environments.SecretGeneratorScopeService, Configuration: "postgres",
				Services: []string{"org-directory/store"}, Keys: []string{"POSTGRES_USER"},
			}},
		},
	}
	scope := []*resources.Service{serviceNamed(t, "org-directory", "store")}
	require.Equal(t, map[string][]string{
		"org-directory/store": {"CODEFLY__SERVICE_SECRET_CONFIGURATION__ORG_DIRECTORY__STORE__POSTGRES__POSTGRES_USER"},
	}, deliveredServiceSecretKeys(env, scope, nil))
}

// The keys come from the very read the doctor already reported on, in the
// spelling the agent's refusal uses, and a workspace-origin group is not a
// service's declaration.
func TestDeliveredKeysComeFromTheServicesOwnConfiguration(t *testing.T) {
	env := &environments.Environment{Name: "blue"}
	scope := []*resources.Service{serviceNamed(t, "org-directory", "store")}
	declared := []scopedConfiguration{
		{origin: "org-directory/store", info: secretValues("POSTGRES_USER", "POSTGRES_PASSWORD")},
		{origin: resources.ConfigurationWorkspace, info: secretValues("IGNORED")},
	}
	require.Equal(t, map[string][]string{
		"org-directory/store": {
			"CODEFLY__SERVICE_SECRET_CONFIGURATION__ORG_DIRECTORY__STORE__POSTGRES__POSTGRES_PASSWORD",
			"CODEFLY__SERVICE_SECRET_CONFIGURATION__ORG_DIRECTORY__STORE__POSTGRES__POSTGRES_USER",
		},
	}, deliveredServiceSecretKeys(env, scope, declared))
}
