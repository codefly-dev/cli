package orchestration

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/deployments"
	"github.com/codefly-dev/cli/pkg/environments"
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

	dependenciesNetworkMappings, err := b.world.SharedState.GetDependenciesNetworkMappings(ctx, b.instance.Service)
	if err != nil {
		return nil, w.Wrapf(err, "cannot load service instance")
	}

	// A deployed service reaches its dependencies inside the cluster, so its
	// ${endpoint:…} references resolve to their in-cluster addresses: its
	// dependencies' and those of every producer its declared groups reference
	// (referencedProducerMappings), derived as their own deploy records them.
	referenced, err := b.world.referencedProducerMappings(ctx, b.instance.Service,
		b.instance.Service.WorkspaceConfigurationDependencies, dependenciesNetworkMappings)
	if err != nil {
		return nil, w.Wrap(err)
	}
	consumerMappings := append(slices.Clone(dependenciesNetworkMappings), referenced...)
	workspaceConfigurations, err := b.world.ConfigurationManager.
		ForConsumer(consumerMappings, resources.NewContainerNetworkAccess()).
		GetWorkspaceDependenciesConfigurations(ctx, b.instance.Service.WorkspaceConfigurationDependencies...)
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
	validation, err := resolveClusterValidation(ctx, b.world, profile, namespace)
	if err != nil {
		return nil, w.Wrapf(err, "cannot resolve promotable GitOps validation target")
	}
	if validation.Skipped != "" {
		w.Warn(validation.Skipped)
		if b.world.OutputSink != nil {
			b.world.OutputSink.Info("%s", validation.Skipped)
		}
	}
	deploy.GetKubernetes().ValidateServerSide = validation.Enabled()
	deploy.GetKubernetes().ValidationKubeconfig = validation.Kubeconfig
	deploy.GetKubernetes().ValidationContext = validation.Context
	validationContext := validation.Context

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

// remoteProducerMappings derives a producer's mappings as its own deploy records
// them (Deploy), for a consumer deployed before it. A deployed address is a pure
// function of the producer's identity and namespace, so it holds whether or not
// this operation deploys that producer.
func (world *World) remoteProducerMappings(ctx context.Context, identity *resources.ServiceIdentity, endpoints []*basev0.Endpoint) ([]*basev0.NetworkMapping, error) {
	if world.RemoteNetworkManager == nil {
		return nil, nil
	}
	mappings, err := world.RemoteNetworkManager.GenerateNetworkMappings(ctx, world.Env, world.Workspace, identity, endpoints)
	if err != nil {
		return nil, err
	}
	return withContainerReachableAsPublic(ctx, mappings), nil
}

// clusterValidation is the outcome of deciding whether a promotable deploy asks
// the agent for a server-side dry-run: the exact target when it does, or the
// reason it does not although the caller opted in.
type clusterValidation struct {
	Kubeconfig string
	Context    string
	// Skipped explains why an opted-in validation cannot run against this
	// cluster. It is surfaced to the caller rather than silently dropped.
	Skipped string
}

// Enabled reports whether the deploy request carries a server-side validation
// target.
func (v clusterValidation) Enabled() bool {
	return v.Context != ""
}

// NamespaceProbe reports whether a namespace exists in the cluster a kubeconfig
// context selects. It is the one cluster read a render performs, so a World
// can substitute it.
type NamespaceProbe func(ctx context.Context, kubeconfig, kubeContext, namespace string) (bool, error)

// resolveClusterValidation decides the server-side validation a promotable
// deploy requests. A render is a pure function of the workspace: it must not
// need a cluster, so the dry-run is opt-in (World.ValidateCluster, set by
// `deploy gitops render --validate-cluster`) and off by default. Direct apply
// paths are untouched — they never request a promotable profile, so the
// decision here does not reach them.
//
// When opted in, the environment must declare cluster.context, and the
// namespace the manifests bind to must already exist: core runs the dry-run
// with `kubectl apply --namespace <ns>`, and a server-side dry-run of namespaced
// objects fails on a namespace that is not there, while the rendered Argo
// Application does not create it (CreateNamespace=false). Rather than fail a
// render on that gap, the validation is skipped with a clear reason.
func resolveClusterValidation(
	ctx context.Context,
	world *World,
	profile builderv0.KubernetesOutputProfile,
	namespace string,
) (clusterValidation, error) {
	// Only a restricted (promotable) profile is ever validated against a
	// cluster: an ephemeral local apply is about to be applied for real.
	if world == nil || !world.ValidateCluster || !coreservices.IsRestrictedOutputProfile(profile) {
		return clusterValidation{}, nil
	}
	kubeconfig, contextName, err := kubernetesValidationTarget(ctx, world.Env)
	if err != nil {
		return clusterValidation{}, err
	}
	probe := world.NamespaceProbe
	if probe == nil {
		probe = kubectlNamespaceExists
	}
	exists, err := probe(ctx, kubeconfig, contextName, namespace)
	if err != nil {
		return clusterValidation{}, fmt.Errorf("cannot inspect namespace %q in context %q: %w", namespace, contextName, err)
	}
	if !exists {
		return clusterValidation{Skipped: fmt.Sprintf(
			"skipping server-side validation: namespace %q does not exist in context %q; "+
				"the rendered Argo Application does not create it (CreateNamespace=false), "+
				"so create the namespace first to validate this render against the cluster",
			namespace, contextName,
		)}, nil
	}
	return clusterValidation{Kubeconfig: kubeconfig, Context: contextName}, nil
}

