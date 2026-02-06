package deployments

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/wool"
)

func GetK8sConfig(ctx context.Context, env *resources.Environment) (string, error) {
	w := wool.Get(ctx).In("GetK8sClient")
	home, err := os.UserHomeDir()
	if err != nil {
		return "", w.Wrapf(err, "cannot get user home dir")
	}
	if env.Name == "aws" {
		return path.Join(home, "Development/codefly.dev/infrastructure/eks/kubeconfig.yaml"), nil
	} else {
		return path.Join(home, ".kube/config"), nil
	}
}

func kubectlApply(ctx context.Context, configPath, resource string) error {
	w := wool.Get(ctx).In("kubectlApply")
	// Prepare the kubectl command with stdin from the resource string
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", configPath, "apply", "-f", "-")
	cmd.Stdin = bytes.NewBufferString(resource)

	// Capture the output and error if any
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	// Execute the command
	err := cmd.Run()
	if err != nil {
		return w.Wrapf(err, "cannot run kubectl apply: %s", stderr.String())
	}

	if strings.Contains(out.String(), "unchanged") {
		return nil
	}
	w.Info(strings.TrimSpace(out.String()))
	return nil
}

func KubernetesApply(ctx context.Context, service *resources.Service, env *resources.Environment, sources ...string) error {
	w := wool.Get(ctx).In("KubernetesApply", wool.ThisField(resources.WithUnique(service)))
	// Create the Kubernetes client
	configPath, err := GetK8sConfig(ctx, env)
	if err != nil {
		return w.Wrapf(err, "cannot get k8s client")
	}

	for _, r := range sources {
		err := kubectlApply(ctx, configPath, r)
		if err != nil {
			return w.Wrapf(err, "cannot apply resource")
		}
	}
	return nil
}
