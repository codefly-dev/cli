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
const mountQualificationImage = "alpine:3.22"

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
