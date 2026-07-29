package orchestration

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPromotableDeploymentInputsReplaceSecretValuesWithReferences(t *testing.T) {
	configuration := &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "database",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "host", Value: "postgres.users.svc"},
				{Key: "password", Value: "own-secret", Secret: true},
			},
		}},
	}
	dependency := &basev0.Configuration{
		Origin: "infra/postgres",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "postgres",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "connection", Value: "postgres://credential-bearing-value"},
				{Key: "port", Value: "5432"},
			},
		}},
	}
	originalConfiguration := proto.Clone(configuration)
	originalDependency := proto.Clone(dependency)

	sanitized, dependencies, references, err := promotableDeploymentInputs(
		"accounts",
		configuration,
		[]*basev0.Configuration{dependency},
	)
	require.NoError(t, err)

	require.Equal(t, originalConfiguration, configuration)
	require.Equal(t, originalDependency, dependency)
	require.Equal(t, []*basev0.ConfigurationValue{
		{Key: "host", Value: "postgres.users.svc"},
	}, sanitized.GetInfos()[0].GetConfigurationValues())
	require.Equal(t, []*basev0.ConfigurationValue{
		{Key: "port", Value: "5432"},
	}, dependencies[0].GetInfos()[0].GetConfigurationValues())
	require.Equal(t, map[string]*builderv0.KubernetesSecretKeyReference{
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__DATABASE__PASSWORD": {
			Name: "accounts-secrets",
			Key:  "CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__DATABASE__PASSWORD",
		},
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__INFRA__POSTGRES__POSTGRES__CONNECTION": {
			Name: "accounts-secrets",
			Key:  "CODEFLY__SERVICE_SECRET_CONFIGURATION__INFRA__POSTGRES__POSTGRES__CONNECTION",
		},
	}, references)
}

func TestPromotableDeploymentInputsRejectSecretStructuredData(t *testing.T) {
	configuration := &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "certificate",
			Data: &basev0.ConfigurationData{
				Kind:    "pem",
				Content: []byte("secret certificate"),
				Secret:  true,
			},
		}},
	}

	_, _, _, err := promotableDeploymentInputs("accounts", configuration, nil)
	require.EqualError(t, err, `secret configuration data "certificate" has no Kubernetes Secret key reference`)
}

func TestKubernetesOutputProfileDefaultsByClusterAndHonorsExplicitGitOps(t *testing.T) {
	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
		kubernetesOutputProfile(&World{Env: resources.LocalEnvironment()}),
	)
	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		kubernetesOutputProfile(&World{Env: &resources.Environment{
			Name:    "aws",
			Cluster: &resources.EnvironmentCluster{Kind: "eks"},
		}}),
	)
	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		kubernetesOutputProfile(&World{
			Env:                     resources.LocalEnvironment(),
			KubernetesOutputProfile: builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		}),
	)
}

func TestValidateKubernetesDeploymentOutputRejectsProfileMismatch(t *testing.T) {
	output := validKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
	)

	err := validateKubernetesDeploymentOutput(
		output,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		"k3d-codefly-local",
	)
	require.EqualError(t, err,
		"builder returned profile KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1 for requested profile KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1",
	)
}

func TestValidateKubernetesDeploymentOutputAcceptsPromotableContract(t *testing.T) {
	output := validKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	)

	require.NoError(t, validateKubernetesDeploymentOutput(
		output,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		"k3d-codefly-local",
	))
}

func TestValidateKubernetesDeploymentOutputRejectsDifferentValidationContext(t *testing.T) {
	output := validKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	)

	err := validateKubernetesDeploymentOutput(
		output,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		"mind-aws",
	)
	require.EqualError(t, err,
		`builder validated Kubernetes context "k3d-codefly-local" for requested context "mind-aws"`,
	)
}

func TestValidateKubernetesDeploymentOutputAcceptsOfflinePromotableContract(t *testing.T) {
	output := validKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	)
	output.GetKubernetes().Validation.ServerSideValidation =
		builderv0.KubernetesManifestValidation_STATUS_NOT_RUN
	output.GetKubernetes().Validation.ValidatedContext = ""

	require.NoError(t, validateKubernetesDeploymentOutput(
		output,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		"",
	))
}

func validKubernetesDeploymentOutput(profile builderv0.KubernetesOutputProfile) *builderv0.DeploymentOutput {
	return &builderv0.DeploymentOutput{
		Kind: &builderv0.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeploymentOutput{
				Profile:         profile,
				ContractVersion: "kubernetes-output/v1",
				Validation: &builderv0.KubernetesManifestValidation{
					StaticValidation:     builderv0.KubernetesManifestValidation_STATUS_PASSED,
					ServerSideValidation: builderv0.KubernetesManifestValidation_STATUS_PASSED,
					Promotable:           true,
					ValidatedContext:     "k3d-codefly-local",
				},
			},
		},
	}
}
