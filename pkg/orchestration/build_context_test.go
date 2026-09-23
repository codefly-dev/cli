package orchestration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
)

var recipeContextCases = []string{"symlink", "declared override", "sibling override", "unreadable exclusion", "reincluded child"}

func recipeContextFixture(t *testing.T, scenario string) (string, string, *builderv0.DockerBuildRecipe) {
	t.Helper()
	root := t.TempDir()
	output := filepath.Join(root, "builder")
	require.NoError(t, os.MkdirAll(output, 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "assets"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "assets/index.html"), []byte("required asset"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(output, "Dockerfile"), []byte("FROM scratch\nCOPY . /\n"), 0o600))
	recipe := &builderv0.DockerBuildRecipe{Dockerfile: "Dockerfile"}
	switch scenario {
	case "symlink":
		link := filepath.Join(t.TempDir(), "source")
		require.NoError(t, os.Symlink(root, link))
		return link, output, recipe
	case "declared override", "sibling override":
		require.NoError(t, os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("assets/\n"), 0o600))
		policy := "!assets/\nbuilder/\n"
		if scenario == "declared override" {
			recipe.Dockerignore = "dockerignore"
			require.NoError(t, os.WriteFile(filepath.Join(output, "dockerignore"), []byte(policy), 0o600))
			policy = "assets/\n"
		}
		require.NoError(t, os.WriteFile(filepath.Join(output, "Dockerfile.dockerignore"), []byte(policy), 0o600))
	case "unreadable exclusion":
		dir := filepath.Join(root, "ignored")
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("excluded"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("ignored/\n"), 0o600))
		require.NoError(t, os.Chmod(dir, 0))
		t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0o700)) })
	case "reincluded child":
		require.NoError(t, os.WriteFile(filepath.Join(root, "assets/secret"), []byte("excluded"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, ".dockerignore"), []byte("assets/\n!assets/index.html\n"), 0o600))
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
	return root, output, recipe
}

func TestPrepareRecipeContextPreservesDockerInputs(t *testing.T) {
	for _, scenario := range recipeContextCases {
		t.Run(scenario, func(t *testing.T) {
			root, output, recipe := recipeContextFixture(t, scenario)
			prepared, err := prepareRecipeContext(context.Background(), root, output, recipe)
			require.NoError(t, err)
			defer prepared.Close()
			resolved, err := filepath.EvalSymlinks(root)
			require.NoError(t, err)
			require.Equal(t, resolved, prepared.Root)
			require.FileExists(t, filepath.Join(prepared.Root, "assets/index.html"))
			if scenario == "declared override" {
				policy, err := os.ReadFile(prepared.Dockerfile + ".dockerignore")
				require.NoError(t, err)
				require.Equal(t, "!assets/\nbuilder/\n", string(policy))
				original, err := os.ReadFile(filepath.Join(output, "Dockerfile.dockerignore"))
				require.NoError(t, err)
				require.Equal(t, "assets/\n", string(original))
				second, err := prepareRecipeContext(context.Background(), root, output, recipe)
				require.NoError(t, err)
				defer second.Close()
				require.NotEqual(t, prepared.Dockerfile, second.Dockerfile)
				require.NoError(t, prepared.Close())
				require.NoFileExists(t, prepared.Dockerfile)
				require.FileExists(t, second.Dockerfile)
			}
		})
	}
}

