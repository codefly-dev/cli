package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// baseCompanionName is the companion every other companion builds on: it
// is built first and its failure aborts the run.
const baseCompanionName = "codefly"

// baseImageArg is the build argument a companion Dockerfile declares for the
// codefly companion image it builds on. core owns the Dockerfile and names the
// dependency; this CLI is the builder core's BuildSpec.Base delegates to, and
// resolving that name to a reference is this side of the contract.
//
// The Dockerfiles carry a default (ARG CODEFLY_BASE_IMAGE=...:0.0.4) so a bare
// `docker build` works. That default is a pinned literal, so a build that does
// not pass this argument silently bakes in whatever base the Dockerfile was
// last edited against, no matter which version info.codefly.yaml pins now.
const baseImageArg = "CODEFLY_BASE_IMAGE"

// BuildCmd builds one or more companion Docker images.
//
// Order: when --all is in play, the codefly base image is built FIRST.
// Language companions (go, python, node) COPY --from the codefly base
// image's tag the cross-compiled CLI; if base isn't built first their
// build fails.
// This is the same constraint build_companions.sh enforced; we encode
// it here in code rather than relying on script ordering.
var BuildCmd = &cobra.Command{
	Use:   "build [name]",
	Short: "Build one or all companion images from the core repository",
	Long: `Build builds a companion image from its directory.

With a name argument, builds just that companion. With --all, builds
every companion under <core>/companions/, in dependency order (codefly
base image first, then language runtimes, then dev tooling).

A companion is a directory under core/companions/ with an
info.codefly.yaml (declaring version) and either a Dockerfile, a
flake.nix, or both. When flake.nix is present AND nix is installed,
the flake build is preferred (reproducible, layered cache).

The image tag is derived from <name> and the version in info.codefly.yaml;
see Companion.Tag.

Examples:
  codefly companion build proto
  codefly companion build --all
  codefly companion build go --push                    # build then push
  codefly companion build --all --core-dir ./core      # explicit anchor`,
	RunE: runBuild,
}

func init() {
	BuildCmd.Flags().Bool("all", false, "Build every companion under <core>/companions/")
	BuildCmd.Flags().String("core-dir", "", "Path to the core directory (default: walk up from cwd looking for companions/)")
	BuildCmd.Flags().Bool("push", false, "Push each image to the registry after a successful build")
	BuildCmd.Flags().Bool("force-docker", false, "Skip the flake.nix path even when present + nix is installed")
	BuildCmd.Flags().Bool("pull", false, "Always pull a newer base image (docker build --pull) — picks up upstream patch releases (e.g. golang:1.26-alpine → latest 1.26.x)")
	BuildCmd.Flags().String("platform", "", "Target platform(s) for Docker builds (e.g. linux/amd64 or linux/amd64,linux/arm64). Multiple platforms require --push.")
}

// BuildOptions controls a companion build run. Shared by the `companion
// build` command and by `codefly update deps --companions`.
type BuildOptions struct {
	Push        bool
	ForceDocker bool
	// Pull forces `docker build --pull` so a floating base tag
	// (golang:1.26-alpine, …) is refreshed to its latest patch. This is
	// how `update deps` clears base-image CVEs like the Go stdlib bumps.
	Pull bool
	// Platform overrides the Docker build target. A comma-separated list
	// publishes one multi-platform manifest through buildx. Empty means the
	// host Linux architecture. Ignored by the Nix build path.
	Platform string
}

// BuildAll builds every companion under coreDir/companions in dependency
// order (codefly base first). Reusable entry point so `update deps` can
// rebuild the whole companion set with fresh base images.
func BuildAll(coreDir string, opts BuildOptions) error {
	targets, err := listCompanionsRequired(coreDir)
	if err != nil {
		return err
	}
	targets = sortCompanionsForBuild(targets)
	_, err = buildTargets(coreDir, targets, opts)
	return err
}

