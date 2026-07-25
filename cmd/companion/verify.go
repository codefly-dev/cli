package companion

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

// VerifyCmd asserts that every companion image core/companions/ defines is
// actually present in the registry. Each image companion pins its tag
// (codeflydev/<name>:<version>) via info.codefly.yaml and agents pull it at
// runtime, but nothing otherwise guarantees a pinned tag was ever pushed —
// a version bump that references an unpublished tag passes review and only
// fails later, at the companion pull. Wiring `companion verify` into CI
// turns that late failure into a fast one. verify is the exact inverse of
// `publish`: the set it checks is the set `publish` produces.
//
// Scope: verify resolves tags from info.codefly.yaml, the source of truth
// the issue and `publish` both use. It does NOT chase image references core
// hardcodes in Go code outside that convention (e.g. a DAP language image
// pinned to a literal tag) — keeping those in sync with info.codefly.yaml,
// or exposing the embedded set as data the CLI can consume, is tracked in
// codefly-dev/core#73 and can't be derived from a checkout here.
var VerifyCmd = &cobra.Command{
	Use:   "verify [name]",
	Short: "Verify companion images defined under core/companions exist in the registry",
	Long: `Verify resolves the tag each image companion pins in its info.codefly.yaml
(codeflydev/<name>:<version>) and checks the manifest exists in the
registry via "docker manifest inspect". It verifies exactly the set
"companion publish" produces.

With no argument it verifies every image companion under
<core>/companions/; pass a name to verify just one. It exits non-zero when
any tag is missing, listing the missing tags — wire it into CI so a bump
that references an unpublished tag fails fast instead of at runtime.

Detecting a missing tag on a namespaced repo needs registry auth, so run
"docker login" first (CI does); anonymously Docker Hub reports a missing
tag as an auth error, which verify surfaces rather than guessing.

Examples:
  codefly companion verify
  codefly companion verify proto
  codefly companion verify --core-dir ./core`,
	RunE: runVerify,
}

func init() {
	VerifyCmd.Flags().Bool("all", false, "Verify every companion under <core>/companions/ (default when no name is given)")
	VerifyCmd.Flags().String("core-dir", "", "Path to the core directory (default: walk up from cwd looking for companions/)")
}

func runVerify(cmd *cobra.Command, args []string) error {
	all, _ := cmd.Flags().GetBool("all")
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")

	coreDir, err := resolveCoreDir(coreDirFlag)
	if err != nil {
		return err
	}
	// Verifying is read-only, so the friendly default is "check them all";
	// a bare `companion verify` is the CI invocation.
	if len(args) == 0 {
		all = true
	}
	targets, err := selectTargets(coreDir, all, args)
	if err != nil {
		return err
	}
	targets = imageCompanions(targets)
	if len(targets) == 0 {
		return fmt.Errorf("no image-producing companions to verify under %s/companions/", coreDir)
	}

	var missing []string
	for _, c := range targets {
		ok, err := manifestExists(c.Tag())
		if err != nil {
			return fmt.Errorf("verify %s: %w", c.Tag(), err)
		}
		if ok {
			fmt.Printf("    ok       %s\n", c.Tag())
			continue
		}
		fmt.Printf("    MISSING  %s\n", c.Tag())
		missing = append(missing, c.Tag())
	}

	if len(missing) > 0 {
		return fmt.Errorf("%d companion tag(s) not published to the registry: %s\nrun `codefly companion publish --all` to publish them",
			len(missing), strings.Join(missing, ", "))
	}
	fmt.Printf("==> all %d companion tag(s) present in the registry\n", len(targets))
	return nil
}

// imageCompanions keeps only companions that build an image. The `golang`
// Go-package companion has an info.codefly.yaml (so it carries a version)
// but no Dockerfile/flake, so there is no manifest to look up.
func imageCompanions(in []*Companion) []*Companion {
	out := make([]*Companion, 0, len(in))
	for _, c := range in {
		if c.ProducesImage() {
			out = append(out, c)
		}
	}
	return out
}

// manifestExists reports whether tag resolves to a manifest in the
// registry. It shells out to `docker manifest inspect` and treats a
// manifest-not-found failure as absent, propagating any other failure as a
// real error so a flaky or unauthenticated registry can't be mistaken for a
// missing tag.
//
// Authentication matters: for a namespaced repo, Docker Hub answers a
// missing tag with "manifest unknown" only when authenticated; anonymously
// it returns "unauthorized"/"denied", which we deliberately surface as an
// error rather than guessing. Run `docker login` first (CI does) so a
// genuinely missing tag is reported as missing, not as an auth failure.
func manifestExists(tag string) (bool, error) {
	out, err := exec.Command("docker", "manifest", "inspect", tag).CombinedOutput()
	if err == nil {
		return true, nil
	}
	if isManifestNotFound(string(out)) {
		return false, nil
	}
	return false, fmt.Errorf("docker manifest inspect %s failed (run `docker login` if this is an auth error): %s",
		tag, strings.TrimSpace(string(out)))
}

// isManifestNotFound classifies `docker manifest inspect` failure output as
// "the tag isn't in the registry" versus some other failure. It matches
// only the two ways docker and the registry v2 API phrase an absent
// manifest — a broader match (e.g. bare "not found") would swallow
// repository/auth errors and let verify pass when it shouldn't.
func isManifestNotFound(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no such manifest") ||
		strings.Contains(lower, "manifest unknown")
}
