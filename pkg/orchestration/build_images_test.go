package orchestration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreservices "github.com/codefly-dev/core/agents/services"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

func TestBuildImagePolicyVerifiesEveryExternalRecipeImage(t *testing.T) {
	ref := "registry.example/alpine@sha256:" + strings.Repeat("a", 64)
	other := "registry.example/tool@sha256:" + strings.Repeat("b", 64)
	dir := t.TempDir()
	path := filepath.Join(dir, "Dockerfile")
	for _, body := range []string{"FROM " + other + "\n", "FROM " + ref + "\nCOPY --from=" + other + " /tool /tool\n", "# syntax=" + other + "\nFROM " + ref + "\n", "FROM " + ref + "\nRUN --mount=type=bind,from=" + other + ",target=/tool true\n", "ARG BASE\nFROM ${BASE}\n"} {
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		plan, err := coreservices.SingleImageBuildPlan(dir, "example.test/app:v1", []string{"linux/amd64"}, []string{"Dockerfile"})
		require.NoError(t, err)
		require.Error(t, verifyBuildImages(dir, plan, []*builderv0.BuildImageInput{{Role: "runtime", Reference: ref, Variant: "alpine"}}), body)
	}
	body := "# syntax=" + other + "\nFROM scratch AS proxy\nFROM " + ref + " AS build\nRUN --mount=type=bind,from=proxy,target=/proxy true\nFROM scratch\nCOPY --from=build /app /app\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	plan, err := coreservices.SingleImageBuildPlan(dir, "example.test/app:v1", []string{"linux/amd64"}, []string{"Dockerfile"})
	require.NoError(t, err)
	bindings := []*builderv0.BuildImageInput{{Role: "runtime", Reference: ref, Variant: "alpine"}, {Role: "frontend", Reference: other, Variant: "dockerfile-v1"}}
	require.NoError(t, verifyBuildImages(dir, plan, bindings))
	scope := imageBuildScope{OutputDir: dir, ContextDir: dir, WorkspaceDir: dir}
	first, err := imageBuildIdentity(t.Context(), plan.Recipes[0], plan, scope, path, "", []string{"buildx", "build", dir}, nil, nil, bindings...)
	require.NoError(t, err)
	bindings[0].Reference = other
	second, err := imageBuildIdentity(t.Context(), plan.Recipes[0], plan, scope, path, "", []string{"buildx", "build", dir}, nil, nil, bindings...)
	require.NoError(t, err)
	require.NotEqual(t, first.Key, second.Key)
	require.NotEqual(t, first.Classes.Bases, second.Classes.Bases)
	require.Equal(t, first.Classes.Code, second.Classes.Code)
	require.ErrorContains(t, verifyBuildImages(dir, plan, bindings), "unselected")
}
