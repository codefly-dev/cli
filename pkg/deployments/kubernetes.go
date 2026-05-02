package deployments

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// GetK8sConfig resolves the kubeconfig path for an environment.
// Lookup order:
//  1. env.Cluster.Kubeconfig (declared in workspace.codefly.yaml). Tilde
//     expansion via $HOME so users can write "~/.kube/configs/eks.yaml".
//  2. $KUBECONFIG (standard kubectl env var).
//  3. ~/.kube/config (kubectl's default).
//
// Legacy fallback: if env.Name == "aws" and nothing above resolves, we
// keep the historical hardcoded EKS path so existing dev setups don't
// break before workspace YAMLs declare environments.
func GetK8sConfig(ctx context.Context, env *resources.Environment) (string, error) {
	w := wool.Get(ctx).In("GetK8sClient")
	home, err := os.UserHomeDir()
	if err != nil {
		return "", w.Wrapf(err, "cannot get user home dir")
	}

	if env != nil && env.Cluster != nil && env.Cluster.Kubeconfig != "" {
		p := env.Cluster.Kubeconfig
		if strings.HasPrefix(p, "~/") {
			p = path.Join(home, p[2:])
		}
		return p, nil
	}

	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		return kc, nil
	}

	// Legacy fallback for envs that haven't migrated to the declared
	// schema. Once saas-starter (and other workspaces) declare their
	// environments explicitly, this branch becomes dead code we can
	// drop.
	if env != nil && env.Name == "aws" {
		return path.Join(home, "Development/codefly.dev/infrastructure/eks/kubeconfig.yaml"), nil
	}

	return path.Join(home, ".kube/config"), nil
}

func kubectlApply(ctx context.Context, configPath, kubeContext, resource string) error {
	w := wool.Get(ctx).In("kubectlApply")
	// Prepare the kubectl command with stdin from the resource string
	args := []string{"--kubeconfig", configPath}
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	args = append(args, "apply", "-f", "-")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
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
	var kubeContext string
	if env != nil && env.Cluster != nil {
		kubeContext = env.Cluster.Context
	}

	for _, r := range sources {
		err := kubectlApply(ctx, configPath, kubeContext, r)
		if err != nil {
			return w.Wrapf(err, "cannot apply resource")
		}
	}
	return nil
}