func runBuild(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
	push, _ := cmd.Flags().GetBool("push")
	forceDocker, _ := cmd.Flags().GetBool("force-docker")
	pull, _ := cmd.Flags().GetBool("pull")
	platform, _ := cmd.Flags().GetString("platform")

	coreDir, err := resolveCoreDir(coreDirFlag)
	if err != nil {
		return err
	}
	if !all && len(args) == 0 {
		return fmt.Errorf("must specify a companion name or --all")
	}
	targets, err := selectTargets(coreDir, all, args)
	if err != nil {
		return err
	}

	opts := BuildOptions{Push: push, ForceDocker: forceDocker, Pull: pull, Platform: platform}
	_, err = buildTargets(coreDir, targets, opts)
	return err
}

// resolveCoreDir turns the --core-dir flag (or, when empty, an upward walk
// from cwd) into an absolute core directory, and validates that its
// companions/ subdirectory exists. Shared by build, publish, and verify so
// they agree on how the tree is located.
func resolveCoreDir(coreDirFlag string) (string, error) {
	coreDir := coreDirFlag
	if coreDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot read working directory: %w", err)
		}
		coreDir = FindCompanionsRoot(cwd)
	}
	companionsDir := filepath.Join(coreDir, "companions")
	if info, err := os.Stat(companionsDir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("companions directory not found at %s; pass --core-dir or run from within the codefly.dev tree", companionsDir)
	}
	return coreDir, nil
}

// selectTargets resolves the companions to act on: a single named companion
// (args[0]), or every companion under coreDir when all is true. The --all
// set is returned in dependency-build order (codefly base first) so a
// build/publish run over the whole set can't fail on an unbuilt base image.
func selectTargets(coreDir string, all bool, args []string) ([]*Companion, error) {
	if all {
		if len(args) > 0 {
			return nil, fmt.Errorf("cannot combine --all with a companion name (%q)", args[0])
		}
		targets, err := listCompanionsRequired(coreDir)
		if err != nil {
			return nil, err
		}
		return sortCompanionsForBuild(targets), nil
	}
	c, err := LoadCompanion(filepath.Join(coreDir, "companions", args[0]))
	if err != nil {
		return nil, fmt.Errorf("cannot load companion %q: %w", args[0], err)
	}
	return []*Companion{c}, nil
}

