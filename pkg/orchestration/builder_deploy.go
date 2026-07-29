package orchestration

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/deployments"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"google.golang.org/protobuf/proto"
)

func (b *Builder) Deploy(ctx context.Context) (*OutputProperty, error) {
	w := wool.Get(ctx).In("Builder", wool.ThisField(b.instance))
	w.Debug("Handle")

	env, err := b.world.Env.Proto()
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	conf, err := b.world.ConfigurationManager.GetServiceConfiguration(ctx, b.instance.Identity)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get ConfigurationManager information")
	}

	dependenciesConfigurations, err := b.world.SharedState.GetDependentConfigurationsFor(ctx, b.instance.Identity)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get configuration")
	}

	networkMappings, err := b.world.RemoteNetworkManager.GenerateNetworkMappings(ctx, b.world.Env, b.world.Workspace, b.instance.Identity, b.endpoints)
	if err != nil {
		return nil, w.Wrapf(err, "cannot generate network mappings for service endpoints")
	}

	err = b.world.SharedState.RecordNetworkMappings(ctx, b.instance.Service, networkMappings)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record network mappings")
	}

	dependenciesNetworkMappings, err := b.world.SharedState.GetDependenciesNetworkMappings(ctx, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	namespace, err := b.world.RemoteNetworkManager.GetNamespace(ctx, b.world.Env, b.world.Workspace, b.instance.Identity)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get namespace")
	}

	// Build the request
	dockerContext, err := builder.DockerBuildContext(ctx, b.world.Workspace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create build context")
	}

	deploy, err := deployments.GetKubernetesDeployment(ctx, dockerContext, b.world.Workspace, b.instance.Module, b.instance.Service, b.world.Env, namespace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	if b.world.DeploymentDestination != nil {
		deploy.GetKubernetes().Destination = b.world.DeploymentDestination(b.instance.Module, b.instance.Service)
	}
	profile := kubernetesOutputProfile(b.world)
	deploy.GetKubernetes().Profile = profile
	deploy.GetKubernetes().ValidateServerSide =
		profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 &&
			b.world.Env.IsK3d()
	validationContext := ""
	if profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		if deploy.GetKubernetes().GetValidateServerSide() {
			kubeconfig, contextName, targetErr := kubernetesValidationTarget(ctx, b.world.Env)
			if targetErr != nil {
				return nil, w.Wrapf(targetErr, "cannot resolve promotable GitOps validation target")
			}
			deploy.GetKubernetes().ValidationKubeconfig = kubeconfig
			deploy.GetKubernetes().ValidationContext = contextName
			validationContext = contextName
		}
		conf, dependenciesConfigurations, deploy.GetKubernetes().SecretReferences, err =
			promotableDeploymentInputs(b.instance.Service.Name, conf, dependenciesConfigurations)
		if err != nil {
			return nil, w.Wrapf(err, "cannot prepare promotable GitOps inputs")
		}
	}

	// Build the request
	w.Debug("deployments", wool.Field("deployments", deploy))

	resp, err := b.instance.Builder.Deploy(ctx, &builderv0.DeploymentRequest{
		Environment:                 env,
		Deployment:                  deploy,
		Configuration:               conf,
		DependenciesConfigurations:  dependenciesConfigurations,
		NetworkMappings:             networkMappings,
		DependenciesNetworkMappings: dependenciesNetworkMappings,
	})
	if err != nil {
		return nil, w.Wrapf(err, "cannot deploy service instance")
	}

	if resp.State != nil && resp.State.State != builderv0.DeploymentStatus_SUCCESS {
		return nil, w.NewError("cant deploy service instance")
	}
	if err = validateKubernetesDeploymentOutput(resp.GetDeployment(), profile, validationContext); err != nil {
		return nil, w.Wrapf(err, "cannot accept Kubernetes deployment output")
	}

	err = b.world.ConfigurationManager.ExposeConfiguration(ctx, b.instance.Identity, resp.Configuration)
	if err != nil {
		return nil, w.Wrapf(err, "cannot record shared configuration configurations")
	}

	err = b.outputPropertyForSync.Set(ctx, &BuilderSyncOutput{})
	if err != nil {
		return nil, w.Wrapf(err, "cannot set outputProperty for deploy")
	}

	outputProperty, err := b.outputPropertyForSync.Process(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty for deploy")
	}

	if resp.Deployment == nil {
		return outputProperty, nil
	}
	// Render-only mode: caller (cli/cmd/deploy) skipped wiring a
	// deployment manager so manifests get written to disk by the
	// agent's KustomizeDeploy but no kubectl apply runs. Used by
	// the gitops flow where ArgoCD picks up the rendered tree.
	if b.world.RemoteManager == nil {
		return outputProperty, nil
	}
	err = b.world.RemoteManager.Handle(ctx, b.instance.Service, b.instance.Module, resp.Deployment)
	if err != nil {
		return nil, w.Wrapf(err, "cannot handle deployment")
	}
	return outputProperty, nil
}

