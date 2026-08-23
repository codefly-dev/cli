package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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

	require.NoError(t, recordBuildRecipe(context.Background(), service))

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
}

func TestRecordBuildRecipeReplacesStalePriorArchive(t *testing.T) {
	service := serviceWithRecipe(t, "0.3.5", map[string]string{"Dockerfile": "FROM alpine\n"})
	require.NoError(t, recordBuildRecipe(context.Background(), service))

	stale := filepath.Join(service.Dir(), buildRecipeArchiveDir, "0.3.5", "old-only.txt")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o644))

	require.NoError(t, recordBuildRecipe(context.Background(), service))

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

	require.NoError(t, recordBuildRecipe(context.Background(), service))

	_, err := os.Stat(filepath.Join(dir, buildRecipeArchiveDir))
	require.True(t, os.IsNotExist(err))
}

func TestRecordBuildRecipeRejectsUnsafeVersion(t *testing.T) {
	service := serviceWithRecipe(t, "../escape", map[string]string{"Dockerfile": "FROM alpine\n"})
	require.Error(t, recordBuildRecipe(context.Background(), service))
}
