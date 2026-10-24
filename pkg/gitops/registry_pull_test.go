package gitops

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/builder"
	"github.com/codefly-dev/cli/pkg/environments"
)

// A render names its images by the repository the cell pulls from — the
// remote repository mirroring ghcr.io — which is the DockerRepository every
// agent writes the manifest's image (and so the cell file's) under.
func TestSnapshotImagesAreNamedByThePullRepository(t *testing.T) {
	previous, err := builder.DockerBuildContext(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { builder.SetRepository(previous.DockerRepository) })
	for _, tc := range []struct {
		registry *environments.EnvironmentRegistry
		want     string
	}{
		{&environments.EnvironmentRegistry{URL: "ghcr.io/obin-ai", Mirrors: map[string]string{"ghcr.io": "us-east1-docker.pkg.dev/cell/ghcr"}}, "us-east1-docker.pkg.dev/cell/ghcr/obin-ai"},
		{&environments.EnvironmentRegistry{URL: "registry.example.com/team"}, "registry.example.com/team"},
	} {
		if err := prepareSnapshotRegistry(context.Background(), &environments.Environment{Name: "snap", Registry: tc.registry}); err != nil {
			t.Fatal(err)
		}
		built, err := builder.DockerBuildContext(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if built.DockerRepository != tc.want {
			t.Fatalf("repository = %q, want %q", built.DockerRepository, tc.want)
		}
	}
}

func TestFrontendBundlesUseTheArtifactsRegistryWhenDeclared(t *testing.T) {
	for _, tc := range []struct {
		registry *environments.EnvironmentRegistry
		want     string
	}{
		{&environments.EnvironmentRegistry{URL: "ghcr.io/obin-ai", Artifacts: "us-east1-docker.pkg.dev/cell/bundles/"}, "us-east1-docker.pkg.dev/cell/bundles/platform/" + FrontendBundleRepository},
		{&environments.EnvironmentRegistry{URL: "ghcr.io/obin-ai"}, "ghcr.io/obin-ai/platform/" + FrontendBundleRepository},
	} {
		got, err := frontendBundleRepositoryOf(&environments.Environment{Name: "staging", Registry: tc.registry}, "platform")
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("bundle repository = %q, want %q", got, tc.want)
		}
	}
}
