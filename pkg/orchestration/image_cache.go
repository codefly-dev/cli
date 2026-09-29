package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// The image build cache answers one question: would building this recipe again
// produce the same image? A configuration edit re-renders a module and reaches
// the image build for every one of its services, and an image build is minutes
// each, so a change that touches nothing an image is built from must keep the
// digest it already has instead of paying for a rebuild that cannot differ.
//
// It is content-addressed in the literal sense: the key is a digest over the
// bytes that go into the image — every file of the build context, the verified
// recipe tree, and the exact buildx invocation — never over a declaration that
// summarises them. A dependency manifest, a lock file or a tree digest recorded
// by something else states what the inputs are *supposed* to be; only the bytes
// state what they are. Keying on a manifest is how a cache serves a stale
// binary for a source edit in a language whose manifest did not move: the edit
// is real, the key does not move, and neither the image digest nor the artifact
// itself shows anything wrong. So a source file that changes by one byte
// changes the context digest, and therefore the key, whether or not any
// manifest changed.
//
// Everything the cache cannot account for makes it decline rather than guess: a
// plan with no verified tree digest, a context it cannot walk, an ignore file it
// cannot parse, and a recorded image the container engine or registry can no
// longer produce all fall through to a build.
//
// One input is bound indirectly, and it is worth stating why that is not the
// same mistake. A recipe declaring Go module downloads is built with a module
// proxy the plan phase fetched into a per-run directory (goModulePrefetch), so
// that directory's path is normalized out of the key and its contents are not
// hashed. What the build reads out of it is decided entirely by the go.mod and
// go.sum of a module root that proxiesFor has already rejected unless it lies
// inside the build context — so both files are hashed here — and go.sum states
// the content hash of every module version, which the prefetch verifies with
// -mod=readonly. The proxy is therefore a function of bytes the key already
// binds, by a cryptographic hash rather than by a name. A filesystem `replace`
// resolves to a directory instead, which the build can only read if it is in the
// context, where it is hashed like any other file.

const (
	// imageCacheSchema is part of every key, so a change to what the identity
	// binds retires every entry recorded under the old definition instead of
	// matching one computed a different way.
	imageCacheSchema = 1
	// imageCacheDir is the workspace-relative directory entries live in.
	// `.codefly/` is CLI-owned scratch, gitignored by core's composition, and
	// excluded from the context digest, so writing here cannot perturb the
	// inputs of the next build.
	imageCacheDir = ".codefly/build-cache"
	// imageCacheScratchSegment names the CLI-owned directory excluded from a
	// context digest at any depth.
	imageCacheScratchSegment = ".codefly"
)

// imageInputs is everything that decides what an image build produces. Its JSON
// encoding is the preimage of the cache key.
//
// Command is the buildx argv the build runs, with three normalizations: the
// build context, the staged Dockerfile and each prefetched Go module proxy are
// replaced by placeholders (their paths are per-run temporary locations, while
// their contents are bound by ContextTree and RecipeTree), and `--builder` and
// `--metadata-file` are dropped (where a build runs and where buildx writes its
// metadata do not change the image it produces). Everything else buildx is told
// — platforms, target, every build argument and its value, push versus load, the
// registry layer cache policy — is in the key verbatim, so a flag added to the
// invocation later enters the identity without anyone remembering to list it
// here.
type imageInputs struct {
	Schema       int      `json:"schema"`
	Recipe       string   `json:"recipe"`
	Dockerfile   string   `json:"dockerfile"`
	Context      string   `json:"context"`
	Dockerignore string   `json:"dockerignore,omitempty"`
	Command      []string `json:"command"`
	// RecipeTree is the emitted recipe's aggregate content digest, as the plan
	// declares it and VerifyDockerBuildPlan has already verified it against the
	// files on disk.
	RecipeTree string `json:"recipe_tree_digest"`
	// ContextTree is the digest of every byte of the build context Docker would
	// send, computed here rather than taken from any declaration.
	ContextTree string `json:"context_tree_digest"`
}

