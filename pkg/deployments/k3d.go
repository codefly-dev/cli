package deployments

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/codefly-dev/core/wool"
)

// DetectK3dCluster returns the k3d cluster name if the current kubeconfig
// points to a k3d cluster, or "" if not k3d.
func DetectK3dCluster(ctx context.Context) string {
	w := wool.Get(ctx).In("DetectK3dCluster")

	// Check current kubectl context name — k3d contexts are named "k3d-<cluster>"
	cmd := exec.CommandContext(ctx, "kubectl", "config", "current-context")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return ""
	}
	ctxName := strings.TrimSpace(out.String())
	if !strings.HasPrefix(ctxName, "k3d-") {
		return ""
	}
	cluster := strings.TrimPrefix(ctxName, "k3d-")
	w.Debug("detected k3d cluster", wool.Field("cluster", cluster))
	return cluster
}

// K3dImportImages imports Docker images into a k3d cluster so pods
// can use them without a registry. For images not present locally,
// attempts a docker pull first (handles stock images like neo4j:5).
func K3dImportImages(ctx context.Context, cluster string, images []string) error {
	w := wool.Get(ctx).In("K3dImportImages")

	if len(images) == 0 {
		return nil
	}

	// Ensure all images exist locally — pull if needed.
	var available []string
	for _, img := range images {
		if imageExistsLocally(ctx, img) {
			available = append(available, img)
			continue
		}

		// Try pulling stock/external images.
		w.Info(fmt.Sprintf("pulling %s...", img))
		pullCmd := exec.CommandContext(ctx, "docker", "pull", img)
		if err := pullCmd.Run(); err != nil {
			w.Warn(fmt.Sprintf("cannot pull %s, skipping", img))
			continue
		}
		available = append(available, img)
	}

	if len(available) == 0 {
		return nil
	}

	args := append([]string{"image", "import", "-c", cluster}, available...)
	cmd := exec.CommandContext(ctx, "k3d", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	w.Info(fmt.Sprintf("importing %d image(s) into k3d cluster %q", len(available), cluster))
	if err := cmd.Run(); err != nil {
		return w.Wrapf(err, "k3d image import failed: %s", stderr.String())
	}

	for _, img := range available {
		w.Info(fmt.Sprintf("imported %s", img))
	}
	return nil
}

// imageExistsLocally checks if a Docker image exists in the local daemon.
func imageExistsLocally(ctx context.Context, image string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	return cmd.Run() == nil
}
