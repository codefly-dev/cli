package gitops

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
)

// FrontendArtifactName is the name a release declares its component's
// complete frontend bundle under, as a runtime artifact of the package
// manifest: the one artifact a module manifest's generated frontend.payload names.
const FrontendArtifactName = "frontend"

// stampFrontendPayload generates the manifest's frontend.payload from the
// release's own artifact metadata, as core asks of a renderer: the runtime
// artifact named frontend gives the payload its artifact name, digest and
// media type, verbatim. A release declaring no such artifact gives a frontend
// no payload — a generation whose bundle is not published, which core admits
// and the host decides whether to serve. The renderer invents nothing: a
// bundle declared under another purpose, without the explicit media type the
// payload carries, or by a headless component (a bundle is the executable
// output of a declared page) is refused by name. That a release declares the
// name once is the manifest loader's rule.
func stampFrontendPayload(instance *ModuleInstance, artifacts []composition.ReleaseArtifact) error {
	index := slices.IndexFunc(artifacts, func(artifact composition.ReleaseArtifact) bool { return artifact.Name == FrontendArtifactName })
	if index < 0 {
		return nil
	}
	bundle := &artifacts[index]
	switch {
	case bundle.Purpose != composition.ArtifactRuntime:
		return fmt.Errorf("%s %s declares the release artifact %q with purpose %q; a frontend bundle is a runtime artifact", instance.Kind, instance.Name, FrontendArtifactName, bundle.Purpose)
	case instance.Frontend == nil:
		return fmt.Errorf("%s %s declares the release artifact %q and no frontend; a bundle is the output of a declared page", instance.Kind, instance.Name, FrontendArtifactName)
	case bundle.MediaType == "":
		return fmt.Errorf("%s %s declares the release artifact %q without a media type; the frontend payload carries the explicit canonical media type the release declares", instance.Kind, instance.Name, FrontendArtifactName)
	}
	instance.Frontend.Payload = &solutionhost.FrontendPayload{Artifact: bundle.Name, Digest: solutionhost.PayloadDigest(bundle.Digest), MediaType: bundle.MediaType}
	return nil
}

// FrontendBundleRepository is the repository, under the environment's image
// repository for the workspace (beside <module>/<service>), that a render
// publishes every component's frontend bundle to: one repository, so a host
// reads every bundle from the one source it is handed (ArtifactCacheRepositoryKey).
const FrontendBundleRepository = "frontend-bundles"

// publishesFrontendBundles reports whether a render for env publishes
// frontend bundles: a deployed environment's, never a local one's nor a local
// k3d cluster's, whose registry the host's Pods cannot read and the artifact
// client does not speak (it is HTTPS only). Those render no REPOSITORY either.
func publishesFrontendBundles(env *environments.Environment) bool {
	return !env.Local() && !env.DeclaresLocalK3d()
}

// frontendBundleRepositoryOf is where a render of a workspace's modules for
// env publishes frontend bundles: under the workspace's segment of the
// registry, beside <workspace>/<module>/<service> where a modules-layout
// workspace's images go.
func frontendBundleRepositoryOf(env *environments.Environment, workspace string) (string, error) {
	if env.Registry == nil || strings.TrimSpace(env.Registry.URL) == "" {
		return "", fmt.Errorf("environment %s declares no registry.url, so a frontend bundle has nowhere to be published", env.Name)
	}
	// A bundle is not an image a mirror is proven to serve, so registry.artifacts,
	// when declared, is where it is both published and read.
	base := env.Registry.URL
	if env.Registry.Artifacts != "" {
		base = env.Registry.Artifacts
	}
	return strings.TrimSuffix(base, "/") + "/" + workspace + "/" + FrontendBundleRepository, nil
}

// artifactCacheInjection projects the artifact-cache group for a service that
// declares it in a deployed render: REPOSITORY, the one repository this
// render publishes frontend bundles to, which a host's registry source reads
// a bundle from by the digest a signed manifest names. A local render projects
// nothing; there the native run hands the cache ROOT instead.
func artifactCacheInjection(service *resources.Service, env *environments.Environment, scope unitScope, injection serviceInjection) (serviceInjection, error) {
	if !publishesFrontendBundles(env) || !slices.Contains(service.WorkspaceConfigurationDependencies, environments.ArtifactCacheConfiguration) {
		return injection, nil
	}
	repository, err := frontendBundleRepositoryOf(env, scope.Workspace)
	if err != nil {
		return serviceInjection{}, err
	}
	configuration := &basev0.Configuration{Origin: resources.ConfigurationWorkspace}
	key := resources.ConfigurationValueEnvironmentKey(configuration, environments.ArtifactCacheConfiguration,
		&basev0.ConfigurationValue{Key: environments.ArtifactCacheRepositoryKey})
	if _, exists := injection.Public[key]; exists {
		return serviceInjection{}, fmt.Errorf("service %s already derives render-owned key %s", resources.ServiceUnique(scope.Module, service.Name), key)
	}
	public := map[string]string{key: repository}
	maps.Copy(public, injection.Public)
	return serviceInjection{Public: public, Secrets: injection.Secrets}, nil
}

// buildFrontendBundle and publishRuntimeArtifact are the build and the push a
// test stands in for, the way stubBuild stands in for an image build.
var (
	buildFrontendBundle = func(ctx context.Context, module *resources.Module, env *environments.Environment, destination string) error {
		builder, err := orchestration.EnsureEnvironmentBuilder(ctx, env)
		if err != nil {
			return err
		}
		return solutionrun.BuildFrontendBundleWithBuilder(ctx, module, destination, builder)
	}
	publishRuntimeArtifact = clicomposition.PublishRuntimeArtifact
)

// publishFrontendBundle gives a deployed render of a component that declares
// a page the frontend release artifact its payload is stamped from: the
// render builds the bundle from the release's own source, as it builds the
// release's images, archives it in core's canonical form and publishes it to
// the environment's bundle repository by digest. A host fetches those bytes by
// the digest the signed manifest names and verifies them before serving.
//
// A release that declares its own frontend artifact keeps it: the render
// invents nothing over the release's metadata. A local render publishes
// nothing, since a native run builds and retains its bundle itself.
func publishFrontendBundle(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment, artifacts []composition.ReleaseArtifact) ([]composition.ReleaseArtifact, error) {
	if !publishesFrontendBundles(env) || slices.ContainsFunc(artifacts, func(artifact composition.ReleaseArtifact) bool { return artifact.Name == FrontendArtifactName }) {
		return artifacts, nil
	}
	frontend, err := solutionrun.DeclaredFrontend(ctx, module)
	if err != nil || frontend == nil {
		return artifacts, err
	}
	repository, err := frontendBundleRepositoryOf(env, workspace.Name)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp("", "codefly-frontend-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	bundle := filepath.Join(scratch, "bundle")
	if err = buildFrontendBundle(ctx, module, env, bundle); err != nil {
		return nil, err
	}
	archive, _, err := composition.CanonicalArchive(bundle)
	if err != nil {
		return nil, fmt.Errorf("archive the frontend bundle of %s: %w", module.Name, err)
	}
	artifact, err := publishRuntimeArtifact(ctx, repository, FrontendArtifactName, clicomposition.CanonicalArchiveMediaType, archive, nil)
	if err != nil {
		return nil, fmt.Errorf("publish the frontend bundle of %s to %s: %w", module.Name, repository, err)
	}
	return append(slices.Clone(artifacts), artifact), nil
}