// buildTargets builds the given companions in the order provided. It
// cross-compiles the linux CLI once up front when any target needs it
// (language companions COPY it), and resolves the base companion reference
// for any dependent that takes one.
//
// It returns the companions it actually built and pushed alongside any error,
// including on a partial failure: the caller records that set for provenance
// attestation, and losing it would either drop provenance for images that
// were published or invite attesting the whole declared set instead.
func buildTargets(coreDir string, targets []*Companion, opts BuildOptions) ([]publishedImage, error) {
	platforms, err := resolveDockerPlatforms(opts.Platform)
	if err != nil {
		return nil, err
	}
	multiPlatform := len(platforms) > 1
	if multiPlatform && !opts.Push {
		return nil, fmt.Errorf("multi-platform companion builds require --push")
	}

	// Companion Dockerfiles select bin/linux/$TARGETARCH/codefly. Produce
	// one binary per requested architecture before BuildKit evaluates the
	// platform-specific stages.
	if needsLinuxCLI(targets) {
		for _, platform := range platforms {
			if err := buildLinuxCLI(coreDir, platform.Arch); err != nil {
				return nil, fmt.Errorf("cross-compile codefly CLI for %s: %w", platform.Value, err)
			}
		}
	}

	base := newBaseResolver(coreDir, targets, opts.Push)

	// Push failures are collected rather than returned immediately. A first
	// push of a new companion lands in a ghcr package that is private until
	// someone flips it in the UI, so aborting the loop there would leave the
	// rest of the fleet unpublished over a condition that has nothing to do
	// with them. Build failures of a leaf companion are collected for the same
	// reason: nothing is built from them, so one flaky apk mirror must not
	// strand every companion ordered after it.
	var pushFailures []string
	var buildFailures []string
	// Companions this run actually built and pushed. Provenance attestation
	// must cover these and nothing else: a tag skipped as already published
	// was produced by some earlier run, and signing it here would attribute
	// a build to this one that never happened.
	var published []publishedImage

	for _, c := range targets {
		// Skip companions that aren't built as images. A directory with an
		// info.codefly.yaml but neither a Dockerfile nor a flake.nix (e.g.
		// the `golang` Go-package companion) is discovered by ListCompanions
		// but has no image to produce — building it would hard-fail.
		if !c.HasDockerfile && !c.HasFlake {
			fmt.Printf("==> Skipping %s (no Dockerfile or flake.nix — not an image companion)\n", c.Name)
			continue
		}
		method := "docker"
		if c.HasFlake && !opts.ForceDocker && nixOnPath() {
			method = "nix"
		}
		if multiPlatform && method == "nix" {
			return published, fmt.Errorf("multi-platform companion %s must use Docker; pass --force-docker", c.Name)
		}
		fmt.Printf("==> Building %s (%s) via %s\n", c.Tag(), c.Dir, method)

		// companionBase is recorded alongside the image this iteration publishes:
		// it is the input a dependent's provenance turns on, and re-deriving it
		// after the run means re-reading a tag that has since moved.
		companionBase := ""
		buildDigest := ""
		var buildErr error
		switch method {
		case "nix":
			buildErr = buildWithNix(c)
		default:
			if companionBase, err = base.referenceFor(c); err != nil {
				return published, err
			}
			buildDigest, buildErr = buildWithDocker(c, coreDir, opts.Pull, platforms, opts.Push, companionBase)
		}
		if buildErr != nil {
			// The base is the one build whose failure invalidates what
			// follows: every other companion resolves it as their FROM.
			if isBaseCompanion(c.Name) {
				return published, fmt.Errorf("build %s failed, and every other companion builds on it: %w", c.Name, buildErr)
			}
			fmt.Printf("    FAILED   %s\n", c.Tag())
			buildFailures = append(buildFailures, fmt.Sprintf("%s: %v", c.Name, buildErr))
			continue
		}
		fmt.Printf("    built %s\n", c.Tag())

		// A multi-platform build is pushed atomically by buildx; there is no
		// single local image for `docker push` to publish afterward.
		if opts.Push && !(method == "docker" && multiPlatform) {
			pushDigest, pushErr := pushImage(c.Tag())
			// A digest means the upload landed, which pushImage reports even
			// when it goes on to fail the visibility check. Dependents are owed
			// the image that is now in the registry either way; the failure is
			// still collected below and still fails the run.
			buildDigest = pushDigest
			base.recordPush(c, buildDigest)
			if pushErr != nil {
				fmt.Printf("    FAILED   %s\n", c.Tag())
				pushFailures = append(pushFailures, fmt.Sprintf("%s: %v", c.Name, pushErr))
				continue
			}
			fmt.Printf("    pushed %s\n", c.Tag())
		} else if opts.Push {
			// buildx pushed the manifest list itself and reported its digest.
			base.recordPush(c, buildDigest)
		}
		if opts.Push {
			published = append(published, publishedImage{Companion: c, Base: companionBase, Digest: buildDigest})
		}
	}

	failures := make([]string, 0, len(buildFailures)+len(pushFailures))
	failures = append(failures, buildFailures...)
	failures = append(failures, pushFailures...)
	if len(failures) > 0 {
		return published, fmt.Errorf("%d companion build/push(es) failed:\n  %s",
			len(failures), strings.Join(failures, "\n  "))
	}
	return published, nil
}

// publishedImage is one companion this run built and pushed, together with the
// base reference it was built against and the digest the registry now holds it
// under. The base is a load-bearing input to the image's provenance and cannot
// be recovered after the run: re-deriving it means re-reading a mutable tag.
type publishedImage struct {
	Companion *Companion
	Base      string
	Digest    string
}

