package deployment

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func KustomizeApply(ctx context.Context, service *resources.Service, env *resources.Environment, dir string) error {
	w := wool.Get(ctx).In("Builder", wool.ThisField(service))
	w.Debug("applying kustomize", wool.DirField(dir))
	cmd := exec.Command("kustomize", "build", dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return w.Wrapf(err, "cannot run kustomize build: %s", stderr.String())
	}
	// Split the output into individual objs
	objs := strings.Split(stdout.String(), "---")
	w.Info(fmt.Sprintf("Found %d resources to apply", len(objs)))
	return KubernetesApply(ctx, service, env, objs...)
}
