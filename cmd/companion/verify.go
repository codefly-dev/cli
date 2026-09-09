package companion

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// VerifyCmd asserts that every companion image core/companions/ defines is
// actually present in the registry. Each image companion pins its version
// via info.codefly.yaml and agents pull it at runtime, but nothing
// otherwise guarantees a pinned tag was ever pushed —
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
and checks the manifest exists in the registry via "docker manifest
inspect". It verifies exactly the set "companion publish" produces.

With no argument it verifies every image companion under
<core>/companions/; pass a name to verify just one. It exits non-zero when
any tag is missing, listing the missing tags — wire it into CI so a bump
that references an unpublished tag fails fast instead of at runtime.

The check runs with no stored docker credentials, so it sees exactly what
an anonymous puller (agents, CI, other developers) sees — a package an
operator can see only because they're logged in locally would otherwise
pass verify and still 401 for everyone else.

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

	// Both missing tags and hard errors are collected so one run reports the
	// whole picture. A companion whose ghcr package does not exist at all —
	// the state a first migration to a new registry leaves every companion in
	// — comes back as "denied" rather than "manifest unknown", which is a hard
	// error, so returning on the first one would report a single companion per
	// run and hide the rest.
	var missing []string
	var failures []string
	for _, c := range targets {
		ok, err := manifestExists(c.Name, c.Tag())
		if err != nil {
			fmt.Printf("    ERROR    %s\n", c.Tag())
			failures = append(failures, err.Error())
			continue
		}
		if ok {
			fmt.Printf("    ok       %s\n", c.Tag())
			continue
		}
		fmt.Printf("    MISSING  %s\n", c.Tag())
		missing = append(missing, c.Tag())
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d companion tag(s) could not be verified:\n  %s",
			len(failures), strings.Join(failures, "\n  "))
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
// registry, checked anonymously (see anonymousManifestInspect) so the
// result matches what any puller without local credentials sees. A
// manifest-not-found response is treated as "absent"; any other failure
// (private package, unreachable registry, ...) is a real error, since a
// flaky or access-denied registry must not be mistaken for a missing tag.
func manifestExists(name, tag string) (bool, error) {
	ok, out, err := anonymousManifestInspect(tag)
	if err != nil {
		return false, fmt.Errorf("verify %s: %w", tag, err)
	}
	if ok {
		return true, nil
	}
	if isManifestNotFound(out) {
		return false, nil
	}
	return false, fmt.Errorf(`verify %s: docker manifest inspect failed anonymously: %s
possible causes:
  - the tag was never pushed: run "codefly companion publish %s"
  - the package is private: %s`,
		tag, strings.TrimSpace(out), name, registryPrivacyHint(tag))
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

// authenticatedManifestInspect runs `docker manifest inspect <tag>` with the
// ambient docker credentials, and is the lookup for deciding whether a tag is
// already taken.
//
// The anonymous probe below cannot answer that question: a package that has
// never been pushed and a package that exists but is private both come back
// "denied", so an anonymous caller has to guess, and the safe-looking guess
// ("assume absent, publish it") is the one that overwrites a live tag. With
// credentials the two separate — an existing private package resolves, an
// absent one does not — which is what makes the skip decision sound.
func authenticatedManifestInspect(tag string) (ok bool, output string) {
	cmd := exec.Command("docker", "manifest", "inspect", tag)
	out, runErr := cmd.CombinedOutput()
	return runErr == nil, string(out)
}

// anonymousManifestInspect runs `docker manifest inspect <tag>` with
// DOCKER_CONFIG pointed at a throwaway directory holding an empty config,
// so the lookup carries no stored credentials regardless of what the
// operator is logged into locally. ok reports whether the command
// succeeded; output is its combined stdout+stderr for callers to classify
// (isManifestNotFound) or report. err is only set for failures unrelated
// to the docker command's own exit status (e.g. can't create the temp
// config dir).
func anonymousManifestInspect(tag string) (ok bool, output string, err error) {
	configDir, err := os.MkdirTemp("", "codefly-companion-anon-docker-*")
	if err != nil {
		return false, "", fmt.Errorf("create anonymous docker config dir: %w", err)
	}
	defer os.RemoveAll(configDir)
	// Credential-free, but experimental stays enabled: older Docker CLIs gate
	// `docker manifest` behind that flag, and a bare "{}" would strip it from
	// under an operator who has it set, turning a working check into an error
	// that is neither "manifest unknown" nor a real privacy problem.
	const anonymousConfig = `{"experimental":"enabled"}`
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(anonymousConfig), 0o600); err != nil {
		return false, "", fmt.Errorf("write anonymous docker config: %w", err)
	}

	cmd := exec.Command("docker", "manifest", "inspect", tag)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+configDir)
	out, runErr := cmd.CombinedOutput()
	return runErr == nil, string(out), nil
}
