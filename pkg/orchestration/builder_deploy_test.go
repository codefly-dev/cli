package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/deployments"
	coreservices "github.com/codefly-dev/core/agents/services"
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

	sanitized, dependencies, references, err := promotableDeploymentConfigurations(
		configuration,
		[]*basev0.Configuration{dependency},
		"accounts-secrets",
	)
	require.NoError(t, err)

	require.True(t, proto.Equal(originalConfiguration, configuration))
	require.True(t, proto.Equal(originalDependency, dependency))
	require.Len(t, sanitized.GetInfos()[0].GetConfigurationValues(), 1)
	require.Equal(t, "host", sanitized.GetInfos()[0].GetConfigurationValues()[0].GetKey())
	require.Equal(t, "postgres.users.svc", sanitized.GetInfos()[0].GetConfigurationValues()[0].GetValue())
	require.Len(t, dependencies[0].GetInfos()[0].GetConfigurationValues(), 1)
	require.Equal(t, "port", dependencies[0].GetInfos()[0].GetConfigurationValues()[0].GetKey())
	require.Equal(t, "5432", dependencies[0].GetInfos()[0].GetConfigurationValues()[0].GetValue())
	for _, key := range []string{
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__DATABASE__PASSWORD",
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__INFRA__POSTGRES__POSTGRES__CONNECTION",
	} {
		require.Equal(t, "accounts-secrets", references[key].GetName())
		require.Equal(t, key, references[key].GetKey())
	}
}

func TestPromotableDeploymentInputsRejectMisplacedSecretEndpointKeys(t *testing.T) {
	configuration := &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "identity",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "IDENTITY_AUTHORIZE_URL", Value: "https://issuer.example.com/authorize"},
				{Key: "IDENTITY_TOKEN_URL", Value: "https://issuer.example.com/token"},
				{Key: "IDENTITY_AUTHORIZE_SELECTOR", Value: "primary"},
				{Key: "IDENTITY_ISSUER", Value: "https://issuer.example.com"},
			},
		}},
	}

	_, _, _, err := promotableDeploymentConfigurations(configuration, nil, "accounts-secrets")
	require.Error(t, err)
	// Every misplaced key is named in one error, each with the file it belongs in.
	require.Contains(t, err.Error(), "IDENTITY_AUTHORIZE_URL (identity.secret.env)")
	require.Contains(t, err.Error(), "IDENTITY_TOKEN_URL (identity.secret.env)")
	require.Contains(t, err.Error(), "IDENTITY_AUTHORIZE_SELECTOR (identity.secret.env)")
	require.Contains(t, err.Error(), "identity.secret.env")
	// A plain endpoint-shaped key (no credential marker) is not a secret and must
	// not be dragged into the misplacement error.
	require.NotContains(t, err.Error(), "IDENTITY_ISSUER")
}

func TestPromotableDeploymentInputsPromoteDeclaredSecretEndpointKeys(t *testing.T) {
	configuration := &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "identity",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "IDENTITY_AUTHORIZE_URL", Value: "https://issuer.example.com/authorize", Secret: true},
				{Key: "IDENTITY_SELECTOR", Value: "primary"},
			},
		}},
	}

	sanitized, _, references, err := promotableDeploymentConfigurations(configuration, nil, "accounts-secrets")
	require.NoError(t, err)

	values := sanitized.GetInfos()[0].GetConfigurationValues()
	kept := map[string]string{}
	for _, value := range values {
		kept[value.GetKey()] = value.GetValue()
	}
	require.Equal(t, map[string]string{"IDENTITY_SELECTOR": "primary"}, kept)

	prefix := "CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__IDENTITY__"
	require.Equal(t, map[string]*builderv0.KubernetesSecretKeyReference{
		prefix + "IDENTITY_AUTHORIZE_URL": {Name: "accounts-secrets", Key: prefix + "IDENTITY_AUTHORIZE_URL"},
	}, references)
}

func TestPromotableDeploymentInputsPromoteCredentialURLsAsReferences(t *testing.T) {
	configuration := &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "identity",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "DATABASE_URL", Value: "postgres://credential-bearing-value"},
				{Key: "SECRET_URL", Value: "credential-bearing-value"},
				{Key: "PRIVATE_KEY_URL", Value: "credential-bearing-value"},
				{Key: "ACCESS_TOKEN", Value: "credential-bearing-value"},
			},
		}},
	}

	sanitized, _, references, err := promotableDeploymentConfigurations(configuration, nil, "accounts-secrets")
	require.NoError(t, err)
	require.Empty(t, sanitized.GetInfos())

	prefix := "CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__IDENTITY__"
	require.Equal(t, map[string]*builderv0.KubernetesSecretKeyReference{
		prefix + "DATABASE_URL":    {Name: "accounts-secrets", Key: prefix + "DATABASE_URL"},
		prefix + "SECRET_URL":      {Name: "accounts-secrets", Key: prefix + "SECRET_URL"},
		prefix + "PRIVATE_KEY_URL": {Name: "accounts-secrets", Key: prefix + "PRIVATE_KEY_URL"},
		prefix + "ACCESS_TOKEN":    {Name: "accounts-secrets", Key: prefix + "ACCESS_TOKEN"},
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

	_, _, _, err := promotableDeploymentConfigurations(configuration, nil, "accounts-secrets")
	require.EqualError(t, err, `structured secret configuration "certificate" requires typed Kubernetes key references`)
}

func TestPromotableDeploymentInputsPreserveWorkspaceConfigurationAndExtractSecrets(t *testing.T) {
	workspaceConfiguration := &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name: "workos",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "MODE", Value: "production"},
				{Key: "WORKOS_CLIENT_SECRET", Value: "credential-bearing-value", Secret: true},
			},
		}},
	}

	_, dependencies, references, err := promotableDeploymentConfigurations(
		nil,
		[]*basev0.Configuration{workspaceConfiguration},
		"accounts-secrets",
	)
	require.NoError(t, err)
	values := dependencies[0].GetInfos()[0].GetConfigurationValues()
	require.Len(t, values, 1)
	require.Equal(t, "MODE", values[0].GetKey())
	require.Equal(t, "production", values[0].GetValue())
	require.False(t, values[0].GetSecret())
	require.Equal(t, map[string]*builderv0.KubernetesSecretKeyReference{
		"CODEFLY__WORKSPACE_SECRET_CONFIGURATION__WORKOS__WORKOS_CLIENT_SECRET": {
			Name: "accounts-secrets",
			Key:  "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__WORKOS__WORKOS_CLIENT_SECRET",
		},
	}, references)
}

