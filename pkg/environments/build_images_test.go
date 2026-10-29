package environments

import (
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestBuildImagesSelectionPrecedenceAndNoMutation(t *testing.T) {
	ref := "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64)
	declared := []*builderv0.BuildImageInput{{Role: "runtime", Reference: ref, Variant: "alpine3.23-musl"}, {Role: "builder", Reference: ref, Variant: "go1.27"}}
	typeRef := "registry.example/type/alpine@sha256:" + strings.Repeat("b", 64)
	serviceRef := "registry.example/service/alpine@sha256:" + strings.Repeat("c", 64)
	policy := &BuildImages{Registries: map[string]string{"docker.io": "registry.example/public"}, Types: map[string]map[string]BuildImage{"codefly.dev/unknown-agent": {"runtime": {Reference: typeRef, Variant: "alpine3.23-musl"}}}, Services: map[string]map[string]BuildImage{"module/api": {"runtime": {Reference: serviceRef, Variant: "alpine3.23-musl"}}}}
	inputs, err := policy.Resolve("codefly.dev/unknown-agent", "module/api", declared)
	require.NoError(t, err)
	require.Equal(t, serviceRef, inputs[0].Reference)
	require.Equal(t, "registry.example/public/library/alpine@sha256:"+strings.Repeat("a", 64), inputs[1].Reference)
	inputs, err = policy.Resolve("codefly.dev/unknown-agent", "module/other", declared)
	require.NoError(t, err)
	require.Equal(t, typeRef, inputs[0].Reference)
	require.Equal(t, ref, declared[0].Reference)
	policy.Services["module/api"]["unknown"] = BuildImage{Reference: serviceRef, Variant: "alpine3.23-musl"}
	_, err = policy.Resolve("codefly.dev/unknown-agent", "module/api", declared)
	require.ErrorContains(t, err, "unsupported image role")
}

func TestBuildImagesCoordinateImportStrictness(t *testing.T) {
	prefix := `{"schema":"codefly/coordinate/v1","requires_capabilities":["build-image-bindings"],"environment":{"name":"stage","build-images":`
	for _, policy := range []string{`{"types":{"go":{"runtime":{"reference":"scratch","variant":"scratch"}}}}`, `{"services":{"m/s":{"runtime":{"reference":"alpine:latest","variant":"alpine"}}}}`, `{"registries":{"docker.io":"https://registry.example"}}`, `{"typo":{}}`} {
		_, err := ParseCoordinateContract([]byte(prefix + policy + `}}`))
		require.Error(t, err, policy)
	}
	c, err := ParseCoordinateContract([]byte(prefix + `{"types":{"codefly.dev/unknown":{"runtime":{"reference":"scratch","variant":"scratch"}}}}}}`))
	require.NoError(t, err)
	require.True(t, c.Environment.BuildImages.Applies("codefly.dev/unknown", "m/s"))
	require.False(t, c.Environment.BuildImages.Applies("codefly.dev/another", "m/s"))
}