// imageCacheEntry records which image a previously executed build produced for
// one identity, and how to prove that image still exists before standing in for
// a build with it.
type imageCacheEntry struct {
	Schema    int      `json:"schema"`
	Key       string   `json:"key"`
	Service   string   `json:"service"`
	Recipe    string   `json:"recipe"`
	Image     string   `json:"image"`
	Pushed    bool     `json:"pushed"`
	Digest    string   `json:"digest,omitempty"`
	ImageID   string   `json:"image_id,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
	Recorded  string   `json:"recorded_at"`
}

// imageBuildCache is the workspace-local store of entries. A nil root disables
// it: a flow with no workspace directory has nowhere to keep one, and builds.
type imageBuildCache struct {
	root string
}

func (b *Builder) imageBuildCache() *imageBuildCache {
	if b.world == nil || b.world.Workspace == nil {
		return &imageBuildCache{}
	}
	dir := b.world.Workspace.Dir()
	if dir == "" {
		return &imageBuildCache{}
	}
	return &imageBuildCache{root: filepath.Join(dir, filepath.FromSlash(imageCacheDir))}
}

func (cache *imageBuildCache) enabled() bool { return cache != nil && cache.root != "" }

// path is one entry's file. Each identity is its own file, written by rename, so
// several services building concurrently — which a module render does — never
// contend on a shared index.
func (cache *imageBuildCache) path(key string) string {
	return filepath.Join(cache.root, strings.TrimPrefix(key, "sha256:")+".json")
}

func (cache *imageBuildCache) lookup(key string) (*imageCacheEntry, bool) {
	if !cache.enabled() || key == "" {
		return nil, false
	}
	payload, err := os.ReadFile(cache.path(key))
	if err != nil {
		return nil, false
	}
	var entry imageCacheEntry
	if err = json.Unmarshal(payload, &entry); err != nil {
		return nil, false
	}
	if entry.Schema != imageCacheSchema || entry.Key != key || entry.Image == "" {
		return nil, false
	}
	return &entry, true
}

func (cache *imageBuildCache) store(ctx context.Context, entry *imageCacheEntry) error {
	if !cache.enabled() {
		return nil
	}
	if err := os.MkdirAll(cache.root, 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	return shared.WriteFileAtomic(ctx, cache.path(entry.Key), append(payload, '\n'), 0o644)
}

// imageBuildIdentity digests the inputs of one recipe's image build. An empty
// key with no error means the build is not cacheable and must run.
func imageBuildIdentity(recipe *builderv0.DockerBuildRecipe, plan *builderv0.DockerBuildPlan, outputDir, contextDir, dockerfile string, args []string, proxies map[string]string) (string, error) {
	recipeTree := plan.GetDigest()
	if recipeTree == "" {
		return "", fmt.Errorf("recipe tree of %s declares no verified content digest", recipe.GetName())
	}
	patterns, err := contextIgnorePatterns(outputDir, contextDir, recipe)
	if err != nil {
		return "", err
	}
	ignore, err := patternmatcher.New(patterns)
	if err != nil {
		return "", fmt.Errorf("ignore policy of %s: %w", recipe.GetName(), err)
	}
	contextTree, err := contextContentDigest(contextDir, ignore, contextExclusions(contextDir, outputDir))
	if err != nil {
		return "", err
	}
	inputs := imageInputs{
		Schema:       imageCacheSchema,
		Recipe:       recipe.GetName(),
		Dockerfile:   recipe.GetDockerfile(),
		Context:      recipe.GetContext(),
		Dockerignore: recipe.GetDockerignore(),
		Command:      normalizedBuildxArgs(args, contextDir, dockerfile, proxies),
		RecipeTree:   recipeTree,
		ContextTree:  contextTree,
	}
	payload, err := json.Marshal(inputs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// normalizedBuildxArgs rewrites the invocation into the form described on
// imageInputs.Command: per-run paths become placeholders, and the two flags that
// say where the build runs and where its metadata lands are dropped.
func normalizedBuildxArgs(args []string, contextDir, dockerfile string, proxies map[string]string) []string {
	substitutions := make([][2]string, 0, 2+len(proxies))
	substitutions = append(substitutions, [2]string{dockerfile, "{dockerfile}"}, [2]string{contextDir, "{context}"})
	for _, proxy := range proxies {
		substitutions = append(substitutions, [2]string{proxy, "{proxy}"})
	}
	// Longest literal first: a staged Dockerfile or a proxy can sit inside the
	// build context, and collapsing the shorter path first would leave the
	// longer one only partly rewritten.
	sort.Slice(substitutions, func(i, j int) bool { return len(substitutions[i][0]) > len(substitutions[j][0]) })
	normalized := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--builder", "--metadata-file":
			index++
			continue
		}
		argument := args[index]
		for _, substitution := range substitutions {
			if substitution[0] == "" {
				continue
			}
			argument = strings.ReplaceAll(argument, substitution[0], substitution[1])
		}
		normalized = append(normalized, argument)
	}
	return normalized
}

// contextIgnorePatterns is the ignore policy Docker applies to this recipe's
// context, read from the same two places and with the same precedence the build
// uses: a recipe-declared ignore file replaces the context root's, exactly as a
// Dockerfile-specific ignore file replaces it for Docker (prepareRecipeContext
// stages the declared one beside a private copy of the definition for that
// reason). A file the policy excludes is not sent to the builder, so it is not
// an input; matching is left to the same matcher Docker uses rather than to a
// second reading of the same patterns.
func contextIgnorePatterns(outputDir, contextDir string, recipe *builderv0.DockerBuildRecipe) ([]string, error) {
	path := filepath.Join(contextDir, ".dockerignore")
	if declared := recipe.GetDockerignore(); declared != "" {
		path = filepath.Join(outputDir, filepath.FromSlash(declared))
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read ignore policy %s: %w", path, err)
	}
	defer file.Close()
	patterns, err := ignorefile.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("parse ignore policy %s: %w", path, err)
	}
	return patterns, nil
}

// contextExclusions lists the slash-separated context-relative paths left out of
// the digest because the CLI itself writes them from inputs the digest already
// binds. The recipe archive is a copy of the emitted recipe tree keyed by agent
// version, so recording it after a build would otherwise invalidate the identity
// of the build that produced it — a rebuild on every second run, forever.
func contextExclusions(contextDir, outputDir string) map[string]bool {
	// contextDir has had its symlinks resolved by prepareRecipeContext, so the
	// recipe root must be resolved the same way before the two are compared —
	// on a host whose temporary or home directory is a link, the unresolved
	// spelling reads as outside the context and excludes nothing.
	root := filepath.Dir(outputDir)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	archive := filepath.Join(root, buildRecipeArchiveDir)
	relative, err := filepath.Rel(contextDir, archive)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil
	}
	return map[string]bool{filepath.ToSlash(relative): true}
}

// contextContentDigest hashes every byte of the build context Docker would send.
// Traversal is lexical and records each entry's kind, so an added, removed,
// renamed, retyped or edited file all move the digest.
func contextContentDigest(contextDir string, ignore *patternmatcher.PatternMatcher, excluded map[string]bool) (string, error) {
	root := filepath.Clean(contextDir)
	hasher := sha256.New()
	writeImageCacheRecord(hasher, "root", filepath.Base(root))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		slashed := filepath.ToSlash(relative)
		if excluded[slashed] || hasScratchSegment(slashed) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		ignored, err := ignore.MatchesOrParentMatches(relative)
		if err != nil {
			return err
		}
		if ignored {
			// A directory whose whole subtree is excluded is skipped, unless the
			// policy has exception patterns that could re-include a child — the
			// same condition Docker's own context traversal prunes on. Either
			// way the excluded entry itself is not part of the context.
			if entry.IsDir() && !ignore.Exclusions() {
				return filepath.SkipDir
			}
			return nil
		}
		return hashContextEntry(hasher, slashed, path, entry)
	})
	if err != nil {
		return "", fmt.Errorf("digest build context %s: %w", contextDir, err)
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func hasScratchSegment(slashed string) bool {
	for _, segment := range strings.Split(slashed, "/") {
		if segment == imageCacheScratchSegment {
			return true
		}
	}
	return false
}

func hashContextEntry(hasher hash.Hash, relative, path string, entry fs.DirEntry) error {
	info, err := entry.Info()
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, linkErr := os.Readlink(path)
		if linkErr != nil {
			return linkErr
		}
		writeImageCacheRecord(hasher, "symlink", relative, filepath.ToSlash(target))
		return nil
	case info.IsDir():
		writeImageCacheRecord(hasher, "directory", relative)
		return nil
	case !info.Mode().IsRegular():
		writeImageCacheRecord(hasher, "special", relative, info.Mode().String())
		return nil
	}
	executable := "0"
	if info.Mode().Perm()&0o111 != 0 {
		executable = "1"
	}
	writeImageCacheRecord(hasher, "file", relative, executable, strconv.FormatInt(info.Size(), 10))
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	hasher.Write([]byte{0})
	return errors.Join(copyErr, closeErr)
}

// writeImageCacheRecord frames each field with its length, so no combination of
// path and content can be spelled two ways and collide.
func writeImageCacheRecord(hasher hash.Hash, fields ...string) {
	for _, field := range fields {
		_, _ = hasher.Write([]byte(strconv.Itoa(len(field))))
		_, _ = hasher.Write([]byte{':'})
		_, _ = hasher.Write([]byte(field))
	}
	_, _ = hasher.Write([]byte{0xff})
}

// imageStillExists proves the recorded image can still be produced before a
// cache entry stands in for a build. An entry is a claim about an image, not the
// image: a pushed manifest can be deleted from the registry and a loaded image
// pruned from the daemon, and reusing either would pin a reference that resolves
// to nothing. Anything short of a positive answer — including an unreachable
// registry — is a miss.
func imageStillExists(ctx context.Context, entry *imageCacheEntry) bool {
	if entry.Pushed {
		reference, ok := digestReference(entry.Image, entry.Digest)
		if !ok {
			return false
		}
		return exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", "--raw", reference).Run() == nil
	}
	if entry.ImageID == "" {
		return false
	}
	identifier, err := inspectLocalImageID(ctx, entry.Image)
	return err == nil && identifier == entry.ImageID
}

// digestReference rewrites a tagged image into its digest form, so presence is
// checked for the exact manifest that was recorded rather than for whatever the
// tag serves now.
func digestReference(image, digest string) (string, bool) {
	if !sha256Digest.MatchString(digest) {
		return "", false
	}
	repository := image
	if separator := strings.LastIndex(image, "/"); separator >= 0 {
		if colon := strings.LastIndex(image[separator:], ":"); colon >= 0 {
			repository = image[:separator+colon]
		}
	} else if colon := strings.LastIndex(image, ":"); colon >= 0 {
		repository = image[:colon]
	}
	if repository == "" {
		return "", false
	}
	return repository + "@" + digest, true
}

// cachedImageEntry is the entry a completed build records, or nil when the
// build resolved no immutable identity for its image and therefore has nothing
// a later run could verify.
func cachedImageEntry(key, service string, recipe *builderv0.DockerBuildRecipe, pushed bool, digest, imageID string) *imageCacheEntry {
	if key == "" || (pushed && digest == "") || (!pushed && imageID == "") {
		return nil
	}
	return &imageCacheEntry{
		Schema:    imageCacheSchema,
		Key:       key,
		Service:   service,
		Recipe:    recipe.GetName(),
		Image:     recipe.GetImage(),
		Pushed:    pushed,
		Digest:    digest,
		ImageID:   imageID,
		Platforms: recipe.GetPlatforms(),
		Recorded:  time.Now().UTC().Format(time.RFC3339),
	}
}
