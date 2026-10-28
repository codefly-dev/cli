package gitops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// stubFrontendPublication stands in for the bundle build and the registry
// push, recording what the render asked of each.
func stubFrontendPublication(t *testing.T) (*int, *string) {
	t.Helper()
	builds, published := 0, ""
	previousBuild, previousPublish := buildFrontendBundle, publishRuntimeArtifact
	t.Cleanup(func() { buildFrontendBundle, publishRuntimeArtifact = previousBuild, previousPublish })
	buildFrontendBundle = func(_ context.Context, _ *resources.Module, _ *environments.Environment, destination string) error {
		builds++
		require.NoError(t, os.MkdirAll(destination, 0o755))
		return os.WriteFile(filepath.Join(destination, solutionrun.FrontendBundleManifest), []byte("{}"), 0o600)
	}
	publishRuntimeArtifact = func(_ context.Context, repository, name, mediaType string, data []byte, _ *http.Client) (composition.ReleaseArtifact, error) {
		published = repository
		require.NotEmpty(t, data)
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
		return composition.ReleaseArtifact{Name: name, Purpose: composition.ArtifactRuntime, URI: "oci://" + repository + "@" + digest, Digest: digest, MediaType: mediaType}, nil
	}
	return &builds, &published
}

func deployedBundleEnvironment(name string) *environments.Environment {
	env := identityEnvironment()
	env.Name = name
	env.Registry = &environments.EnvironmentRegistry{URL: "registry.example/project/repo"}
	return env
}

// A deployed render of a component with a page builds its bundle from the
// release's source, publishes it beside the workspace's images and stamps the
// payload from the artifact it published; a release declaring its own bundle,
// a headless component and a local render build and publish nothing.
func TestADeployedRenderPublishesTheFrontendBundleItStamps(t *testing.T) {
	declared := "release-artifacts:\n  - name: frontend\n    purpose: runtime\n    uri: oci://registry.example/shared/frontend@" + releasedBundleDigest + "\n    digest: " + releasedBundleDigest + "\n    media-type: application/x-tar\n"
	cases := []struct {
		name, env, artifacts string
		headless             bool
		published            bool
		payload              string
	}{
		{name: "a page with no released bundle", env: "staging", published: true},
		{name: "a release declaring its bundle", env: "staging", artifacts: declared, payload: releasedBundleDigest},
		{name: "a headless component", env: "staging", headless: true},
		{name: "a local render", env: "local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			builds, repository := stubFrontendPublication(t)
			module, services := bundleModule(t, tc.artifacts, tc.headless)
			env := deployedBundleEnvironment(tc.env)
			pkg, artifacts, err := modulePackage(module.Dir())
			require.NoError(t, err)
			workspace := &resources.Workspace{Name: "platform", Layout: resources.LayoutKindModules}
			artifacts, err = publishFrontendBundle(t.Context(), workspace, module, env, artifacts)
			require.NoError(t, err)
			instance, _, err := moduleInstanceOf(t.Context(), workspace, module, services, env, pkg, artifacts, nil)
			require.NoError(t, err)
			if !tc.published {
				require.Zero(t, *builds)
				require.Empty(t, *repository)
				if tc.payload != "" {
					require.EqualValues(t, tc.payload, instance.Frontend.Payload.Digest)
				} else if instance.Frontend != nil {
					require.Nil(t, instance.Frontend.Payload)
				}
				return
			}
			require.Equal(t, 1, *builds)
			require.Equal(t, "registry.example/project/repo/platform/"+FrontendBundleRepository, *repository)
			require.NotNil(t, instance.Frontend.Payload)
			require.Equal(t, FrontendArtifactName, instance.Frontend.Payload.Artifact)
			require.Equal(t, clicomposition.CanonicalArchiveMediaType, instance.Frontend.Payload.MediaType)
			require.Regexp(t, `^sha256:[0-9a-f]{64}$`, string(instance.Frontend.Payload.Digest))
		})
	}
}

// A deployed render with no registry has nowhere to publish a page's bundle
// and says so, rather than signing a frontend no host can serve.
func TestADeployedRenderWithoutARegistryRefusesAPage(t *testing.T) {
	stubFrontendPublication(t)
	module, _ := bundleModule(t, "", false)
	env := deployedBundleEnvironment("staging")
	env.Registry = nil
	_, err := publishFrontendBundle(t.Context(), &resources.Workspace{Name: "platform"}, module, env, nil)
	require.ErrorContains(t, err, "declares no registry.url")
}

