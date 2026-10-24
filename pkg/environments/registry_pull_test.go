package environments

import (
	"strings"
	"testing"
)

// A cell pulls the images an environment pushes through the mirror declared
// for the push registry; with no such mirror it pulls where it pushes.
func TestPullRepositoryReadsTheRegistryThroughItsMirror(t *testing.T) {
	remote := "us-east1-docker.pkg.dev/cell/ghcr"
	for _, tc := range []struct {
		name     string
		registry *EnvironmentRegistry
		want     string
	}{
		{"nil", nil, ""},
		{"no mirrors", &EnvironmentRegistry{URL: "ghcr.io/obin-ai"}, "ghcr.io/obin-ai"},
		{"mirror of another registry", &EnvironmentRegistry{URL: "ghcr.io/obin-ai", Mirrors: map[string]string{"docker.io": "mirror.example/hub"}}, "ghcr.io/obin-ai"},
		{"mirrored", &EnvironmentRegistry{URL: "ghcr.io/obin-ai", Mirrors: map[string]string{"ghcr.io": remote}}, remote + "/obin-ai"},
		{"trailing slash", &EnvironmentRegistry{URL: "ghcr.io/obin-ai/", Mirrors: map[string]string{"ghcr.io": remote}}, remote + "/obin-ai"},
		{"bare host", &EnvironmentRegistry{URL: "localhost:5001", Mirrors: map[string]string{"localhost:5001": remote}}, remote},
		// A Hub prefix is a namespace, not a library image.
		{"hub namespace", &EnvironmentRegistry{URL: "myorg", Mirrors: map[string]string{"docker.io": remote}}, remote + "/myorg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.registry.PullRepository(); got != tc.want {
				t.Fatalf("PullRepository() = %q, want %q", got, tc.want)
			}
		})
	}
	// The same spelling a base image is mirrored with.
	if got := MirroredReference("debian:trixie-slim", map[string]string{"docker.io": remote}); got != remote+"/library/debian:trixie-slim" {
		t.Fatalf("MirroredReference = %q", got)
	}
}

func TestRegistryValidatesArtifactsAndGHCRAuth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		registry EnvironmentRegistry
		want     string
	}{
		{"artifacts", EnvironmentRegistry{URL: "ghcr.io/obin-ai", Artifacts: "us-east1-docker.pkg.dev/cell/bundles"}, ""},
		{"ghcr auth", EnvironmentRegistry{URL: "ghcr.io/obin-ai", Auth: "ghcr"}, ""},
		{"artifacts scheme", EnvironmentRegistry{URL: "ghcr.io/obin-ai", Artifacts: "https://bundles.example"}, "carries a scheme"},
		{"artifacts tag", EnvironmentRegistry{URL: "ghcr.io/obin-ai", Artifacts: "bundles.example/repo:latest"}, "carries a tag"},
		{"artifacts whitespace", EnvironmentRegistry{URL: "ghcr.io/obin-ai", Artifacts: " bundles.example"}, "whitespace"},
		{"ghcr auth elsewhere", EnvironmentRegistry{URL: "registry.example/team", Auth: "ghcr"}, "is not on ghcr.io"},
		{"ghcr auth lookalike", EnvironmentRegistry{URL: "ghcr.io.example/team", Auth: "ghcr"}, "is not on ghcr.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.registry.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// The coordinate decoder refuses unknown fields: auth, mirrors and artifacts
// are all carried by an imported contract.
func TestCoordinateContractCarriesRegistryAuthMirrorsAndArtifacts(t *testing.T) {
	contract, err := ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","environment":{"name":"staging","registry":{` +
		`"url":"ghcr.io/obin-ai","auth":"ghcr","mirrors":{"ghcr.io":"us-east1-docker.pkg.dev/cell/ghcr"},"artifacts":"us-east1-docker.pkg.dev/cell/bundles"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	registry := contract.Environment.Registry
	if registry.Auth != "ghcr" || registry.Artifacts != "us-east1-docker.pkg.dev/cell/bundles" {
		t.Fatalf("registry = %+v", registry)
	}
	if got := registry.PullRepository(); got != "us-east1-docker.pkg.dev/cell/ghcr/obin-ai" {
		t.Fatalf("PullRepository() = %q", got)
	}
	_, err = ParseCoordinateContract([]byte(`{"schema":"codefly/coordinate/v1","environment":{"name":"staging","registry":{"url":"registry.example/team","auth":"ghcr"}}}`))
	if err == nil || !strings.Contains(err.Error(), "is not on ghcr.io") {
		t.Fatalf("ghcr auth on another registry: %v", err)
	}
}
