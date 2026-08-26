package orchestration

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

const (
	buildRecipeSourceDir  = "builder"
	buildRecipeArchiveDir = "build-recipes"
	buildRecipeManifest   = "recipe.codefly.json"
	// buildRecipeSchema is v2 because the manifest now carries the recipe build
	// instructions (image, paths, target, platforms, and build-args) alongside
	// the file digests. v1 recorded only the file digests, so a consumer had the
	// vendored Dockerfile but not the build-args the image was built with — a
	// FRONTEND_SKIN_RUNTIME=1 dropped from the durable recipe rebuilds a different
	// image than the one that shipped.
	buildRecipeSchema = "codefly.dev/build-recipe/v2"
)

// BuildRecipe records which agent produced a service's build recipe, the exact
// build instructions for every image it emits, and the digest of every recipe
// file, so a consumer without the codefly toolchain can identify the recipe and
// rebuild the image directly from the vendored Dockerfile.
type BuildRecipe struct {
	Schema    string            `json:"schema"`
	Publisher string            `json:"publisher"`
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Recipes   []RecipeSpec      `json:"recipes,omitempty"`
	Files     map[string]string `json:"files"`
}

// RecipeSpec is the durable, agent-declared build instruction for one image: the
// Dockerfile and context to build, the target stage, the platforms, and the
// build-args that must be passed with --build-arg. Persisting build-args is what
// lets a consumer reproduce the exact image — a build-arg such as
// FRONTEND_SKIN_RUNTIME changes what the image compiles, so a recipe that omits
// it rebuilds a different image.
type RecipeSpec struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	Dockerfile   string            `json:"dockerfile"`
	Context      string            `json:"context"`
	Dockerignore string            `json:"dockerignore,omitempty"`
	Target       string            `json:"target,omitempty"`
	Platforms    []string          `json:"platforms,omitempty"`
	BuildArgs    map[string]string `json:"buildArgs,omitempty"`
}

// recipeSpecs projects the emitted build plan's recipes into their durable
// manifest form.
func recipeSpecs(plan *builderv0.DockerBuildPlan) []RecipeSpec {
	recipes := plan.GetRecipes()
	if len(recipes) == 0 {
		return nil
	}
	specs := make([]RecipeSpec, 0, len(recipes))
	for _, recipe := range recipes {
		specs = append(specs, RecipeSpec{
			Name:         recipe.GetName(),
			Image:        recipe.GetImage(),
			Dockerfile:   recipe.GetDockerfile(),
			Context:      recipe.GetContext(),
			Dockerignore: recipe.GetDockerignore(),
			Target:       recipe.GetTarget(),
			Platforms:    recipe.GetPlatforms(),
			BuildArgs:    recipe.GetBuildArgs(),
		})
	}
	return specs
}

// recordBuildRecipe copies a service's freshly generated builder/ recipe into a
// durable, version-tagged archive committed alongside the service. The live
// builder/ tree is transient — gitignored and hash-excluded from composed
// modules, and re-rendered per machine — so without this archive the reproducible
// build recipe is lost the moment the working tree is discarded. The archive
// preserves the recipe per producing agent version so a consumer can inspect and
// rebuild the exact recipe that shipped an image.
func recordBuildRecipe(ctx context.Context, service *resources.Service, plan *builderv0.DockerBuildPlan) error {
	w := wool.Get(ctx).In("recordBuildRecipe", wool.NameField(service.Name))
	source := filepath.Join(service.Dir(), buildRecipeSourceDir)
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return w.Wrapf(err, "cannot inspect build recipe")
	}
	if !info.IsDir() {
		return nil
	}
	version := service.Agent.Version
	if version == "" || version != filepath.Base(version) || version == "." || version == ".." {
		return w.NewError("service %s has an agent version %q that is not a safe recipe archive name", service.Name, version)
	}
	destination := filepath.Join(service.Dir(), buildRecipeArchiveDir, version)
	if err = os.RemoveAll(destination); err != nil {
		return w.Wrapf(err, "cannot reset recipe archive")
	}
	files, err := copyRecipeTree(source, destination)
	if err != nil {
		return w.Wrapf(err, "cannot archive build recipe")
	}
	manifest := BuildRecipe{
		Schema:    buildRecipeSchema,
		Publisher: service.Agent.Publisher,
		Name:      service.Agent.Name,
		Version:   version,
		Recipes:   recipeSpecs(plan),
		Files:     files,
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return w.Wrapf(err, "cannot encode recipe manifest")
	}
	if err = writeRecipeFile(filepath.Join(destination, buildRecipeManifest), append(payload, '\n'), 0o644); err != nil {
		return w.Wrapf(err, "cannot write recipe manifest")
	}
	w.Debug("recorded build recipe", wool.Field("archive", destination), wool.Field("files", len(files)))
	return nil
}

// copyRecipeTree copies the regular files under source into destination and
// returns their sha256 digests keyed by slash-separated relative path. Symlinks
// are rejected: a durable recipe must be self-contained and must never carry a
// link that escapes the archive.
func copyRecipeTree(source, destination string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symbolic links are not allowed in a build recipe", relative)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed in a build recipe", relative)
		}
		digest, err := copyRecipeFile(path, filepath.Join(destination, relative), info.Mode().Perm())
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = digest
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// copyRecipeFile copies path to target and returns the sha256 digest of the
// bytes written, hashing in the same pass as the copy.
func copyRecipeFile(path, target string, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return "", err
	}
	writer := bufio.NewWriter(output)
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(writer, hash), input)
	flushErr := writer.Flush()
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if flushErr != nil {
		return "", flushErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeRecipeFile(target string, data []byte, mode os.FileMode) error {
	output, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, writeErr := output.Write(data)
	closeErr := output.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
