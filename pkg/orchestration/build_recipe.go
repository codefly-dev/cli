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

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

const (
	buildRecipeSourceDir  = "builder"
	buildRecipeArchiveDir = "build-recipes"
	buildRecipeManifest   = "recipe.codefly.json"
	buildRecipeSchema     = "codefly.dev/build-recipe/v1"
)

// BuildRecipe records which agent produced a service's build recipe and the
// digest of every recipe file, so a consumer without the codefly toolchain can
// identify the recipe and rebuild the image directly from the vendored
// Dockerfile.
type BuildRecipe struct {
	Schema    string            `json:"schema"`
	Publisher string            `json:"publisher"`
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Files     map[string]string `json:"files"`
}

// recordBuildRecipe copies a service's freshly generated builder/ recipe into a
// durable, version-tagged archive committed alongside the service. The live
// builder/ tree is transient — gitignored and hash-excluded from composed
// modules, and re-rendered per machine — so without this archive the reproducible
// build recipe is lost the moment the working tree is discarded. The archive
// preserves the recipe per producing agent version so a consumer can inspect and
// rebuild the exact recipe that shipped an image.
func recordBuildRecipe(ctx context.Context, service *resources.Service) error {
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
