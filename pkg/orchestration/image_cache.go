package orchestration

import (
	"bytes"
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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/asottile/dockerfile"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/wool"
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
// Two inputs a build reads that are not plain context files are bound
// explicitly, because each is a way an image can change while every context
// byte stays put.
//
// A recipe declaring Go module downloads is built against a module proxy the
// plan phase fetched into a per-run directory (goModulePrefetch), whose path is
// normalized out of the key. What the build reads out of that proxy is decided
// by the go.mod and go.sum of each declared module root, so those files are
// hashed directly, by path, outside the ignore policy. Hashing them only as part
// of the context would not do: the prefetch reads them off the filesystem, where
// no ignore policy applies, so a policy that excluded them would drop a real
// input from the key while the prefetch still acted on it. With the manifests
// bound, the proxy's contents follow from them — the module cache verifies every
// downloaded version against go.sum's content hashes, and -mod=readonly makes a
// version missing from go.sum fail the fetch instead of being written in. A
// filesystem `replace` resolves to a directory rather than the proxy, and the
// build can only read that directory if it is in the context, where it is hashed
// like any other file.
//
// A base image is resolved, not taken from its name. A recipe that says
// `FROM golang:1.25` is built from whatever manifest that tag points at when the
// build runs, and the tag is re-pushed whenever the base is patched, so the
// Dockerfile text alone binds the base's *name* and not the base. Each external
// reference is resolved to its manifest digest and the digest goes in the key;
// a reference that cannot be resolved declines the recipe, so a build runs
// rather than reusing an image whose base may have moved underneath it.
//
// What is NOT bound, and cannot be by a digest over inputs: whatever a build
// step fetches from the network itself. A `RUN` that installs from a mutable
// package index reads bytes no input names, so reuse means "the image built from
// these inputs", not "the image a build today would produce". Docker's own layer
// cache has the same property.

const (
	// imageCacheSchema is part of every key, so a change to what the identity
	// binds retires every entry recorded under the old definition instead of
	// matching one computed a different way.
	imageCacheSchema = 1
	// imageCacheDir is the workspace-relative directory entries live in.
	// `.codefly/` is CLI-owned scratch, gitignored by core's composition, and the
	// workspace's own is excluded from a context digest, so writing here cannot
	// perturb the inputs of the next build.
	imageCacheDir = ".codefly/build-cache"
	// imageCacheScratchDir names the CLI-owned directory whose contents are
	// excluded from a context digest. Only the workspace's own is excluded, by
	// absolute path: the name appearing anywhere else in a context belongs to
	// whatever is being built, not to the CLI.
	imageCacheScratchDir = ".codefly"
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
	// GoModules is the digest of the go.mod and go.sum of each Go module root the
	// recipe declares a download for, hashed by path so no ignore policy can
	// drop them.
	GoModules []string `json:"go_module_digests,omitempty"`
	// Bases is the resolved manifest digest of each external image the recipe's
	// Dockerfile builds from.
	Bases []string `json:"base_image_digests,omitempty"`
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

// lookup returns the entry recorded for key, and nothing at all unless the file
// on disk agrees with the build asking for it. The entry is not an interior
// value: it was written by some earlier run, possibly by an older CLI, and
// adoptCachedImage branches on its Pushed field to decide whether to publish a
// registry digest or a daemon image id — so the fields that steer that decision
// are checked here rather than trusted.
func (cache *imageBuildCache) lookup(key, service string, pushed bool) (*imageCacheEntry, bool) {
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
	if entry.Service != service || entry.Pushed != pushed {
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

// imageIdentity is one recipe's resolved image identity: the cache key, and the
// build context digest that went into it, kept so the same tree can be proven
// unchanged after the build without resolving every input a second time.
type imageIdentity struct {
	Key         string
	ContextTree string
}

// imageBuildScope is where one recipe's inputs live. It travels as a unit so the
// identity and the post-build re-check cannot be computed over different trees.
type imageBuildScope struct {
	OutputDir    string
	ContextDir   string
	WorkspaceDir string
}

// contextDigest is the one definition of "the bytes of this recipe's build
// context". Both the identity and the post-build re-check go through it, so
// there is no second reading of the ignore policy to drift from the first.
func contextDigest(ctx context.Context, recipe *builderv0.DockerBuildRecipe, scope imageBuildScope) (string, error) {
	patterns, err := contextIgnorePatterns(scope.OutputDir, scope.ContextDir, recipe)
	if err != nil {
		return "", err
	}
	ignore, err := patternmatcher.New(patterns)
	if err != nil {
		return "", fmt.Errorf("ignore policy of %s: %w", recipe.GetName(), err)
	}
	return contextContentDigest(ctx, scope.ContextDir, ignore, contextExclusions(ctx, scope.ContextDir, scope.OutputDir, scope.WorkspaceDir))
}

// imageBuildIdentity digests the inputs of one recipe's image build. A zero
// identity with no error means the build is not cacheable and must run.
func imageBuildIdentity(ctx context.Context, recipe *builderv0.DockerBuildRecipe, plan *builderv0.DockerBuildPlan, scope imageBuildScope, dockerfilePath, builderName string, args []string, proxies map[string]string) (imageIdentity, error) {
	recipeTree := plan.GetDigest()
	if recipeTree == "" {
		return imageIdentity{}, fmt.Errorf("recipe tree of %s declares no verified content digest", recipe.GetName())
	}
	contextTree, err := contextDigest(ctx, recipe, scope)
	if err != nil {
		return imageIdentity{}, err
	}
	goModules, err := goModuleManifestDigests(scope.ContextDir, recipe)
	if err != nil {
		return imageIdentity{}, err
	}
	bases, err := baseImageDigests(ctx, dockerfilePath, builderName)
	if err != nil {
		return imageIdentity{}, err
	}
	inputs := imageInputs{
		Schema:       imageCacheSchema,
		Recipe:       recipe.GetName(),
		Dockerfile:   recipe.GetDockerfile(),
		Context:      recipe.GetContext(),
		Dockerignore: recipe.GetDockerignore(),
		Command:      normalizedBuildxArgs(args, scope.ContextDir, dockerfilePath, proxies),
		RecipeTree:   recipeTree,
		ContextTree:  contextTree,
		GoModules:    goModules,
		Bases:        bases,
	}
	payload, err := json.Marshal(inputs)
	if err != nil {
		return imageIdentity{}, err
	}
	sum := sha256.Sum256(payload)
	return imageIdentity{Key: "sha256:" + hex.EncodeToString(sum[:]), ContextTree: contextTree}, nil
}

// normalizedBuildxArgs rewrites the invocation into the form described on
// imageInputs.Command: per-run paths become placeholders, and the two flags that
// say where the build runs and where its metadata lands are dropped.
func normalizedBuildxArgs(args []string, contextDir, dockerfilePath string, proxies map[string]string) []string {
	substitutions := make([][2]string, 0, 2+len(proxies))
	substitutions = append(substitutions, [2]string{dockerfilePath, "{dockerfile}"}, [2]string{contextDir, "{context}"})
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

// goModuleManifestDigests binds the dependency declaration of every Go module
// root the recipe declares a download for. The prefetch reads these files off
// the filesystem, so they are hashed by path and not through the context walk:
// an ignore policy that excluded them would otherwise take a live input out of
// the key while the prefetch went on acting on it. proxiesFor already refuses a
// module root outside the build context, and the same containment is required
// here rather than assumed, so a recipe that names an escaping root declines
// instead of hashing something the build cannot read.
func goModuleManifestDigests(contextDir string, recipe *builderv0.DockerBuildRecipe) ([]string, error) {
	downloads := recipe.GetGoModuleDownloads()
	if len(downloads) == 0 {
		return nil, nil
	}
	digests := make([]string, 0, len(downloads))
	for _, download := range downloads {
		root := filepath.Join(contextDir, filepath.FromSlash(download.GetModuleRoot()))
		relative, err := filepath.Rel(contextDir, root)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("recipe %s declares Go module root %q outside its build context", recipe.GetName(), download.GetModuleRoot())
		}
		hasher := sha256.New()
		writeImageCacheRecord(hasher, "go-module-root", filepath.ToSlash(relative))
		for _, name := range []string{"go.mod", "go.sum"} {
			payload, err := os.ReadFile(filepath.Join(root, name))
			switch {
			case os.IsNotExist(err):
				// A module with no dependencies has no go.sum; its absence is
				// itself part of the declaration.
				writeImageCacheRecord(hasher, "absent", name)
			case err != nil:
				return nil, fmt.Errorf("read %s of Go module root %q: %w", name, download.GetModuleRoot(), err)
			default:
				writeImageCacheRecord(hasher, "present", name)
				hasher.Write(payload)
			}
		}
		digests = append(digests, "sha256:"+hex.EncodeToString(hasher.Sum(nil)))
	}
	sort.Strings(digests)
	return digests, nil
}

// baseImageDigests resolves every external image the Dockerfile builds from to
// the manifest digest that reference points at now.
//
// A floating tag is the normal way a recipe names its base, and it is re-pushed
// every time the base is patched, so the Dockerfile text binds the base's name
// and not the base. Resolving it is one registry read against a reference the
// build is about to pull anyway, on the path that decides whether minutes of
// build are skipped.
//
// A reference that names an earlier stage is not external and is skipped; a
// reference the recipe parameterizes, or one that cannot be resolved — an
// unreachable registry, an unauthenticated one, no container engine — makes the
// whole recipe decline, so the build runs.
func baseImageDigests(ctx context.Context, dockerfilePath, builderName string) ([]string, error) {
	references, err := externalBaseReferences(dockerfilePath)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(references))
	for _, reference := range references {
		digest, err := resolveImageManifestDigest(ctx, reference, builderName)
		if err != nil {
			return nil, fmt.Errorf("resolve base image %s: %w", reference, err)
		}
		digests = append(digests, reference+"@"+digest)
	}
	sort.Strings(digests)
	return digests, nil
}

// externalBaseReferences lists the image references a Dockerfile builds FROM,
// excluding `scratch` and any reference naming a stage the same file defines.
func externalBaseReferences(dockerfilePath string) ([]string, error) {
	commands, err := dockerfile.ParseFile(dockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", dockerfilePath, err)
	}
	stages := map[string]bool{}
	seen := map[string]bool{}
	var references []string
	for _, command := range commands {
		if !strings.EqualFold(command.Cmd, "from") || len(command.Value) == 0 {
			continue
		}
		reference := command.Value[0]
		// `FROM <image> AS <stage>` names a stage later references may use.
		if len(command.Value) >= 3 && strings.EqualFold(command.Value[1], "as") {
			stages[strings.ToLower(command.Value[2])] = true
		}
		if strings.EqualFold(reference, "scratch") || stages[strings.ToLower(reference)] {
			continue
		}
		if strings.ContainsAny(reference, "$") {
			return nil, fmt.Errorf("%s builds FROM %q, whose value this build does not resolve", dockerfilePath, reference)
		}
		if seen[reference] {
			continue
		}
		seen[reference] = true
		references = append(references, reference)
	}
	return references, nil
}

// resolveImageManifestDigest fingerprints what a reference resolves to, by
// hashing the manifest the registry serves for it.
//
// It hashes the raw manifest rather than reading a digest out of a rendered
// field, because the key needs a value that moves when the image behind the
// reference moves and nothing more — and hashing bytes the registry returned
// depends on no output format staying the way it is. An empty answer is an
// error: a fingerprint over nothing would be a constant, and a constant in a key
// is an input that stopped being checked.
//
// It is a seam so a test can bind a base without a registry.
var resolveImageManifestDigest = func(ctx context.Context, reference, builderName string) (string, error) {
	probe, cancel := context.WithTimeout(ctx, imageRegistryProbeTimeout)
	defer cancel()
	args := []string{buildxCommand, "imagetools", "inspect"}
	if builderName != "" {
		args = append(args, "--builder", builderName)
	}
	args = append(args, "--raw", reference)
	manifest, err := exec.CommandContext(probe, "docker", args...).Output()
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(manifest)) == 0 {
		return "", fmt.Errorf("registry served no manifest for %s", reference)
	}
	sum := sha256.Sum256(manifest)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// imageRegistryProbeTimeout bounds every registry read the reuse decision makes.
// A registry that accepts a connection and then stalls would otherwise hang a
// check whose whole purpose is to be cheaper than the build it replaces.
const imageRegistryProbeTimeout = 30 * time.Second

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
// binds: the recipe archive, which is a copy of the emitted recipe tree keyed by
// agent version (so recording it after a build would otherwise invalidate the
// identity of the build that produced it — a rebuild on every second run,
// forever), and the workspace's own CLI-owned scratch, which is where these very
// records live.
func contextExclusions(ctx context.Context, contextDir, outputDir string, workspaceDir string) map[string]bool {
	excluded := map[string]bool{}
	for _, path := range []string{
		filepath.Join(resolvedDir(ctx, filepath.Dir(outputDir)), buildRecipeArchiveDir),
		filepath.Join(resolvedDir(ctx, workspaceDir), imageCacheScratchDir),
	} {
		if relative, ok := containedRelativePath(contextDir, path); ok {
			excluded[relative] = true
		}
	}
	return excluded
}

// containedRelativePath is path's context-relative, slash-separated spelling
// when it lies inside contextDir.
func containedRelativePath(contextDir, path string) (string, bool) {
	if path == "" || contextDir == "" {
		return "", false
	}
	relative, err := filepath.Rel(contextDir, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

// resolvedDir is dir with its symlinks resolved. contextDir has had its resolved
// by prepareRecipeContext, so anything compared against it must be resolved the
// same way — on a host whose temporary or home directory is a link, the
// unresolved spelling reads as outside the context and excludes nothing, which
// silently restores the every-second-build churn these exclusions exist to
// prevent. A path that cannot be resolved is reported rather than passed over.
func resolvedDir(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		wool.Get(ctx).In("orchestration.resolvedDir").Debug("cannot resolve a path the context digest excludes; it will be hashed",
			wool.DirField(dir), wool.ErrField(err))
		return dir
	}
	return resolved
}

// contextContentDigest hashes every byte of the build context Docker would send.
// Traversal is lexical and records each entry's kind, so an added, removed,
// renamed, retyped or edited file all move the digest.
func contextContentDigest(ctx context.Context, contextDir string, ignore *patternmatcher.PatternMatcher, excluded map[string]bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
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
		if excluded[slashed] {
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
func imageStillExists(ctx context.Context, entry *imageCacheEntry, builderName string) bool {
	if entry.Pushed {
		reference, ok := digestReference(entry.Image, entry.Digest)
		if !ok {
			return false
		}
		_, err := resolveImageManifestDigest(ctx, reference, builderName)
		return err == nil
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
	if !sha256Digest.MatchString(digest) || image == "" || strings.Contains(image, "@") {
		return "", false
	}
	repository := image
	last := image
	if separator := strings.LastIndex(image, "/"); separator >= 0 {
		last = image[separator+1:]
	}
	// Only the final path component may carry a tag; a colon before it belongs to
	// a registry port. A trailing component that is not a valid tag is left
	// alone rather than truncated at a colon that means something else.
	if colon := strings.LastIndex(last, ":"); colon >= 0 {
		if !imageTag.MatchString(last[colon+1:]) {
			return "", false
		}
		repository = image[:len(image)-(len(last)-colon)]
	}
	if repository == "" {
		return "", false
	}
	return repository + "@" + digest, true
}

// imageTag is Docker's tag grammar: what may legitimately follow the colon of
// the final path component.
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,127}$`)

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