// baseResolver hands each dependent the reference for the base companion it
// builds on. It resolves that reference once per run, and remembers what this
// run itself published for the base, because that digest — not the tag it went
// under — is what a dependent must be pinned to.
type baseResolver struct {
	coreDir string
	pushing bool
	// inTargets records that this run set out to publish the base, which is
	// what makes a missing push a refusal rather than a lookup.
	inTargets bool

	pushed       bool
	pushedDigest string

	// resolved is tracked separately from a non-empty reference: "not yet
	// resolved" and "resolved" are different states, and only one may look up.
	resolved  bool
	reference string
}

func newBaseResolver(coreDir string, targets []*Companion, pushing bool) *baseResolver {
	r := &baseResolver{coreDir: coreDir, pushing: pushing}
	for _, c := range targets {
		if isBaseCompanion(c.Name) && c.ProducesImage() {
			r.inTargets = true
			break
		}
	}
	return r
}

// recordPush remembers the digest this run put in the registry for the base.
// Called at the point each publishing path learns it — the buildx metadata for
// a multi-platform manifest, docker push's own report otherwise.
func (r *baseResolver) recordPush(c *Companion, digest string) {
	if !isBaseCompanion(c.Name) || digest == "" {
		return
	}
	r.pushed = true
	r.pushedDigest = digest
}

// referenceFor returns the base reference c must build on, empty when c takes
// none — the base builds itself, and a companion whose Dockerfile declares no
// base argument would only collect an unconsumed one.
func (r *baseResolver) referenceFor(c *Companion) (string, error) {
	if isBaseCompanion(c.Name) {
		return "", nil
	}
	declares, err := declaresBaseImageArg(c)
	if err != nil || !declares {
		return "", err
	}
	if r.resolved {
		return r.reference, nil
	}
	// A run that is publishing the base owes its dependents that base and no
	// other. Reading the registry here instead would answer with whatever the
	// tag names now, which — if this run failed to push it — is the previous
	// base, published by a run that never built these dependents.
	if r.pushing && r.inTargets && !r.pushed {
		return "", fmt.Errorf(
			"%s builds on the %s companion, which this run was publishing but did not put in the registry; refusing to build it on the base already there",
			c.Name, baseCompanionName)
	}
	reference, err := resolveBaseImage(r.coreDir, r.pushing, r.pushedDigest)
	if err != nil {
		return "", fmt.Errorf("%s builds on the %s companion: %w", c.Name, baseCompanionName, err)
	}
	r.reference = reference
	r.resolved = true
	return reference, nil
}

// isBaseCompanion reports whether other companions build on this one. It is
// the same "codefly first" fact sortCompanionsForBuild orders by, named once
// so the ordering and the abort rule cannot disagree.
func isBaseCompanion(name string) bool {
	return name == baseCompanionName
}

// declaresBaseImageArg reports whether this companion's Dockerfile takes the
// base companion image as a build argument. Only those builds need the
// resolved reference passed, and only they are broken by its absence.
func declaresBaseImageArg(c *Companion) (bool, error) {
	if !c.HasDockerfile {
		return false, nil
	}
	content, err := os.ReadFile(filepath.Join(c.Dir, "Dockerfile"))
	if err != nil {
		return false, fmt.Errorf("read %s Dockerfile: %w", c.Name, err)
	}
	return strings.Contains(string(content), baseImageArg), nil
}

