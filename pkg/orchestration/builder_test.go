package orchestration

import (
	"strings"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestBuildResultKindAssertionIsSafeForNonDockerResults(t *testing.T) {
	if got := dockerBuildResult(nil); got != nil {
		t.Fatalf("nil result returned %#v", got)
	}
	if got := dockerBuildResult(&builderv1.BuildResult{}); got != nil {
		t.Fatalf("empty result returned %#v", got)
	}
	want := &builderv1.DockerBuildResult{Images: []string{"example:test"}}
	result := &builderv1.BuildResult{Kind: &builderv1.BuildResult_DockerBuildResult{DockerBuildResult: want}}
	if got := dockerBuildResult(result); got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestCheckImageArchitectureRejectsNonDeploymentArch(t *testing.T) {
	if err := checkImageArchitecture("frontend:v1", "amd64"); err != nil {
		t.Fatalf("amd64 image rejected: %v", err)
	}
	err := checkImageArchitecture("frontend:v1", "arm64")
	if err == nil {
		t.Fatal("arm64 image was accepted for push to amd64 nodes")
	}
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("error should name both architectures: %v", err)
	}
	if err := checkImageArchitecture("frontend:v1", ""); err == nil {
		t.Fatal("empty architecture was accepted")
	}
}

func TestPromotableDeploymentConfigurationsReplaceSecretBytesWithTypedReferences(t *testing.T) {
	configuration := &basev1.Configuration{
		Origin: "users/accounts",
		Infos: []*basev1.ConfigurationInformation{{
			Name: "authentication",
			ConfigurationValues: []*basev1.ConfigurationValue{
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
	_, _, _, err := promotableDeploymentConfigurations(&basev1.Configuration{
		Origin: "users/accounts",
		Infos: []*basev1.ConfigurationInformation{{
			Name: "certificate",
			Data: &basev1.ConfigurationData{
				Secret: true, Content: []byte("must-not-pass"),
			},
		}},
	}, nil, "secret-accounts")
	if err == nil || !strings.Contains(err.Error(), "typed Kubernetes key references") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateKubernetesDeploymentOutputRequiresRequestedProfile(t *testing.T) {
	requested := builderv1.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_RESTRICTED_PORTABLE_V1
	output := &builderv1.DeploymentOutput{
		Kind: &builderv1.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv1.KubernetesDeploymentOutput{
				Profile: builderv1.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
			},
		},
	}
	err := validateKubernetesDeploymentOutput(requested, output, "")
	if err == nil || !strings.Contains(err.Error(), "requested") {
		t.Fatalf("error = %v", err)
	}
	output.GetKubernetes().Profile = requested
	output.GetKubernetes().ContractVersion = coreservices.KubernetesManifestContractVersion
	output.GetKubernetes().Validation = &builderv1.KubernetesManifestValidation{
		StaticValidation:     builderv1.KubernetesManifestValidation_STATUS_PASSED,
		ServerSideValidation: builderv1.KubernetesManifestValidation_STATUS_PASSED,
		Restricted:           true,
	}
	if err := validateKubernetesDeploymentOutput(requested, output, ""); err != nil {
		t.Fatal(err)
	}
	// The security property, not the delivery decision: clearing restricted is
	// what must refuse.
	output.GetKubernetes().Validation.Restricted = false
	if err := validateKubernetesDeploymentOutput(requested, output, ""); err == nil || !strings.Contains(err.Error(), "successfully validated") {
		t.Fatalf("validation error = %v", err)
	}
}

// A profile this CLI does not request is refused, with or without evidence.
//
// The gate read "anything that is not the restricted profile needs no
// validation evidence", so the deprecated PROMOTABLE_GITOPS_V1 — and any
// profile added to the enum later — was accepted on the strength of the plugin
// echoing back the profile it was asked for. core v0.14.0 deleted that value
// and reserved its number, so the case is stated as the number: a request
// decoded from an older schema still carries it, and the gate refuses it for
// not being one of the two profiles this CLI requests.
func TestValidateKubernetesDeploymentOutputRefusesAProfileTheCLIDoesNotRequest(t *testing.T) {
	// PROMOTABLE_GITOPS_V1's number, which the schema reserves now.
	deprecated := builderv1.KubernetesOutputProfile(2)
	if !builderv1.KubernetesOutputProfile(0).Descriptor().ReservedRanges().Has(protoreflect.EnumNumber(deprecated)) {
		t.Fatalf("profile number %d is not reserved by the schema; the case must name a deleted value", deprecated)
	}
	output := &builderv1.DeploymentOutput{
		Kind: &builderv1.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv1.KubernetesDeploymentOutput{Profile: deprecated},
		},
	}
	err := validateKubernetesDeploymentOutput(deprecated, output, "")
	if err == nil {
		t.Fatal("the deprecated promotable-gitops profile was accepted with no validation evidence")
	}
	if !strings.Contains(err.Error(), "not a profile this CLI requests") {
		t.Fatalf("error = %v", err)
	}

	// The local-apply exemption is BY NAME, and it stands: a direct apply
	// produces no promotable artifact and nothing persists it.
	local := builderv1.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	localOutput := &builderv1.DeploymentOutput{
		Kind: &builderv1.DeploymentOutput_Kubernetes{
			Kubernetes: &builderv1.KubernetesDeploymentOutput{Profile: local},
		},
	}
	if err := validateKubernetesDeploymentOutput(local, localOutput, ""); err != nil {
		t.Fatalf("the local-apply profile was refused: %v", err)
	}
}