// The render hands a host the repository it publishes bundles to, through the
// render-owned artifact-cache group the host declares; a local render and a
// service that does not declare the group receive nothing.
func TestArtifactCacheRepositoryReachesTheServicesDeclaringIt(t *testing.T) {
	declaring := &resources.Service{Name: "accounts", WorkspaceConfigurationDependencies: []string{environments.ArtifactCacheConfiguration}}
	env := deployedBundleEnvironment("staging")
	injection, err := artifactCacheInjection(declaring, env, scopeOf(env), serviceInjection{Public: map[string]string{"OTHER": "kept"}})
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"CODEFLY__WORKSPACE_CONFIGURATION__ARTIFACT_CACHE__REPOSITORY": "registry.example/project/repo/platform/" + FrontendBundleRepository,
		"OTHER": "kept",
	}, injection.Public)

	for name, tc := range map[string]struct {
		service *resources.Service
		env     *environments.Environment
	}{
		"a local render":         {declaring, deployedBundleEnvironment("local")},
		"an undeclaring service": {&resources.Service{Name: "api"}, env},
	} {
		t.Run(name, func(t *testing.T) {
			injection, err := artifactCacheInjection(tc.service, tc.env, scopeOf(tc.env), serviceInjection{})
			require.NoError(t, err)
			require.Empty(t, injection.Public)
		})
	}
}

// The render stage publishes and stamps through declareInstances itself: a
// staging render's declared instance carries the payload of the bundle it
// published, with no helper called by hand.
func TestTheRenderStageStampsThePublishedBundle(t *testing.T) {
	builds, repository := stubFrontendPublication(t)
	module, services := bundleModule(t, "", false)
	pkg, artifacts, err := modulePackage(module.Dir())
	require.NoError(t, err)
	render := &moduleRender{
		workspace: &resources.Workspace{Name: "platform", Layout: resources.LayoutKindModules},
		module:    module,
		env:       deployedBundleEnvironment("staging"),
		options:   &RenderOptions{Package: pkg, ReleaseArtifacts: artifacts, Units: []InventoryUnit{{Name: "api", Path: "services/api"}}},
	}
	require.NoError(t, render.declareInstances(t.Context(), services))
	require.Equal(t, 1, *builds)
	require.Equal(t, "registry.example/project/repo/platform/"+FrontendBundleRepository, *repository)
	payload := render.options.Instance.Frontend.Payload
	require.NotNil(t, payload)
	require.Equal(t, render.options.ReleaseArtifacts[len(render.options.ReleaseArtifacts)-1].Digest, string(payload.Digest))
}

// The repository reaches the rendered ConfigMap of a host service through the
// service's own projection, and a local k3d cluster's render projects none.
func TestTheRenderedHostConfigurationCarriesTheBundleRepository(t *testing.T) {
	key := "CODEFLY__WORKSPACE_CONFIGURATION__ARTIFACT_CACHE__REPOSITORY"
	service := &resources.Service{Name: "accounts", WorkspaceConfigurationDependencies: []string{environments.ArtifactCacheConfiguration}}
	env := storeEnvironment()
	env.Registry = &environments.EnvironmentRegistry{URL: "registry.example/project/repo"}
	root := t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, service.Name, "database.example:5432")
	require.NoError(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, root), root, service, env, scopeOf(env), serviceInjection{}))
	require.Equal(t, "registry.example/project/repo/platform/"+FrontendBundleRepository, configMapData(t, buildOverlay(t, root, env.Name))[key])

	env.Cluster = &environments.EnvironmentCluster{Kind: environments.ClusterKindK3d, Context: "k3d-example"}
	root = t.TempDir()
	writeConsumerTree(t, root, env.Name, env.Namespace, service.Name, "database.example:5432")
	require.NoError(t, projectServiceConfiguration(t.Context(), testRenderRoot(t, root), root, service, env, scopeOf(env), serviceInjection{}))
	require.NotContains(t, configMapData(t, buildOverlay(t, root, env.Name)), key)
}