// resolveBaseImage returns the reference for the base companion at the version
// currently pinned in its info.codefly.yaml.
//
// It reads the base off disk rather than out of the target list on purpose:
// the base is routinely absent from targets — a single named build, or a
// publish run that skipped it as already published — and its dependents still
// have to be built against it. Taking it from the targets would silently fall
// back to the Dockerfile's pinned default in exactly those runs.
//
// It names that base by digest whenever the run publishes, because a tag is
// mutable: what a dependent bakes in would otherwise depend on where the tag
// pointed the moment its build ran, and anything with write access to the
// package can move it in between.
//
// pushedDigest is what this run itself put in the registry for the base, and is
// preferred over any lookup — it is the only source that cannot be overtaken.
// It is empty when the base was not this run's to publish (a single named
// dependent build, or a publish run that skipped the base as already
// published), and only then is the tag read back to find the digest it names.
//
// A run that does not publish keeps the tag. It has no digest to offer: nothing
// went to a registry, and the base such a run's dependents resolve is whatever
// the tag names locally — the image just built, or, under --pull, the one the
// registry currently serves.
func resolveBaseImage(coreDir string, pushing bool, pushedDigest string) (string, error) {
	base, err := LoadCompanion(filepath.Join(coreDir, "companions", baseCompanionName))
	if err != nil {
		return "", fmt.Errorf("resolve the %s base image every other companion builds on: %w", baseCompanionName, err)
	}
	if pushedDigest != "" {
		return repoPath(base.Tag()) + "@" + pushedDigest, nil
	}
	if !pushing {
		return base.Tag(), nil
	}
	digest, err := imageDigest(base.Tag())
	if err != nil {
		return "", err
	}
	return repoPath(base.Tag()) + "@" + digest, nil
}

// imageDigest resolves a reference to the digest of the manifest it currently
// points at, via buildx because a multi-platform companion is a manifest list
// and the list digest is what a dependent's FROM needs — it is what selects the
// per-architecture image. `docker manifest inspect` reports the members
// instead, and pinning one of those would build every dependent, on every
// architecture, on the base for a single arch.
func imageDigest(ref string) (string, error) {
	for attempt := 1; ; attempt++ {
		digest, err := inspectImageDigest(ref)
		if err == nil || attempt >= registryReadAttempts {
			return digest, err
		}
		time.Sleep(registryReadDelay)
	}
}

// registryReadAttempts/registryReadDelay retry the digest lookup above. It is
// the one registry read a publish run cannot route around: it happens between
// the base being published and its dependents being built, in a job that has
// already mutated the registry, so a single 5xx or rate-limited response would
// abort the run half-published. The other registry read in this package
// (anonymousManifestInspectRetrying) retries for the same reason. Package vars
// so tests can shrink the delay.
var (
	registryReadAttempts = 3
	registryReadDelay    = time.Second
)

func inspectImageDigest(ref string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", "buildx", "imagetools", "inspect", ref, "--format", "{{ .Manifest.Digest }}")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("resolve the digest of %s: %w: %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	// buildx renders an unresolvable field as its Go zero value and still exits
	// 0, so a digest is only a digest once it looks like one; the alternative
	// is a --build-arg that reads as a reference and resolves to nothing.
	digest := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(digest, "sha256:") {
		return "", fmt.Errorf("resolve the digest of %s: docker reported %q, which is not a digest", ref, digest)
	}
	return digest, nil
}

// listCompanionsRequired lists every companion under root. The bare
// ListCompanions returns (nil, nil) when no
// companions/ exists; that case can't happen here because we already
// validated the directory.
func listCompanionsRequired(root string) ([]*Companion, error) {
	cs, err := ListCompanions(root)
	if err != nil {
		return nil, fmt.Errorf("scan companions: %w", err)
	}
	if len(cs) == 0 {
		return nil, fmt.Errorf("no companions found under %s/companions/", root)
	}
	return cs, nil
}

