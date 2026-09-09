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
// core embeds companion tags via each companion's info.codefly.yaml and
// agents pull those exact tags at runtime; if a pinned tag was never
// pushed, Codefly-native builds die at the companion pull. `companion
// publish --all` is the path that keeps the registry in sync with what
// core references; `companion verify` asserts it stayed that way.
var PublishCmd = &cobra.Command{
	Use:   "publish [name]",
	Short: "Build and push companion images at their pinned versions",
	Long: `Publish builds each companion and pushes it to the registry under the
tag pinned in its info.codefly.yaml.

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
	PublishCmd.Flags().Bool("force", false, "Republish a companion whose tag is already in the registry, overwriting it")
}

// alreadyPublished splits targets into those whose pinned tag is already
// resolvable in the registry and those still to publish.
//
// A published tag is what agents have already resolved, so overwriting it
// swaps the image under every consumer without changing the reference they
// pin — the reason a version bump is the documented way to ship a companion
// change. Only a confirmed-present tag is skipped: a lookup that fails is
// treated as "publish it". A ghcr package that does not exist yet answers
// "denied" rather than "manifest unknown", so refusing on error would make
// the first push of a new companion impossible, and an existing package that
// answers "denied" is private — already unusable to every anonymous puller,
// so republishing is the repair, not the damage.
func alreadyPublished(targets []*Companion) (published, pending []*Companion) {
	for _, c := range targets {
		if ok, err := manifestExists(c.Name, c.Tag()); err == nil && ok {
			published = append(published, c)
			continue
		}
		pending = append(pending, c)
	}
	return published, pending
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

	force, _ := cmd.Flags().GetBool("force")
	if !force {
		published, pending := alreadyPublished(targets)
		for _, c := range published {
			fmt.Printf("==> Skipping %s (already published — bump companions/%s/info.codefly.yaml, or pass --force to overwrite)\n", c.Tag(), c.Name)
		}
		if len(pending) == 0 {
			fmt.Printf("==> every selected companion tag is already published; nothing to do\n")
			return nil
		}
		targets = pending
	}

	opts := BuildOptions{Push: true, ForceDocker: forceDocker, Pull: pull, Platform: platform}
	return buildTargets(coreDir, targets, opts)
}
