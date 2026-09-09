package companion

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
	PublishCmd.Flags().String("published-manifest", "", "Write JSON describing the companions this run actually built and pushed to this path")
}

// alreadyPublished splits targets into those whose pinned tag is already in
// the registry and those still to publish.
//
// A published tag is what agents have already resolved, so overwriting it
// swaps the image under every consumer without changing the reference they
// pin — the reason a version bump is the documented way to ship a companion
// change. Protecting that means the split has exactly two safe answers,
// "present" and "confirmed absent"; a lookup that could not determine which
// is a third answer and is returned as an error rather than folded into
// either. Treating it as absent would overwrite live tags on a network blip,
// a registry 5xx or an anonymous rate-limit, silently and fleet-wide — the
// failure this command exists to prevent.
//
// The lookup is authenticated for the same reason: see
// authenticatedManifestInspect. A private package that already exists
// resolves and is skipped, which is correct — republishing it would overwrite
// the image and still leave it private, since visibility is a registry
// setting (registryPrivacyHint names the page), not something a push changes.
func alreadyPublished(targets []*Companion) (published, pending []*Companion, err error) {
	for _, c := range targets {
		ok, out := authenticatedManifestInspect(c.Tag())
		switch {
		case ok:
			published = append(published, c)
		case isManifestNotFound(out):
			pending = append(pending, c)
		default:
			return nil, nil, fmt.Errorf(`cannot tell whether %s is already published: %s
refusing to publish over a tag that may already exist and be in use.
possible causes:
  - not logged in to the registry: docker login %s
  - the registry is unreachable or rate-limiting this runner
if the tag is genuinely unpublished, or you mean to overwrite it, pass --force`,
				c.Tag(), strings.TrimSpace(out), registryHost(c.Tag()))
		}
	}
	return published, pending, nil
}

func runPublish(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
	forceDocker, _ := cmd.Flags().GetBool("force-docker")
	pull, _ := cmd.Flags().GetBool("pull")
	platform, _ := cmd.Flags().GetString("platform")
	manifestPath, _ := cmd.Flags().GetString("published-manifest")

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
		published, pending, err := alreadyPublished(targets)
		if err != nil {
			return err
		}
		for _, c := range published {
			fmt.Printf("==> Skipping %s (already published — bump companions/%s/info.codefly.yaml, or pass --force to overwrite)\n", c.Tag(), c.Name)
		}
		// Skipping is the right default for a set: the run publishes what is
		// missing and leaves the rest alone. For one explicitly named
		// companion it is not — the operator asked for that image to be
		// published and would otherwise read exit 0 as "it was". Same
		// reasoning as the no-image case above.
		if !all && len(pending) == 0 {
			return fmt.Errorf("companion %q is already published at %s; bump companions/%s/info.codefly.yaml to ship a change, or pass --force to overwrite the tag",
				targets[0].Name, targets[0].Tag(), targets[0].Name)
		}
		if len(pending) == 0 {
			fmt.Printf("==> every selected companion tag is already published; nothing to do\n")
			return writePublishedManifest(manifestPath, nil)
		}
		targets = pending
	}

	opts := BuildOptions{Push: true, ForceDocker: forceDocker, Pull: pull, Platform: platform}
	pushed, buildErr := buildTargets(coreDir, targets, opts)
	// Written even when the run failed part way: the companions that did get
	// pushed are exactly the ones a downstream attestation may cover, and
	// dropping the record on a partial failure would either lose provenance
	// for them or invite attesting the whole declared set instead.
	if err := writePublishedManifest(manifestPath, pushed); err != nil {
		return err
	}
	return buildErr
}

// publishedEntry is one companion this run built and pushed, in the flat
// string-valued shape a GitHub Actions matrix entry carries.
type publishedEntry struct {
	Companion string `json:"companion"`
	Version   string `json:"version"`
	Image     string `json:"image"`
}

// writePublishedManifest records what this run actually published, as the
// single source of truth for anything that acts on the result.
//
// Provenance attestation is the caller that makes this necessary. Attesting a
// set derived from what core declares would sign images this run did not
// build — every tag skipped as already published — and a build-provenance
// attestation asserting a build that never happened is worse than none: it
// is pushed to the registry and to a public transparency log, where it cannot
// be withdrawn, and consumers verify it and conclude the image traces to this
// run's source.
//
// An empty list is written as an empty array, not an absent file: "this run
// published nothing" and "this run never got far enough to say" are different
// answers and the consumer has to be able to tell them apart.
func writePublishedManifest(path string, published []*Companion) error {
	if path == "" {
		return nil
	}
	entries := make([]publishedEntry, 0, len(published))
	for _, c := range published {
		entries = append(entries, publishedEntry{
			Companion: c.Name,
			Version:   c.Info.Version,
			Image:     c.Tag(),
		})
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("encode published manifest: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write published manifest %s: %w", path, err)
	}
	return nil
}
