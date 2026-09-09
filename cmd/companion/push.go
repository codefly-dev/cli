package companion

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// PushCmd pushes already-built companion images to the registry.
// Equivalent to running `docker push <tag>`, but reads the version from
// info.codefly.yaml so the human can't fat-finger a tag mismatch.
//
// This is a pure publish step — it does not rebuild. Pair with
// `companion build --push` if you want one-shot build+push.
var PushCmd = &cobra.Command{
	Use:   "push <name>",
	Short: "Push a previously built companion image to its registry",
	Long: `Push uses the version in <core>/companions/<name>/info.codefly.yaml
to compute the tag, then runs "docker push <tag>".

The image must already exist locally. Build it first with
"codefly companion build <name>".`,
	Args: cobra.ExactArgs(1),
	RunE: runPush,
}

func init() {
	PushCmd.Flags().String("core-dir", "", "Path to the core directory (default: walk up from cwd)")
}

func runPush(cmd *cobra.Command, args []string) error {
	coreDirFlag, _ := cmd.Flags().GetString("core-dir")
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot read working directory: %w", err)
	}
	coreDir := coreDirFlag
	if coreDir == "" {
		coreDir = FindCompanionsRoot(cwd)
	}

	c, err := LoadCompanion(filepath.Join(coreDir, "companions", args[0]))
	if err != nil {
		return fmt.Errorf("cannot load companion %q: %w", args[0], err)
	}
	fmt.Printf("==> Pushing %s\n", c.Tag())
	if err := pushImage(c.Tag()); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}
	fmt.Printf("    pushed %s\n", c.Tag())
	return nil
}

// pushVerifyAttempts/pushVerifyDelay control the retry of the post-push
// anonymous pull check below. A tag that was just pushed can take a moment
// to become anonymously readable (registry propagation, or — per Docker
// Hub's documented behavior for a namespaced repo — a freshly written tag
// occasionally reporting an auth-shaped error for the first read or two),
// so a single failed check right after push is not yet evidence the
// package is actually private. Package vars so tests can shrink the delay.
var (
	pushVerifyAttempts = 3
	pushVerifyDelay    = time.Second
)

// pushImage runs `docker push <tag>`, then re-checks the tag anonymously
// (the same check `verify` runs) so a push that only succeeded because the
// operator's local daemon is logged in doesn't silently leave the package
// private for everyone else. Used by --push and by the standalone PushCmd.
func pushImage(tag string) error {
	host := registryHost(tag)
	fmt.Printf("    pushing %s to %s\n", tag, host)

	// Stream to the terminal while capturing a copy for isPushDenied. Buffering
	// the whole push instead would hide layer progress for the minutes a large
	// companion takes, which reads as a hang and can trip CI inactivity timeouts.
	var captured bytes.Buffer
	cmd := exec.Command("docker", "push", tag)
	cmd.Stdout = io.MultiWriter(os.Stdout, &captured)
	cmd.Stderr = io.MultiWriter(os.Stderr, &captured)
	runErr := cmd.Run()
	if runErr != nil {
		if isPushDenied(captured.String()) {
			return fmt.Errorf("docker push %s failed: not authenticated for %s\nfix: %s", tag, host, registryLoginHint(tag))
		}
		return fmt.Errorf("docker push %s failed: %w", tag, runErr)
	}

	ok, verifyOut, err := anonymousManifestInspectRetrying(tag)
	if err != nil {
		return fmt.Errorf("push %s succeeded but the anonymous pull check could not run: %w", tag, err)
	}
	if !ok {
		return fmt.Errorf(`push %s succeeded but is not publicly pullable: %s
fix: %s`,
			tag, strings.TrimSpace(verifyOut), registryPrivacyHint(tag))
	}
	return nil
}

// anonymousManifestInspectRetrying retries anonymousManifestInspect up to
// pushVerifyAttempts times, pausing pushVerifyDelay between attempts, and
// returns as soon as a check succeeds, errors outright, or the attempts run
// out — so a transient just-pushed-not-yet-readable response isn't reported
// as a permanently private package.
func anonymousManifestInspectRetrying(tag string) (ok bool, output string, err error) {
	for attempt := 1; ; attempt++ {
		ok, output, err = anonymousManifestInspect(tag)
		if err != nil || ok || attempt >= pushVerifyAttempts {
			return ok, output, err
		}
		time.Sleep(pushVerifyDelay)
	}
}

// isPushDenied reports whether `docker push` output indicates the daemon
// isn't authenticated for the target registry, as opposed to some other
// failure (network, bad tag, ...) that a login hint wouldn't fix.
func isPushDenied(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "denied") || strings.Contains(lower, "unauthorized")
}