func TestKubernetesOutputProfileReservesEphemeralForDirectLocalApply(t *testing.T) {
	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		kubernetesOutputProfile(&World{Env: resources.LocalEnvironment()}),
	)
	require.Equal(t,
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
		kubernetesOutputProfile(&World{
			Env:           resources.LocalEnvironment(),
			RemoteManager: &deployments.LocalApplyManager{},
		}),
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
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		output,
		"k3d-codefly-local",
	)
	require.EqualError(t, err,
		"plugin returned Kubernetes output profile KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1, requested KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1",
	)
}

func TestValidateKubernetesDeploymentOutputAcceptsPromotableContract(t *testing.T) {
	output := validKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	)

	require.NoError(t, validateKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		output,
		"k3d-codefly-local",
	))
}

func TestValidateKubernetesDeploymentOutputRejectsDifferentValidationContext(t *testing.T) {
	output := validKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
	)

	err := validateKubernetesDeploymentOutput(
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		output,
		"mind-aws",
	)
	require.EqualError(t, err,
		`plugin validated Kubernetes context "k3d-codefly-local", requested "mind-aws"`,
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
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		output,
		"",
	))
}

func TestValidateDeploymentOutputAllowsOptionalNoDeploymentResponse(t *testing.T) {
	require.NoError(t, validateDeploymentOutput(
		&deployments.RenderManager{},
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		nil,
		"",
	))
	require.EqualError(t, validateDeploymentOutput(
		requiredDeploymentOutputManager{},
		builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1,
		nil,
		"",
	), "plugin returned no Kubernetes deployment output")
}

func TestWithContainerReachableAsPublicMirrorsContainerOnlyEndpoint(t *testing.T) {
	container := resources.NewHTTPNetworkInstance("saas-vault.saas.svc.cluster.local", 8080, false)
	container.Access = resources.NewContainerNetworkAccess()
	mappings := []*basev0.NetworkMapping{{
		Endpoint:  &basev0.Endpoint{Module: "saas", Service: "vault", Name: "http", Api: "http", Visibility: resources.VisibilityModule},
		Instances: []*basev0.NetworkInstance{container},
	}}

	got := withContainerReachableAsPublic(context.Background(), mappings)

	require.Nil(t, resources.FilterNetworkInstance(context.Background(), mappings[0].Instances, resources.NewPublicNetworkAccess()),
		"source mappings must not be mutated")
	public := resources.FilterNetworkInstance(context.Background(), got[0].Instances, resources.NewPublicNetworkAccess())
	require.NotNil(t, public)
	require.Equal(t, container.GetAddress(), public.GetAddress())
	require.Equal(t, container.GetHost(), public.GetHost())
	require.Equal(t, container.GetPort(), public.GetPort())
}

func TestWithContainerReachableAsPublicLeavesEndpointsWithPublicUntouched(t *testing.T) {
	public := resources.NewHTTPNetworkInstance("frontend.example.com", 443, true)
	public.Access = resources.NewPublicNetworkAccess()
	container := resources.NewHTTPNetworkInstance("frontend.saas.svc.cluster.local", 8080, false)
	container.Access = resources.NewContainerNetworkAccess()
	mappings := []*basev0.NetworkMapping{{
		Endpoint:  &basev0.Endpoint{Module: "saas", Service: "frontend", Name: "http", Api: "http", Visibility: resources.VisibilityPublic},
		Instances: []*basev0.NetworkInstance{public, container},
	}}

	got := withContainerReachableAsPublic(context.Background(), mappings)

	require.Len(t, got[0].Instances, 2)
	require.Same(t, mappings[0], got[0])
}

type requiredDeploymentOutputManager struct{}

func (requiredDeploymentOutputManager) RequiresDeploymentOutput() bool {
	return true
}

func (requiredDeploymentOutputManager) Handle(
	context.Context,
	*resources.Service,
	*resources.Module,
	*builderv0.DeploymentOutput,
) error {
	return nil
}

func validKubernetesDeploymentOutput(profile builderv0.KubernetesOutputProfile) *builderv0.DeploymentOutput {
	return &builderv0.DeploymentOutput{
		Kind: &builderv0.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeploymentOutput{
				Profile:         profile,
				ContractVersion: coreservices.KubernetesManifestContractVersion,
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
