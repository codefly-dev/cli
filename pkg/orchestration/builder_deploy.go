package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/deployments"
	coreservices "github.com/codefly-dev/core/agents/services"
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
	profile := deployments.KubernetesOutputProfile(b.world.RemoteManager)
	var secretReferences map[string]*builderv0.KubernetesSecretKeyReference
	if profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		secretName := "secret-" + b.instance.Service.Name
		conf, dependenciesConfigurations, secretReferences, err = promotableDeploymentConfigurations(
			conf,
			dependenciesConfigurations,
			secretName,
		)
		if err != nil {
			return nil, w.Wrapf(err, "cannot prepare promotable configuration")
		}
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

	deploy, err := deployments.GetKubernetesDeployment(
		ctx,
		dockerContext,
		b.world.Workspace,
		b.instance.Module,
		b.instance.Service,
		b.world.Env,
		namespace,
		profile,
		secretReferences,
	)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}
	if b.world.DeploymentDestination != nil {
		deploy.GetKubernetes().Destination = b.world.DeploymentDestination(b.instance.Module, b.instance.Service)
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
	if err := validateKubernetesDeploymentOutput(profile, resp.GetDeployment()); err != nil {
		return nil, w.Wrapf(err, "cannot verify service deployment output")
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

func promotableDeploymentConfigurations(
	configuration *basev0.Configuration,
	dependencies []*basev0.Configuration,
	secretName string,
) (*basev0.Configuration, []*basev0.Configuration, map[string]*builderv0.KubernetesSecretKeyReference, error) {
	references := map[string]*builderv0.KubernetesSecretKeyReference{}
	own, err := promotableConfiguration(configuration, secretName, references)
	if err != nil {
		return nil, nil, nil, err
	}
	safeDependencies := make([]*basev0.Configuration, 0, len(dependencies))
	for _, dependency := range dependencies {
		safe, err := promotableConfiguration(dependency, secretName, references)
		if err != nil {
			return nil, nil, nil, err
		}
		safeDependencies = append(safeDependencies, safe)
	}
	return own, safeDependencies, references, nil
}

func promotableConfiguration(
	configuration *basev0.Configuration,
	secretName string,
	references map[string]*builderv0.KubernetesSecretKeyReference,
) (*basev0.Configuration, error) {
	if configuration == nil {
		return nil, nil
	}
	safe := proto.Clone(configuration).(*basev0.Configuration)
	safe.Infos = safe.Infos[:0]
	for _, sourceInfo := range configuration.GetInfos() {
		if sourceInfo.GetData().GetSecret() {
			return nil, fmt.Errorf("structured secret configuration %q requires typed Kubernetes key references", sourceInfo.GetName())
		}
		info := proto.Clone(sourceInfo).(*basev0.ConfigurationInformation)
		info.ConfigurationValues = info.ConfigurationValues[:0]
		for _, sourceValue := range sourceInfo.GetConfigurationValues() {
			if !sourceValue.GetSecret() && !resources.IsSensitiveKey(sourceValue.GetKey()) {
				info.ConfigurationValues = append(info.ConfigurationValues, proto.Clone(sourceValue).(*basev0.ConfigurationValue))
				continue
			}
			secretConfiguration := &basev0.Configuration{
				Origin: configuration.GetOrigin(),
				Infos: []*basev0.ConfigurationInformation{{
					Name: sourceInfo.GetName(),
					ConfigurationValues: []*basev0.ConfigurationValue{{
						Key: sourceValue.GetKey(), Secret: true,
					}},
				}},
			}
			environmentVariables := resources.ConfigurationAsEnvironmentVariables(secretConfiguration, true)
			if len(environmentVariables) != 1 {
				return nil, fmt.Errorf("secret configuration %q/%q has no environment identity", sourceInfo.GetName(), sourceValue.GetKey())
			}
			key := environmentVariables[0].Key
			references[key] = &builderv0.KubernetesSecretKeyReference{Name: secretName, Key: key}
		}
		if len(info.GetConfigurationValues()) > 0 || info.GetData() != nil {
			safe.Infos = append(safe.Infos, info)
		}
	}
	return safe, nil
}

func validateKubernetesDeploymentOutput(
	requested builderv0.KubernetesOutputProfile,
	output *builderv0.DeploymentOutput,
) error {
	kubernetes := output.GetKubernetes()
	if kubernetes == nil {
		return fmt.Errorf("plugin returned no Kubernetes deployment output")
	}
	if kubernetes.GetProfile() != requested {
		return fmt.Errorf("plugin returned Kubernetes output profile %s, requested %s", kubernetes.GetProfile(), requested)
	}
	if requested == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		if kubernetes.GetContractVersion() != coreservices.KubernetesManifestContractVersion {
			return fmt.Errorf(
				"plugin returned Kubernetes manifest contract %q, expected %q",
				kubernetes.GetContractVersion(),
				coreservices.KubernetesManifestContractVersion,
			)
		}
		validation := kubernetes.GetValidation()
		if !validation.GetPromotable() ||
			validation.GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED ||
			validation.GetServerSideValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
			return fmt.Errorf("plugin did not return a successfully validated promotable Kubernetes output")
		}
	}
	return nil
}
