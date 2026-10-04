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

// The output-mount check is a statement about a real bind mount: that the
// directory the companion resolves an `out` to is the host directory the CLI
// resolved, and that what the companion writes there reaches the host. Neither
// half exists without a daemon — a fake runner can only assert the shape of
// the check, not that Docker propagates a deletion — so the contract that
// replaced the mtime guard (#885) is qualified here against a real container.
//
// The image is alpine rather than the proto companion: all the probe needs is
// `rm`, and pulling the companion would make a mount proof fail on a buf or
// registry problem. Keep the tag in step with the image the workflow pulls.
const mountQualificationImage = "alpine:3.22"

func TestProtoOutputMountsAreQualifiedAgainstARealBindMount(t *testing.T) {
	requireDockerDaemon(t)
	root := t.TempDir()
	outs := []string{filepath.Join(root, "sdk", "src", "gen"), filepath.Join(root, "svc", "openapi")}

	// An unchanged replay: the output tree is already the generated one, and
	// buf would touch none of it.
	committed := filepath.Join(outs[0], "api_pb.ts")
	if err := os.MkdirAll(filepath.Dir(committed), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(committed, []byte("export {};\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(committed)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the host's own outputs are accepted", func(t *testing.T) {
		companion := mountedCompanion(t, root, protoContainerRoot)
		probe := protoOutputProbe(fmt.Sprintf("mount-live-%d", time.Now().UnixMilli()))
		if err := verifyProtoOutputMounts(context.Background(), companion, outs, root, protoContainerRoot, probe); err != nil {
			t.Fatalf("refused a live mount of the host's outputs: %v", err)
		}
		for _, out := range outs {
			if _, err := os.Lstat(filepath.Join(out, probe)); !os.IsNotExist(err) {
				t.Fatalf("probe left behind in %s: %v", out, err)
			}
		}
		after, err := os.Stat(committed)
		if err != nil {
			t.Fatal(err)
		}
		if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Fatal("the mount check rewrote a file an unchanged replay must leave alone")
		}
	})

	// The failure #836 was about: the companion resolves the declared `out`
	// inside its own filesystem, writes a complete tree there, exits 0, and
	// the host keeps whatever it already had. Mounting a different directory
	// at /workspace reproduces exactly that view.
	t.Run("an output the companion cannot reach is refused", func(t *testing.T) {
		companion := mountedCompanion(t, t.TempDir(), protoContainerRoot)
		probe := protoOutputProbe(fmt.Sprintf("mount-blind-%d", time.Now().UnixMilli()))
		err := verifyProtoOutputMounts(context.Background(), companion, outs, root, protoContainerRoot, probe)
		if err == nil {
			t.Fatal("accepted outputs the companion writes inside the container")
		}
		if !strings.Contains(err.Error(), outs[0]) {
			t.Fatalf("error does not name the unreachable output: %v", err)
		}
		for _, out := range outs {
			if _, err := os.Lstat(filepath.Join(out, probe)); !os.IsNotExist(err) {
				t.Fatalf("probe left behind in %s after a failed check: %v", out, err)
			}
		}
	})
}

// mountedCompanion drives a real container with hostRoot bound at
// containerRoot, the way generateProtoCode mounts the generation tree.
func mountedCompanion(t *testing.T, hostRoot, containerRoot string) protoCommand {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("proto-mount-%d-%s", time.Now().UnixNano(), strings.ToLower(t.Name()[strings.LastIndex(t.Name(), "/")+1:]))
	name = strings.NewReplacer(" ", "-", "'", "", "/", "-").Replace(name)
	runner, err := runners.NewDockerEnvironment(ctx, resources.NewDockerImage(mountQualificationImage), hostRoot, name)
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
