package deployments

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/codefly-dev/core/wool"
)

func EnsureImagesAvailable(ctx context.Context, images []string) error {
	w := wool.Get(ctx).In("EnsureImagesAvailable")
	for _, img := range images {
		if imageExistsLocally(ctx, img) {
			continue
		}

		w.Info(fmt.Sprintf("pulling %s...", img))
		pullCmd := exec.CommandContext(ctx, "docker", "pull", img)
		var stderr bytes.Buffer
		pullCmd.Stderr = &stderr
		if err := pullCmd.Run(); err != nil {
			return w.Wrapf(err, "cannot pull %s: %s", img, stderr.String())
		}
	}
	return nil
}

func K3dImportImages(ctx context.Context, cluster string, images []string) error {
	w := wool.Get(ctx).In("K3dImportImages")
	if len(images) == 0 {
		return nil
	}

	args := append([]string{"image", "import", "-c", cluster}, images...)
	cmd := exec.CommandContext(ctx, "k3d", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	w.Info(fmt.Sprintf("importing %d image(s) into k3d cluster %q", len(images), cluster))
	if err := cmd.Run(); err != nil {
		return w.Wrapf(err, "k3d image import failed: %s", stderr.String())
	}

	for _, img := range images {
		w.Info(fmt.Sprintf("imported %s", img))
	}
	return nil
}

// imageExistsLocally checks if a Docker image exists in the local daemon.
func imageExistsLocally(ctx context.Context, image string) bool {
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", image)
	return cmd.Run() == nil
}
