//go:build integration

package generate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/core/resources"
	runners "github.com/codefly-dev/core/runners/dockerrun"
)

// Generation is staged: buf writes a tree under the mount and the CLI
// publishes from it, so what a run emitted is knowable independently of what
// its outputs already held (#885, and the stale-file hole after it). That
// rests on the companion and the host seeing one directory, which no fake
// runner can prove — a fake can only assert the shape of the check, not that
// Docker propagates a deletion. So the staging mount is qualified here against
// a real container.
//
// The image is alpine rather than the proto companion: all the probe needs is
// `rm`, and pulling the companion would make a mount proof fail on a buf or
// registry problem. Keep the tag in step with the image the workflow pulls.
const mountQualificationImage = "mirror.gcr.io/library/alpine:3.22"

func TestProtoStagingMountIsQualifiedAgainstARealBindMount(t *testing.T) {
	requireDockerDaemon(t)

	t.Run("a staging directory the host shares is accepted", func(t *testing.T) {
		staging := t.TempDir()
		containerStaging := protoStagingRoot
		companion := mountedCompanion(t, staging, containerStaging)
		probe := fmt.Sprintf(".probe-%d", time.Now().UnixMilli())
		if err := verifyProtoStagingMount(context.Background(), companion, staging, containerStaging, probe); err != nil {
			t.Fatalf("refused a live staging mount: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(staging, probe)); !os.IsNotExist(err) {
			t.Fatalf("probe left behind in %s: %v", staging, err)
		}
	})

	// The mount probe alone cannot establish that what a run emitted is the
	// invoking user's to publish. buf creates its output directories 0700, and
	// on Linux a bind mount preserves the identity that created a file, so an
	// unmapped companion emits a tree its caller cannot read or clean up — what
	// document-store PR429 hit against v0.1.174. Desktop Docker maps container
	// ownership onto the host user and hides this entirely, so the publication
	// below only fails on Linux; the identity assert holds everywhere.
	t.Run("private emissions can be published and removed by the host", func(t *testing.T) {
		staging := t.TempDir()
		companion := mountedCompanion(t, staging, protoStagingRoot)
		// The identity is recorded before the umask so it stays readable whoever
		// emitted it: a mapping regression then reports the identity the
		// companion ran as instead of failing to read its own evidence. It sits
		// at the staging root, outside the slot publication reads.
		script := `id -u > /staging/identity && id -g >> /staging/identity && umask 077 && mkdir -p /staging/0/nested && printf emitted > /staging/0/nested/value.txt`
		if err := companion(t.Context(), "sh", "-c", script); err != nil {
			t.Fatalf("emit a private tree into the staging mount: %v", err)
		}
		// Publication is the first half of what PR429 reported: buf generated,
		// and the host could not inspect the slot to find out what.
		root := t.TempDir()
		out := filepath.Join(root, "gen")
		if err := publishProtoOutputs(staging, root, []string{out}, false, "buf.gen.yaml"); err != nil {
			t.Fatalf("publish a private container emission: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(out, "nested", "value.txt"))
		if err != nil || string(got) != "emitted" {
			t.Fatalf("published content = %q, %v", got, err)
		}
		// The second half: generateProtoCode removes the staging tree in a
		// defer, and that failed on the same permission in the same run.
		if err := os.RemoveAll(filepath.Join(staging, "0")); err != nil {
			t.Fatalf("host cannot remove a private emission: %v", err)
		}
		identity, err := os.ReadFile(filepath.Join(staging, "identity"))
		if err != nil {
			t.Fatalf("read the emitting identity: %v", err)
		}
		if want := fmt.Sprintf("%d\n%d\n", os.Getuid(), os.Getgid()); string(identity) != want {
			t.Fatalf("companion emitted as %q, want the invoking host user %q", identity, want)
		}
	})

	// What #836 was about, in the shape it now takes: the companion resolves
	// its staging path inside its own filesystem, writes a complete tree
	// there, exits 0, and the host sees nothing. Mounting a different
	// directory at the container root reproduces exactly that view.
	t.Run("a staging directory the host cannot see is refused", func(t *testing.T) {
		staging := t.TempDir()
		containerStaging := protoStagingRoot
		companion := mountedCompanion(t, t.TempDir(), containerStaging)
		probe := fmt.Sprintf(".probe-%d", time.Now().UnixMilli())
		err := verifyProtoStagingMount(context.Background(), companion, staging, containerStaging, probe)
		if err == nil {
			t.Fatal("accepted a staging directory the companion writes inside the container")
		}
		if !strings.Contains(err.Error(), staging) {
			t.Fatalf("error does not name the staging directory: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(staging, probe)); !os.IsNotExist(err) {
			t.Fatalf("probe left behind in %s after a failed check: %v", staging, err)
		}
	})
}

// mountedCompanion drives a real container with hostRoot bound at
// containerRoot, the way generateProtoCode mounts the staging tree.
func mountedCompanion(t *testing.T, hostRoot, containerRoot string) protoCommand {
	t.Helper()
	ctx := context.Background()
	runner, err := runners.NewDockerEnvironment(ctx, resources.NewDockerImage(mountQualificationImage), hostRoot, fmt.Sprintf("proto-mount-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("create docker environment: %v", err)
	}
	configureProtoRunnerUser(runner)
	runner.WithEphemeral()
	runner.WithMount(hostRoot, containerRoot)
	runner.WithPause()
	if err := runner.Init(ctx); err != nil {
		t.Fatalf("init docker environment: %v", err)
	}
	t.Cleanup(func() {
		if err := runner.Shutdown(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("shutdown docker environment: %v", err)
		}
	})
	return protoRunnerCommand(runner)
}