// kubectlNamespaceExists is the default NamespaceProbe: a read-only
// `kubectl get namespace` against the explicit validation target.
func kubectlNamespaceExists(ctx context.Context, kubeconfig, kubeContext, namespace string) (bool, error) {
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		return false, fmt.Errorf("cluster validation requires kubectl: %w", err)
	}
	output, err := exec.CommandContext(
		ctx,
		kubectl,
		"--kubeconfig", kubeconfig,
		"--context", kubeContext,
		"get", "namespace", namespace,
		"-o", "name",
	).CombinedOutput()
	return namespaceProbeResult(output, err)
}

// namespaceProbeResult interprets a `kubectl get namespace` outcome: success
// means the namespace exists, a NotFound error means it does not, and any other
// failure (unreachable cluster, bad context) is an error the caller must see.
func namespaceProbeResult(output []byte, err error) (bool, error) {
	if err == nil {
		return true, nil
	}
	text := strings.TrimSpace(string(output))
	if strings.Contains(text, "NotFound") || strings.Contains(text, "not found") {
		return false, nil
	}
	return false, fmt.Errorf("%w: %s", err, text)
}

func kubernetesValidationTarget(ctx context.Context, environment *environments.Environment) (string, string, error) {
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
	if misplaced := misplacedSecretKeys(append([]*basev0.Configuration{configuration}, dependencies...)); len(misplaced) > 0 {
		return nil, nil, nil, fmt.Errorf(
			"restricted rendering rejects credential-named keys carrying a plaintext value; declare each as a secret (secretKeyRef) instead of inline config: %s",
			strings.Join(misplaced, ", "),
		)
	}
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

// restrictedRenderRejects reports whether restricted render will refuse a value
// that reaches it inline. It mirrors the per-value half of core's
// validateRestrictedDeploymentRequest guard (core agents/services/base_builder.go):
// a non-empty value carrying an explicit Secret flag or a credential-named key.
// Both sides key off resources.IsSensitiveKey, so core stays the single source of
// truth for the marker list and this is the one place that tracks the guard's
// shape. The Secret clause never decides a misplacement (Secret values promote to
// references first, below) but is kept so the predicate is a faithful standalone
// mirror rather than one narrowed to its current caller.
func restrictedRenderRejects(value *basev0.ConfigurationValue) bool {
	return value.GetValue() != "" && (value.GetSecret() || resources.IsSensitiveKey(value.GetKey()))
}

// misplacedSecretKeys lists the credential-named plaintext values that would be
// forwarded inline to restricted render and rejected there one key per run. A
// value promoted to a secretKeyRef never reaches core inline, so a key is
// misplaced only when it both survives promotion and restrictedRenderRejects.
// Each entry names the key, the configuration scope it came from (so a value
// exposed by a dependency, not authored in a local *.env file, is still
// traceable), and the *.secret.env convention that resolves it.
func misplacedSecretKeys(configurations []*basev0.Configuration) []string {
	var misplaced []string
	for _, configuration := range configurations {
		scope := configuration.GetOrigin()
		if scope == resources.ConfigurationWorkspace {
			scope = "workspace"
		}
		for _, info := range configuration.GetInfos() {
			for _, value := range info.GetConfigurationValues() {
				if promotesToDeploymentSecret(value) || !restrictedRenderRejects(value) {
					continue
				}
				misplaced = append(misplaced, fmt.Sprintf(
					"%s in %s (declare it as a secret, e.g. %s.secret.env)",
					value.GetKey(), scope, info.GetName(),
				))
			}
		}
	}
	return misplaced
}

// endpointKeySuffixes name the OIDC identity endpoint keys — authorize/token
// URLs and selectors — whose only credential signal is an AUTH or TOKEN
// marker. Core decides which of them are sensitive at all: since core v0.5.6
// (codefly-dev/core#640) AUTH is matched per word, so a public word such as
// AUTHORIZE or AUTHORITY no longer marks IDENTITY_AUTHORIZE_URL, while a TOKEN
// endpoint (IDENTITY_TOKEN_URL) and every other AUTH word (OAUTH_*_URL,
// NEXTAUTH_URL, AUTHORIZATION_URL) still do. The ones core still classifies are
// secret-classified here: restricted render requires them in *.secret.env
// (rendered as secretKeyRefs), so a plaintext value under one of these names
// is a misplacement, not plain routing config.
var endpointKeySuffixes = []string{"_URL", "_SELECTOR"}

// endpointMarkerStripper removes the AUTH and TOKEN markers so the suffix
// classifier can ask core whether anything else in the name is a credential.
// AUTH is still stripped although core now matches it per word: OAUTH_*_URL and
// NEXTAUTH_URL remain sensitive in core solely through their AUTH word, and
// must stay on the misplacement path rather than the connection-string one.
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
// (…_URL/…_SELECTOR) that core classifies as sensitive solely through its
// AUTH/TOKEN markers. A key core does not classify at all is never one: the CLI
// only narrows within core's classification, never widens or second-guesses it.
// Stripping the markers and deferring to IsSensitiveKey means a real credential
// marker — including ones core adds later — still takes the connection-string
// promotion path without this package tracking core's list.
func isSecretEndpointKey(key string) bool {
	if !resources.IsSensitiveKey(key) {
		return false
	}
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
			// Flat secret keys have no environment scope; structured secrets are
			// rejected above until a typed Kubernetes reference can carry them.
			environmentVariables, err := resources.ConfigurationAsEnvironmentVariables(secretConfiguration, "", true)
			if err != nil {
				return nil, err
			}
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
