package deployments

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func KustomizeDir(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) string {
	return path.Join(workspace.Dir(), "deployments", "modules", module.Name, "services", service.Name)
}

func KustomizeDirForEnv(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment) string {
	return path.Join(KustomizeDir(ctx, workspace, module, service), "overlays", env.Name)
}

func KustomizeApply(ctx context.Context, service *resources.Service, env *resources.Environment, dir string) error {
	w := wool.Get(ctx).In("Builder", wool.ThisField(resources.WithUnique(service)))
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