// sortCompanionsForBuild orders companions for --all: codefly base
// first (others COPY --from it), then language runtimes (alphabetical
// among themselves), then everything else. The original
// build_companions.sh hard-coded this order in its script body; we
// derive it from the names so adding a new companion doesn't require
// editing this function unless it has unusual ordering needs.
func sortCompanionsForBuild(in []*Companion) []*Companion {
	priority := func(name string) int {
		switch name {
		case baseCompanionName:
			return 0
		case "go", "python", "node":
			return 1
		default:
			return 2
		}
	}
	out := make([]*Companion, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := priority(out[i].Name), priority(out[j].Name)
		if pi != pj {
			return pi < pj
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// needsLinuxCLI returns true when at least one target companion is a
// language or execution runtime — those embed the cross-compiled CLI,
// so we must produce a fresh binary for each target architecture. The codefly base
// companion itself ALSO needs the binary (it's the source of the
// COPY --from used by language images), so we treat it as needing the
// CLI too.
func needsLinuxCLI(targets []*Companion) bool {
	for _, c := range targets {
		switch c.Name {
		case baseCompanionName, "go", "python", "node", "execution":
			return true
		}
	}
	return false
}

// buildLinuxCLI cross-compiles the codefly CLI for one Linux architecture
// with symbol stripping. Output: <core>/bin/linux/<arch>/codefly.
//
// CGO disabled + -extldflags "-static" produces a fully static binary
// the alpine images can run without glibc. Flag stripping (-s -w)
// drops the symbol table and DWARF info; ~25-30% size reduction with
// no runtime cost.
//
// The `codefly_nosemantic` tag drops the tree-sitter semantic analyzer, which
// cannot link without cgo. It is required (not merely implied by CGO_ENABLED=0):
// without it this CGO-free build would select the analyzer variant and fail to
// link. The companion CLI only builds and runs services in-container and never
// serves the semantic gateway, so the analyzer is not needed here. See
// pkg/engine/source_nosemantic.go.
func buildLinuxCLI(coreDir, arch string) error {
	cliDir := filepath.Join(coreDir, "..", "cli")
	if info, err := os.Stat(cliDir); err != nil || !info.IsDir() {
		return fmt.Errorf("cli directory not found at %s (expected sibling of core/)", cliDir)
	}
	outBin := filepath.Join(coreDir, "bin", "linux", arch, "codefly")
	if err := os.MkdirAll(filepath.Dir(outBin), 0o755); err != nil {
		return fmt.Errorf("create bin/linux/%s: %w", arch, err)
	}

	cmd := exec.Command("go", "build",
		"-tags", "codefly_nosemantic",
		"-ldflags", `-s -w -extldflags "-static"`,
		"-o", outBin,
		".",
	)
	cmd.Dir = cliDir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+arch,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type dockerPlatform struct {
	Value string
	Arch  string
}

func resolveDockerPlatforms(value string) ([]dockerPlatform, error) {
	if strings.TrimSpace(value) == "" {
		value = "linux/" + dockerArch()
	}
	raw := strings.Split(value, ",")
	platforms := make([]dockerPlatform, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, candidate := range raw {
		candidate = strings.TrimSpace(candidate)
		parts := strings.Split(candidate, "/")
		if len(parts) != 2 || parts[0] != "linux" ||
			(parts[1] != "amd64" && parts[1] != "arm64") {
			return nil, fmt.Errorf("unsupported companion platform %q; expected linux/amd64 or linux/arm64", candidate)
		}
		if _, duplicate := seen[candidate]; duplicate {
			return nil, fmt.Errorf("duplicate companion platform %q", candidate)
		}
		seen[candidate] = struct{}{}
		platforms = append(platforms, dockerPlatform{Value: candidate, Arch: parts[1]})
	}
	return platforms, nil
}

// buildWithDocker uses the ordinary daemon build for one platform and an
// atomic buildx manifest push for multiple platforms.
//
// A multi-platform build returns the digest buildx recorded for the manifest
// list it pushed. That push is the only place this run can learn what it
// published: buildx pushes atomically, so nothing else in the run touches the
// image, and the tag it was pushed under is mutable afterwards. Every other
// build returns an empty digest — a single-platform image is published by
// pushImage, which reports its own.
func buildWithDocker(c *Companion, coreDir string, pull bool, platforms []dockerPlatform, push bool, baseImage string) (string, error) {
	if !c.HasDockerfile {
		return "", fmt.Errorf("no Dockerfile in %s", c.Dir)
	}
	platformValues := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		platformValues = append(platformValues, platform.Value)
	}
	dockerfile := filepath.Join("companions", c.Name, "Dockerfile")

	dockerArgs := []string{"build", "--platform", strings.Join(platformValues, ",")}
	if len(platforms) > 1 {
		dockerArgs = []string{"buildx", "build", "--platform", strings.Join(platformValues, ",")}
	}
	if pull {
		// --pull re-resolves floating base tags (golang:1.26-alpine) to the
		// latest patch, so a rebuild clears upstream base-image CVEs instead
		// of reusing the cached older layer.
		dockerArgs = append(dockerArgs, "--pull")
	}
	if baseImage != "" {
		// Without this the Dockerfile's own ARG default wins, and that default
		// is a pinned literal: bumping companions/codefly/info.codefly.yaml
		// would publish dependents still built on the previous base, with
		// nothing in build, push or verify reporting the mismatch.
		dockerArgs = append(dockerArgs, "--build-arg", baseImageArg+"="+baseImage)
	}
	dockerArgs = append(dockerArgs, "-f", dockerfile, "-t", c.Tag())
	metadataFile := ""
	if len(platforms) > 1 {
		if !push {
			return "", fmt.Errorf("multi-platform companion builds require push")
		}
		metadata, err := os.CreateTemp("", "codefly-companion-metadata-*.json")
		if err != nil {
			return "", fmt.Errorf("create build metadata file for %s: %w", c.Name, err)
		}
		metadata.Close()
		metadataFile = metadata.Name()
		defer os.Remove(metadataFile)
		dockerArgs = append(dockerArgs, "--push", "--metadata-file", metadataFile)
	}
	dockerArgs = append(dockerArgs, ".")
	cmd := exec.Command("docker", dockerArgs...)
	cmd.Dir = coreDir // build context is core/
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if metadataFile == "" {
		return "", nil
	}
	return buildMetadataDigest(metadataFile), nil
}

// buildMetadataDigest reads the digest buildx wrote to its --metadata-file for
// the manifest it pushed. Empty when the file carries no usable digest; as with
// pushImage, only a caller that needs the pin can judge whether that is fatal.
func buildMetadataDigest(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var metadata struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	if !strings.HasPrefix(metadata.Digest, "sha256:") {
		return ""
	}
	return metadata.Digest
}

// buildWithNix runs `nix build .#dockerImage` in the companion's dir
// and loads the resulting image into the local docker daemon. This
// is the path the proto companion's flake.nix is set up for.
//
// Two-step process:
//  1. `nix build .#dockerImage` produces ./result, an OCI tarball
//  2. `docker load < result` registers the image with the local
//     daemon under whatever tag the flake produced
//
// We don't currently re-tag — the flake is responsible for setting
// the tag via streamLayeredImage's name argument (matching Companion.Tag).
// If the user wants a different tag, they should edit the flake.
func buildWithNix(c *Companion) error {
	build := exec.Command("nix", "build",
		"--extra-experimental-features", "nix-command flakes",
		".#dockerImage",
	)
	build.Dir = c.Dir
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("nix build: %w", err)
	}

	resultPath := filepath.Join(c.Dir, "result")
	resultFile, err := os.Open(resultPath)
	if err != nil {
		return fmt.Errorf("open nix build result: %w", err)
	}
	defer resultFile.Close()

	load := exec.Command("docker", "load")
	load.Stdin = resultFile
	load.Stdout = os.Stdout
	load.Stderr = os.Stderr
	if err := load.Run(); err != nil {
		return fmt.Errorf("docker load: %w", err)
	}
	return nil
}

// nixOnPath reports whether `nix` is available — controls whether the
// flake-build path is even considered. We don't fail when nix is
// missing; we just fall back to docker.
func nixOnPath() bool {
	_, err := exec.LookPath("nix")
	return err == nil
}

// dockerArch maps Go's runtime.GOARCH to Docker's --platform vocab.
// build_companions.sh used `uname -m | sed`; Go's runtime.GOARCH is
// already in Docker's preferred form for amd64 and arm64.
func dockerArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	default:
		return runtime.GOARCH
	}
}

// silence unused-import warnings during early scaffolding when
// context isn't yet referenced. Removed once a future flag wires
// per-call cancellation in.
var _ = context.Background
var _ = strings.TrimSpace
