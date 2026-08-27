package orchestration

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

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

	workspaceConfigurations, err := b.world.ConfigurationManager.GetWorkspaceDependenciesConfigurations(
		ctx,
		b.instance.Service.WorkspaceConfigurationDependencies...,
	)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get workspace configurations")
	}

	dependenciesConfigurations, err := b.world.SharedState.GetDependentConfigurationsFor(ctx, b.instance.Identity)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get configuration")
	}
	dependenciesConfigurations = append(workspaceConfigurations, dependenciesConfigurations...)
	profile := kubernetesOutputProfile(b.world)
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
	networkMappings = withContainerReachableAsPublic(ctx, networkMappings)

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

	dockerContext, err := builder.DockerBuildContext(ctx, b.world.Workspace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot create build context")
	}
	dockerContext.ImageDigest = b.imageDigest

	deploy, err := deployments.GetKubernetesDeployment(
		ctx,
		dockerContext,
		b.world.Workspace,
		b.instance.Module,
		b.instance.Service,
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
	deploy.GetKubernetes().ValidateServerSide =
		profile == builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 &&
			b.world.Env.IsK3d()
	validationContext := ""
	if deploy.GetKubernetes().GetValidateServerSide() {
		kubeconfig, contextName, targetErr := kubernetesValidationTarget(ctx, b.world.Env)
		if targetErr != nil {
			return nil, w.Wrapf(targetErr, "cannot resolve promotable GitOps validation target")
		}
		deploy.GetKubernetes().ValidationKubeconfig = kubeconfig
		deploy.GetKubernetes().ValidationContext = contextName
		validationContext = contextName
	}

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
	if err := validateDeploymentOutput(
		b.world.RemoteManager,
		profile,
		resp.Deployment,
		validationContext,
	); err != nil {
		return nil, w.Wrapf(err, "cannot verify service deployment output")
	}
	if resp.Deployment != nil {
		b.deploymentOutput = proto.Clone(resp.Deployment).(*builderv0.DeploymentOutput)
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

	if resp.Deployment == nil || b.world.RemoteManager == nil {
		return outputProperty, nil
	}
	err = b.world.RemoteManager.Handle(ctx, b.instance.Service, b.instance.Module, resp.Deployment)
	if err != nil {
		return nil, w.Wrapf(err, "cannot handle deployment")
	}
	return outputProperty, nil
}

// withContainerReachableAsPublic normalizes a service's remote network mappings
// so an endpoint reachable only in-cluster still resolves for anyone that injects
// the address using public access.
//
// On a non-local deploy an endpoint without an ingress (e.g. a module-visibility
// vault) only materializes an in-cluster (Container) instance — unlike the
// DNS-backed path, which already emits both a Public and a Container instance. A
// builder requests the public access variant when wiring the address, so with no
// public instance present resolution fails. In cluster the ClusterIP address is
// reachable regardless of access label, so mirror the Container instance as a
// Public one. This runs at generation time, before the mappings are recorded, so
// the owning service's own deploy request and every downstream consumer see the
// mirror from a single normalization point.
//
// The gate is the container/public instance shape, not endpoint visibility: the
// access kind a consumer requests is decided by the out-of-process builder agent,
// so gating on visibility here would guess that agent's behavior and risk
// under-covering endpoints it also resolves as public. The added instance is only
// ever matched by a caller that explicitly asks for public access.
//
// Mappings that need a mirror are cloned, so the input slice's instances are
// never mutated.
func withContainerReachableAsPublic(ctx context.Context, mappings []*basev0.NetworkMapping) []*basev0.NetworkMapping {
	out := make([]*basev0.NetworkMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if mapping == nil {
			out = append(out, mapping)
			continue
		}
		if resources.FilterNetworkInstance(ctx, mapping.Instances, resources.NewPublicNetworkAccess()) != nil {
			out = append(out, mapping)
			continue
		}
		container := resources.FilterNetworkInstance(ctx, mapping.Instances, resources.NewContainerNetworkAccess())
		if container == nil {
			out = append(out, mapping)
			continue
		}
		clone := proto.CloneOf(mapping)
		public := proto.CloneOf(container)
		public.Access = resources.NewPublicNetworkAccess()
		clone.Instances = append(clone.Instances, public)
		out = append(out, clone)
	}
	return out
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
	return deployments.KubernetesOutputProfile(world.RemoteManager)
}

func promotableDeploymentConfigurations(
	configuration *basev0.Configuration,
	dependencies []*basev0.Configuration,
	secretName string,
) (*basev0.Configuration, []*basev0.Configuration, map[string]*builderv0.KubernetesSecretKeyReference, error) {
	references := map[string]*builderv0.KubernetesSecretKeyReference{}
	var misplaced []string
	own, err := promotableConfiguration(configuration, secretName, references, &misplaced)
	if err != nil {
		return nil, nil, nil, err
	}
	safeDependencies := make([]*basev0.Configuration, 0, len(dependencies))
	for _, dependency := range dependencies {
		safe, err := promotableConfiguration(dependency, secretName, references, &misplaced)
		if err != nil {
			return nil, nil, nil, err
		}
		safeDependencies = append(safeDependencies, safe)
	}
	if len(misplaced) > 0 {
		return nil, nil, nil, fmt.Errorf(
			"restricted rendering received secret-classified keys as plaintext config; move each to its *.secret.env so it renders as a secretKeyRef: %s",
			strings.Join(misplaced, ", "),
		)
	}
	return own, safeDependencies, references, nil
}

// endpointKeySuffixes name the OIDC identity endpoint keys — authorize/token
// URLs and the authorize selector — whose only credential signal is the broad
// AUTH/TOKEN substring markers. They are secret-classified: restricted render
// requires them in *.secret.env (rendered as secretKeyRefs), so a plaintext
// value under one of these names is a misplacement, not plain routing config.
var endpointKeySuffixes = []string{"_URL", "_SELECTOR"}

// endpointMarkerStripper removes the broad substring markers that routinely
// appear in OAuth authorize/token endpoint names. AUTH and TOKEN are the only
// markers the suffix classifier keys on; anything else IsSensitiveKey reacts to
// names a real credential the connection-string safety net promotes on its own.
var endpointMarkerStripper = strings.NewReplacer("AUTH", "", "TOKEN", "")

var keyCanonicalizer = strings.NewReplacer(" ", "_", "-", "_", ".", "_", "/", "_")

// promotesToDeploymentSecret reports whether a config value must be pulled into
// a Kubernetes secret reference. An explicit Secret flag is authoritative; the
// name heuristic is a safety net for un-flagged credentials, minus the identity
// endpoint keys, which restricted render handles as a misplacement error rather
// than silently promoting a plaintext value into a secretKeyRef whose backing
// secret was never seeded.
func promotesToDeploymentSecret(value *basev0.ConfigurationValue) bool {
	if value.GetSecret() {
		return true
	}
	if !resources.IsSensitiveKey(value.GetKey()) {
		return false
	}
	return !isSecretEndpointKey(value.GetKey())
}

// isSecretEndpointKey reports whether a key is an OIDC identity endpoint key
// (…_URL/…_SELECTOR) whose sensitivity comes solely from the broad AUTH/TOKEN
// markers. It strips those markers and defers to IsSensitiveKey, so a real
// credential marker — including ones core adds later — still takes the
// connection-string promotion path without this package tracking core's list.
func isSecretEndpointKey(key string) bool {
	canonical := keyCanonicalizer.Replace(strings.ToUpper(key))
	hasEndpointSuffix := false
	for _, suffix := range endpointKeySuffixes {
		if strings.HasSuffix(canonical, suffix) {
			hasEndpointSuffix = true
			break
		}
	}
	if !hasEndpointSuffix {
		return false
	}
	return !resources.IsSensitiveKey(endpointMarkerStripper.Replace(canonical))
}

func promotableConfiguration(
	configuration *basev0.Configuration,
	secretName string,
	references map[string]*builderv0.KubernetesSecretKeyReference,
	misplaced *[]string,
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
			if !promotesToDeploymentSecret(sourceValue) {
				// A plaintext value under a secret-classified key is exactly what
				// core's restricted-render guard rejects, one key per run. Collect
				// every such key here so the operator gets a single actionable
				// error naming the *.secret.env each belongs in.
				if sourceValue.GetValue() != "" && resources.IsSensitiveKey(sourceValue.GetKey()) {
					*misplaced = append(*misplaced, fmt.Sprintf("%s (%s.secret.env)", sourceValue.GetKey(), sourceInfo.GetName()))
					continue
				}
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
	validationContext string,
) error {
	kubernetes := output.GetKubernetes()
	if kubernetes == nil {
		return fmt.Errorf("plugin returned no Kubernetes deployment output")
	}
	if kubernetes.GetProfile() != requested {
		return fmt.Errorf("plugin returned Kubernetes output profile %s, requested %s", kubernetes.GetProfile(), requested)
	}
	if requested != builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_PROMOTABLE_GITOPS_V1 {
		return nil
	}
	if kubernetes.GetContractVersion() != coreservices.KubernetesManifestContractVersion {
		return fmt.Errorf(
			"plugin returned Kubernetes manifest contract %q, expected %q",
			kubernetes.GetContractVersion(),
			coreservices.KubernetesManifestContractVersion,
		)
	}
	validation := kubernetes.GetValidation()
	if !validation.GetPromotable() ||
		validation.GetStaticValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
		return fmt.Errorf("plugin did not return a successfully validated promotable Kubernetes output")
	}
	if validationContext != "" {
		if validation.GetServerSideValidation() != builderv0.KubernetesManifestValidation_STATUS_PASSED {
			return fmt.Errorf("plugin did not pass server-side Kubernetes validation")
		}
		if validation.GetValidatedContext() != validationContext {
			return fmt.Errorf(
				"plugin validated Kubernetes context %q, requested %q",
				validation.GetValidatedContext(),
				validationContext,
			)
		}
	}
	return nil
}

func validateDeploymentOutput(
	manager deployments.Manager,
	requested builderv0.KubernetesOutputProfile,
	output *builderv0.DeploymentOutput,
	validationContext string,
) error {
	if output == nil {
		if deployments.RequiresDeploymentOutput(manager) {
			return fmt.Errorf("plugin returned no Kubernetes deployment output")
		}
		return nil
	}
	return validateKubernetesDeploymentOutput(requested, output, validationContext)
}