func kubernetesValidationTarget(ctx context.Context, environment *resources.Environment) (string, string, error) {
	if environment == nil || environment.Cluster == nil {
		return "", "", fmt.Errorf("environment must declare a Kubernetes cluster")
	}
	if environment.Cluster.Context == "" {
		return "", "", fmt.Errorf("environment %q must declare cluster.context", environment.Name)
	}
	kubeconfig, err := deployments.GetK8sConfig(ctx, environment)
	if err != nil {
		return "", "", err
	}
	if len(filepath.SplitList(kubeconfig)) != 1 {
		return "", "", fmt.Errorf("environment %q must declare exactly one kubeconfig, got %q", environment.Name, kubeconfig)
	}
	kubeconfig, err = filepath.Abs(kubeconfig)
	if err != nil {
		return "", "", fmt.Errorf("resolve kubeconfig %q: %w", kubeconfig, err)
	}
	return kubeconfig, environment.Cluster.Context, nil
}

func kubernetesOutputProfile(world *World) builderv0.KubernetesOutputProfile {
	if world.KubernetesOutputProfile != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_UNSPECIFIED {
		return world.KubernetesOutputProfile
	}
	if world.Env.IsK3d() {
		return builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1
	}
	return builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1
}

func promotableDeploymentInputs(
	secretName string,
	configuration *basev0.Configuration,
	dependencies []*basev0.Configuration,
) (*basev0.Configuration, []*basev0.Configuration, map[string]*builderv0.KubernetesSecretKeyReference, error) {
	references := make(map[string]*builderv0.KubernetesSecretKeyReference)
	sanitized, err := sanitizePromotableConfiguration(secretName, configuration, references)
	if err != nil {
		return nil, nil, nil, err
	}
	sanitizedDependencies := make([]*basev0.Configuration, len(dependencies))
	for index, dependency := range dependencies {
		sanitizedDependencies[index], err = sanitizePromotableConfiguration(secretName, dependency, references)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return sanitized, sanitizedDependencies, references, nil
}

func sanitizePromotableConfiguration(
	secretName string,
	configuration *basev0.Configuration,
	references map[string]*builderv0.KubernetesSecretKeyReference,
) (*basev0.Configuration, error) {
	if configuration == nil {
		return nil, nil
	}
	sanitized := proto.Clone(configuration).(*basev0.Configuration)
	for _, information := range sanitized.GetInfos() {
		if information.GetData().GetSecret() {
			return nil, fmt.Errorf("secret configuration data %q has no Kubernetes Secret key reference", information.GetName())
		}
		values := information.GetConfigurationValues()
		kept := values[:0]
		for _, value := range values {
			if value.GetSecret() || resources.IsSensitiveKey(value.GetKey()) {
				value.Secret = true
				continue
			}
			kept = append(kept, value)
		}
		information.ConfigurationValues = kept
	}

	referenceSource := proto.Clone(configuration).(*basev0.Configuration)
	for _, information := range referenceSource.GetInfos() {
		for _, value := range information.GetConfigurationValues() {
			value.Secret = value.GetSecret() || resources.IsSensitiveKey(value.GetKey())
		}
	}
	for _, environmentVariable := range resources.ConfigurationAsEnvironmentVariables(referenceSource, true) {
		references[environmentVariable.Key] = &builderv0.KubernetesSecretKeyReference{
			Name: secretName + "-secrets",
			Key:  environmentVariable.Key,
		}
	}
	return sanitized, nil
}

func validateKubernetesDeploymentOutput(
	deployment *builderv0.DeploymentOutput,
	profile builderv0.KubernetesOutputProfile,
	validationContext string,
) error {
	kubernetes := deployment.GetKubernetes()
	if kubernetes == nil {
		return fmt.Errorf("builder returned no Kubernetes deployment output")
	}
	if kubernetes.GetProfile() != profile {
		return fmt.Errorf("builder returned profile %s for requested profile %s", kubernetes.GetProfile(), profile)
	}
	if kubernetes.GetContractVersion() == "" {
		return fmt.Errorf("builder returned no Kubernetes contract version")
	}
	validation := kubernetes.GetValidation()
	if validation.GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		return fmt.Errorf("builder did not pass static Kubernetes validation")
	}
	if profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		if validationContext != "" {
			if validation.GetServerSideValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
				return fmt.Errorf("builder did not pass server-side Kubernetes validation")
			}
			if validation.GetValidatedContext() != validationContext {
				return fmt.Errorf(
					"builder validated Kubernetes context %q for requested context %q",
					validation.GetValidatedContext(),
					validationContext,
				)
			}
		}
		if !validation.GetPromotable() {
			return fmt.Errorf("builder did not return a promotable Kubernetes deployment")
		}
	}
	return nil
}