func TestPrepareRecipeContextRejectsMissingDeclaredIgnore(t *testing.T) {
	root, output, recipe := recipeContextFixture(t, "symlink")
	recipe.Dockerignore = "missing"
	_, err := prepareRecipeContext(context.Background(), root, output, recipe)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// Exercise the real context sender and COPY operation, not just argv or the
// existence of source files. No image is pushed, tagged, or loaded into a daemon.
func TestRecipeContextDockerSemantics(t *testing.T) {
	if os.Getenv("CODEFLY_TEST_RECIPE_CONTEXT") != "1" {
		t.Skip("set CODEFLY_TEST_RECIPE_CONTEXT=1 to build context regression fixtures with Docker")
	}
	for _, scenario := range recipeContextCases {
		t.Run(scenario, func(t *testing.T) {
			root, output, recipe := recipeContextFixture(t, scenario)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			prepared, err := prepareRecipeContext(ctx, root, output, recipe)
			require.NoError(t, err)
			defer prepared.Close()
			destination := t.TempDir()
			command := exec.CommandContext(ctx, "docker", "buildx", "build", "--progress=plain", "-f", prepared.Dockerfile,
				"--output", "type=local,dest="+destination, prepared.Root)
			logs, err := command.CombinedOutput()
			if scenario == "unreadable exclusion" {
				// Some Docker clients (including macOS xattr handling) reject
				// unreadable directory metadata before applying exclusions.
				// Require the same outcome as a direct native build, while the
				// preparation assertion above always requires no eager traversal.
				baseline := exec.CommandContext(ctx, "docker", "buildx", "build", "--progress=plain",
					"-f", filepath.Join(output, "Dockerfile"), "--output", "type=local,dest="+t.TempDir(), root)
				baselineLogs, baselineErr := baseline.CombinedOutput()
				if baselineErr != nil {
					require.Contains(t, string(baselineLogs), "permission denied")
					require.Error(t, err)
					require.Contains(t, string(logs), "permission denied")
					return
				}
			}
			require.NoError(t, err, "%s", logs)
			asset, err := os.ReadFile(filepath.Join(destination, "assets/index.html"))
			require.NoError(t, err)
			require.Equal(t, "required asset", string(asset))
			if scenario == "unreadable exclusion" {
				require.NoDirExists(t, filepath.Join(destination, "ignored"))
			}
			if scenario == "reincluded child" {
				require.NoFileExists(t, filepath.Join(destination, "assets/secret"))
			}
			if scenario == "declared override" || scenario == "sibling override" {
				require.NoDirExists(t, filepath.Join(destination, "builder"))
			}
		})
	}
}

// TestRecipeContextRootSelectsWhatBuildxReceives closes the loop the scope
// opens: the root the plan selects is the directory prepareRecipeContext hands
// buildx, and the two scopes select different trees on the same fixture.
//
// The case that matters is a Go service whose module replaces a sibling by
// filesystem path. Its agent carries that sibling into the recipe tree and
// rewrites the directive, so the replacement exists under the recipe root and
// nowhere under the service root. Build the service root and the image resolves
// the original directive against a directory it does not contain; build the
// tree the TREE plan claims — and that the CLI has just verified file by file —
// and it resolves what the host resolves.
func TestRecipeContextRootSelectsWhatBuildxReceives(t *testing.T) {
	serviceRoot := t.TempDir()
	recipeRoot := filepath.Join(serviceRoot, ".codefly-recipe")
	require.NoError(t, os.MkdirAll(filepath.Join(recipeRoot, "code", "_replace"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(recipeRoot, "Dockerfile"), []byte("FROM scratch\nCOPY code/ ./code/\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(serviceRoot, "code"), 0o755))

	recipe := &builderv0.DockerBuildRecipe{Dockerfile: "Dockerfile", Context: "."}
	for _, tc := range []struct {
		name  string
		scope builderv0.RecipeInventoryScope
		root  string
		carry bool
	}{
		{"tree builds the assembled destination", builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_TREE, recipeRoot, true},
		{"emitted builds the service directory", builderv0.RecipeInventoryScope_RECIPE_INVENTORY_SCOPE_EMITTED, serviceRoot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &builderv0.DockerBuildPlan{
				Scope:   tc.scope,
				Recipes: []*builderv0.DockerBuildRecipe{recipe},
				Files:   []*builderv0.RecipeFile{{Path: "Dockerfile"}, {Path: "code/_replace/keep.go"}},
			}
			root := recipeContextRoot(serviceRoot, recipeRoot, plan)
			require.Equal(t, tc.root, root)

			contextDir, err := recipeContext(root, recipe)
			require.NoError(t, err)
			prepared, err := prepareRecipeContext(context.Background(), contextDir, recipeRoot, recipe)
			require.NoError(t, err)
			defer func() { require.NoError(t, prepared.Close()) }()

			resolved, err := filepath.EvalSymlinks(tc.root)
			require.NoError(t, err)
			require.Equal(t, resolved, prepared.Root, "buildx must receive the root the scope selects")

			_, err = os.Stat(filepath.Join(prepared.Root, "code", "_replace"))
			if tc.carry {
				require.NoError(t, err, "a carried replacement must be inside the context buildx receives")
			} else {
				require.True(t, os.IsNotExist(err), "the service tree never holds what the emitter assembled")
			}
		})
	}
}
