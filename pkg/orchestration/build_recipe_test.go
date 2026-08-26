package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func serviceWithRecipe(t *testing.T, version string, recipe map[string]string) *resources.Service {
	t.Helper()
	dir := t.TempDir()
	builder := filepath.Join(dir, buildRecipeSourceDir)
	require.NoError(t, os.MkdirAll(builder, 0o755))
	for name, content := range recipe {
		path := filepath.Join(builder, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	service := &resources.Service{
		Name:  "store",
		Agent: &resources.Agent{Publisher: "codefly.dev", Name: "go-grpc", Version: version},
	}
	service.WithDir(dir)
	return service
}

func TestRecordBuildRecipeArchivesRecipeTaggedWithVersion(t *testing.T) {
	recipe := map[string]string{
		"Dockerfile":              "FROM alpine\nCOPY builder/runtime-access.sql /app/\n",
		"dockerignore":            "code/node_modules\n",
		"runtime-access.sql":      "GRANT SELECT ON ALL TABLES;\n",
		"nested/extra-recipe.txt": "helper\n",
	}
	service := serviceWithRecipe(t, "0.3.5", recipe)

	require.NoError(t, recordBuildRecipe(context.Background(), service, nil))

	archive := filepath.Join(service.Dir(), buildRecipeArchiveDir, "0.3.5")
	for name, content := range recipe {
		got, err := os.ReadFile(filepath.Join(archive, filepath.FromSlash(name)))
		require.NoError(t, err)
		require.Equal(t, content, string(got))
	}

	payload, err := os.ReadFile(filepath.Join(archive, buildRecipeManifest))
	require.NoError(t, err)
	var manifest BuildRecipe
	require.NoError(t, json.Unmarshal(payload, &manifest))
	require.Equal(t, buildRecipeSchema, manifest.Schema)
	require.Equal(t, "codefly.dev", manifest.Publisher)
	require.Equal(t, "go-grpc", manifest.Name)
	require.Equal(t, "0.3.5", manifest.Version)
	require.Len(t, manifest.Files, len(recipe))
	for name, content := range recipe {
		digest := sha256.Sum256([]byte(content))
		require.Equal(t, hex.EncodeToString(digest[:]), manifest.Files[name])
	}
	// A legacy in-agent build emits no plan: the manifest records file digests
	// only and must not carry a "recipes" key at all.
	require.Empty(t, manifest.Recipes)
	require.NotContains(t, string(payload), "\"recipes\"")
}

func TestRecordBuildRecipePersistsAgentDeclaredBuildArgs(t *testing.T) {
	recipe := map[string]string{
		"Dockerfile":   "ARG FRONTEND_SKIN_RUNTIME\nFROM alpine\n",
		"dockerignore": "code/node_modules\n",
	}
	service := serviceWithRecipe(t, "0.3.5", recipe)

	plan := &builderv0.DockerBuildPlan{
		Recipes: []*builderv0.DockerBuildRecipe{{
			Name:         "app",
			Image:        "repo/app:v1",
			Dockerfile:   "Dockerfile",
			Context:      ".",
			Dockerignore: "dockerignore",
			Target:       "final",
			Platforms:    []string{"linux/amd64", "linux/arm64"},
			BuildArgs:    map[string]string{"FRONTEND_SKIN_RUNTIME": "1"},
		}},
	}

	require.NoError(t, recordBuildRecipe(context.Background(), service, plan))

	archive := filepath.Join(service.Dir(), buildRecipeArchiveDir, "0.3.5")
	payload, err := os.ReadFile(filepath.Join(archive, buildRecipeManifest))
	require.NoError(t, err)
	var manifest BuildRecipe
	require.NoError(t, json.Unmarshal(payload, &manifest))

	// The durable recipe must carry the agent-declared build-args, or a consumer
	// rebuilds a different image than the one that shipped.
	require.Len(t, manifest.Recipes, 1)
	got := manifest.Recipes[0]
	require.Equal(t, "app", got.Name)
	require.Equal(t, "repo/app:v1", got.Image)
	require.Equal(t, "Dockerfile", got.Dockerfile)
	require.Equal(t, ".", got.Context)
	require.Equal(t, "dockerignore", got.Dockerignore)
	require.Equal(t, "final", got.Target)
	require.Equal(t, []string{"linux/amd64", "linux/arm64"}, got.Platforms)
	require.Equal(t, map[string]string{"FRONTEND_SKIN_RUNTIME": "1"}, got.BuildArgs)
}

func TestRecordBuildRecipeRedactsSensitiveBuildArgs(t *testing.T) {
	recipe := map[string]string{"Dockerfile": "FROM alpine\n"}
	service := serviceWithRecipe(t, "0.3.5", recipe)

	secret := "npm_s3cr3t-value"
	plan := &builderv0.DockerBuildPlan{
		Recipes: []*builderv0.DockerBuildRecipe{{
			Name:       "app",
			Image:      "repo/app:v1",
			Dockerfile: "Dockerfile",
			Context:    ".",
			BuildArgs: map[string]string{
				"FRONTEND_SKIN_RUNTIME": "1",
				"NPM_TOKEN":             secret,
			},
		}},
	}

	require.NoError(t, recordBuildRecipe(context.Background(), service, plan))

	archive := filepath.Join(service.Dir(), buildRecipeArchiveDir, "0.3.5")
	payload, err := os.ReadFile(filepath.Join(archive, buildRecipeManifest))
	require.NoError(t, err)

	// The secret value must never be written to the committed archive; the key is
	// kept so a consumer still knows the build needs it.
	require.NotContains(t, string(payload), secret)

	var manifest BuildRecipe
	require.NoError(t, json.Unmarshal(payload, &manifest))
	require.Len(t, manifest.Recipes, 1)
	require.Equal(t, map[string]string{
		"FRONTEND_SKIN_RUNTIME": "1",
		"NPM_TOKEN":             redactedBuildArgValue,
	}, manifest.Recipes[0].BuildArgs)
}

func TestRecordBuildRecipeReplacesStalePriorArchive(t *testing.T) {
	service := serviceWithRecipe(t, "0.3.5", map[string]string{"Dockerfile": "FROM alpine\n"})
	require.NoError(t, recordBuildRecipe(context.Background(), service, nil))

	stale := filepath.Join(service.Dir(), buildRecipeArchiveDir, "0.3.5", "old-only.txt")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o644))

	require.NoError(t, recordBuildRecipe(context.Background(), service, nil))

	_, err := os.Stat(stale)
	require.True(t, os.IsNotExist(err), "stale recipe file survived re-recording")
}

func TestRecordBuildRecipeNoRecipeIsNoOp(t *testing.T) {
	dir := t.TempDir()
	service := &resources.Service{
		Name:  "store",
		Agent: &resources.Agent{Publisher: "codefly.dev", Name: "go-grpc", Version: "0.3.5"},
	}
	service.WithDir(dir)

	require.NoError(t, recordBuildRecipe(context.Background(), service, nil))

	_, err := os.Stat(filepath.Join(dir, buildRecipeArchiveDir))
	require.True(t, os.IsNotExist(err))
}

func TestRecordBuildRecipeRejectsUnsafeVersion(t *testing.T) {
	service := serviceWithRecipe(t, "../escape", map[string]string{"Dockerfile": "FROM alpine\n"})
	require.Error(t, recordBuildRecipe(context.Background(), service, nil))
}
