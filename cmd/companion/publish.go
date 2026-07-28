package companion

import (
	"fmt"

	"github.com/spf13/cobra"
)

// PublishCmd builds and pushes companion images at the version pinned in
// each companion's info.codefly.yaml. It exists so there is one command
// whose job is to make every tag core embeds actually present in the
// registry — `build --push` does the same for one image, but publishing
// the whole set is the operation CI and release automation care about.
//
// core embeds companion tags (codeflydev/<name>:<version>) via each
// companion's info.codefly.yaml and agents pull those exact tags at
// runtime; if a pinned tag was never pushed, Codefly-native builds die at
// the companion pull. `companion publish --all` is the path that keeps the
// registry in sync with what core references; `companion verify` asserts
// it stayed that way.
var PublishCmd = &cobra.Command{
	Use:   "publish [name]",
	Short: "Build and push companion images at their pinned versions",
	Long: `Publish builds each companion and pushes it to the registry under the
tag codeflydev/<name>:<version-from-info.codefly.yaml>.

With a name argument, publishes just that companion. With --all, publishes
every image companion under <core>/companions/, in dependency order
(codefly base image first, then language runtimes, then dev tooling).

This is build + push in one step, scoped to the tags core embeds. Use it
from release/tag CI so an embedded tag is never missing from the registry.

Examples:
  codefly companion publish proto
  codefly companion publish --all
  codefly companion publish --all --core-dir ./core`,
	RunE: runPublish,
}

func init() {
	PublishCmd.Flags().Bool("all", false, "Publish every companion under <core>/companions/")
	PublishCmd.Flags().String("core-dir", "", "Path to the core directory (default: walk up from cwd looking for companions/)")
	PublishCmd.Flags().Bool("force-docker", false, "Skip the flake.nix path even when present + nix is installed")
	PublishCmd.Flags().Bool("pull", false, "Always pull a newer base image (docker build --pull) before building")
	PublishCmd.Flags().String("platform", "", "Target platform(s) for Docker builds (e.g. linux/amd64,linux/arm64). Multiple platforms publish one manifest with buildx.")
}

func runPublish(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
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
	// A single named target that builds no image can't be published; fail
	// loudly instead of buildTargets silently skipping it and reporting
	// success. Under --all the skip is correct (don't fail the whole set on
	// a non-image companion like golang).
	if !all && !targets[0].ProducesImage() {
		return fmt.Errorf("companion %q produces no image (no Dockerfile or flake.nix), nothing to publish", targets[0].Name)
	}

	opts := BuildOptions{Push: true, ForceDocker: forceDocker, Pull: pull, Platform: platform}
	return buildTargets(coreDir, targets, opts)
}
