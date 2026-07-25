package companion

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// BuildCmd builds one or more companion Docker images.
//
// Order: when --all is in play, the codefly base image is built FIRST.
// Language companions (go, python, node) COPY --from=codeflydev/codefly
// the cross-compiled CLI; if base isn't built first their build fails.
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

The image tag is codeflydev/<name>:<version-from-info.codefly.yaml>.

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
	return buildTargets(coreDir, targets, opts)
}

func runBuild(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
	push, _ := cmd.Flags().GetBool("push")
	forceDocker, _ := cmd.Flags().GetBool("force-docker")
	pull, _ := cmd.Flags().GetBool("pull")

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

	opts := BuildOptions{Push: push, ForceDocker: forceDocker, Pull: pull}
	return buildTargets(coreDir, targets, opts)
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
// (language companions COPY it). Returns the first build/push error.
func buildTargets(coreDir string, targets []*Companion, opts BuildOptions) error {
	// When a language companion is in the set it COPYs a linux/amd64
	// codefly binary; produce it once up front so the build isn't broken
	// by a stale binary. Cheap to skip when no language companion is
	// targeted.
	if needsLinuxCLI(targets) {
		if err := buildLinuxCLI(coreDir); err != nil {
			return fmt.Errorf("cross-compile codefly CLI for linux/amd64: %w", err)
		}
	}

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
		fmt.Printf("==> Building %s (%s) via %s\n", c.Tag(), c.Dir, method)

		var buildErr error
		switch method {
		case "nix":
			buildErr = buildWithNix(c)
		default:
			buildErr = buildWithDocker(c, coreDir, opts.Pull)
		}
		if buildErr != nil {
			return fmt.Errorf("build %s failed: %w", c.Name, buildErr)
		}
		fmt.Printf("    built %s\n", c.Tag())

		if opts.Push {
			if err := pushImage(c.Tag()); err != nil {
				return fmt.Errorf("push %s failed: %w", c.Name, err)
			}
			fmt.Printf("    pushed %s\n", c.Tag())
		}
	}
	return nil
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
		case "codefly":
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
// so we must produce a fresh linux/amd64 binary. The codefly base
// companion itself ALSO needs the binary (it's the source of the
// COPY --from used by language images), so we treat it as needing the
// CLI too.
func needsLinuxCLI(targets []*Companion) bool {
	for _, c := range targets {
		switch c.Name {
		case "codefly", "go", "python", "node", "execution":
			return true
		}
	}
	return false
}

// buildLinuxCLI cross-compiles the codefly CLI for linux/amd64 with
// symbol stripping. Output: <core>/bin/linux/codefly. Mirrors the
// in-line invocation at the top of build_companions.sh.
//
// CGO disabled + -extldflags "-static" produces a fully static binary
// the alpine images can run without glibc. Flag stripping (-s -w)
// drops the symbol table and DWARF info; ~25-30% size reduction with
// no runtime cost.
func buildLinuxCLI(coreDir string) error {
	cliDir := filepath.Join(coreDir, "..", "cli")
	if info, err := os.Stat(cliDir); err != nil || !info.IsDir() {
		return fmt.Errorf("cli directory not found at %s (expected sibling of core/)", cliDir)
	}
	outBin := filepath.Join(coreDir, "bin", "linux", "codefly")
	if err := os.MkdirAll(filepath.Dir(outBin), 0o755); err != nil {
		return fmt.Errorf("create bin/linux: %w", err)
	}

	cmd := exec.Command("go", "build",
		"-ldflags", `-s -w -extldflags "-static"`,
		"-o", outBin,
		".",
	)
	cmd.Dir = cliDir
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH=amd64",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildWithDocker invokes `docker build` with the host platform.
// build_companions.sh does the same — multi-arch publish is a
// separate pipeline (docker buildx push).
//
// Uses host-arch (uname -m mapped to amd64/arm64) so dev iterations
// are fast; CI matrices that need cross-arch must opt in via
// `docker buildx`. We deliberately don't try to be clever about that
// here — the sh path was already explicit about it.
func buildWithDocker(c *Companion, coreDir string, pull bool) error {
	if !c.HasDockerfile {
		return fmt.Errorf("no Dockerfile in %s", c.Dir)
	}
	platform := "linux/" + dockerArch()
	dockerfile := filepath.Join("companions", c.Name, "Dockerfile")

	dockerArgs := []string{"build", "--platform", platform}
	if pull {
		// --pull re-resolves floating base tags (golang:1.26-alpine) to the
		// latest patch, so a rebuild clears upstream base-image CVEs instead
		// of reusing the cached older layer.
		dockerArgs = append(dockerArgs, "--pull")
	}
	dockerArgs = append(dockerArgs, "-f", dockerfile, "-t", c.Tag(), ".")
	cmd := exec.Command("docker", dockerArgs...)
	cmd.Dir = coreDir // build context is core/
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
// the tag to codeflydev/<name>:<version> via streamLayeredImage's
// name argument. If the user wants a different tag, they should
// edit the flake.
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

// pushImage runs `docker push <tag>`. Used by --push and by the
// standalone PushCmd. Same effect as push_companion.sh.
func pushImage(tag string) error {
	cmd := exec.Command("docker", "push", tag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
