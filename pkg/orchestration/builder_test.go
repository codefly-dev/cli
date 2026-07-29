package orchestration

import (
	"strings"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
)

func TestBuildResultKindAssertionIsSafeForNonDockerResults(t *testing.T) {
	if got := dockerBuildResult(nil); got != nil {
		t.Fatalf("nil result returned %#v", got)
	}
	if got := dockerBuildResult(&builderv0.BuildResult{}); got != nil {
		t.Fatalf("empty result returned %#v", got)
	}
	want := &builderv0.DockerBuildResult{Images: []string{"example:test"}}
	result := &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildResult{DockerBuildResult: want}}
	if got := dockerBuildResult(result); got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestPromotableDeploymentConfigurationsReplaceSecretBytesWithTypedReferences(t *testing.T) {
	configuration := &basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "authentication",
			ConfigurationValues: []*basev0.ConfigurationValue{
				{Key: "issuer", Value: "https://auth.example.com"},
				{Key: "client-secret", Value: "must-not-pass", Secret: true},
				{Key: "api-token", Value: "also-must-not-pass"},
			},
		}},
	}

	safe, dependencies, references, err := promotableDeploymentConfigurations(
		configuration,
		nil,
		"secret-accounts",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 0 {
		t.Fatalf("dependencies = %+v", dependencies)
	}
	if got := safe.GetInfos()[0].GetConfigurationValues(); len(got) != 1 || got[0].GetKey() != "issuer" {
		t.Fatalf("safe configuration = %+v", safe)
	}
	if !strings.Contains(configuration.String(), "must-not-pass") {
		t.Fatal("source configuration was mutated")
	}
	for _, key := range []string{
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__AUTHENTICATION__CLIENT_SECRET",
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__USERS__ACCOUNTS__AUTHENTICATION__API_TOKEN",
	} {
		reference := references[key]
		if reference == nil || reference.GetName() != "secret-accounts" || reference.GetKey() != key {
			t.Fatalf("reference %q = %+v", key, reference)
		}
	}
	for _, reference := range references {
		if strings.Contains(reference.String(), "must-not-pass") {
			t.Fatalf("secret bytes reached typed reference: %+v", reference)
		}
	}
}

func TestPromotableDeploymentConfigurationsRejectStructuredSecretBytes(t *testing.T) {
	_, _, _, err := promotableDeploymentConfigurations(&basev0.Configuration{
		Origin: "users/accounts",
		Infos: []*basev0.ConfigurationInformation{{
			Name: "certificate",
			Data: &basev0.ConfigurationData{
				Secret: true, Content: []byte("must-not-pass"),
			},
		}},
	}, nil, "secret-accounts")
	if err == nil || !strings.Contains(err.Error(), "typed Kubernetes key references") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateKubernetesDeploymentOutputRequiresRequestedProfile(t *testing.T) {
	requested := builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1
	output := &builderv0.DeploymentOutput{
		Kind: &builderv0.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv0.KubernetesDeploymentOutput{
				Profile: builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
			},
		},
	}
	err := validateKubernetesDeploymentOutput(requested, output, "")
	if err == nil || !strings.Contains(err.Error(), "requested") {
		t.Fatalf("error = %v", err)
	}
	output.GetKubernetes().Profile = requested
	output.GetKubernetes().ContractVersion = coreservices.KubernetesManifestContractVersion
	output.GetKubernetes().Validation = &builderv0.KubernetesManifestValidation{
		StaticValidation:     builderv0.KubernetesManifestValidation_STATUS_PASSED,
		ServerSideValidation: builderv0.KubernetesManifestValidation_STATUS_PASSED,
		Promotable:           true,
	}
	if err := validateKubernetesDeploymentOutput(requested, output, ""); err != nil {
		t.Fatal(err)
	}
	output.GetKubernetes().Validation.Promotable = false
	if err := validateKubernetesDeploymentOutput(requested, output, ""); err == nil || !strings.Contains(err.Error(), "successfully validated") {
		t.Fatalf("validation error = %v", err)
	}
}
