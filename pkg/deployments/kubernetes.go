package deployments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"gopkg.in/yaml.v3"
)

type VerifiedKubernetesTarget struct {
	Kind       string
	Kubeconfig string
	Context    string
	Cluster    string
	APIServer  string
	K3dCluster string
	// ClusterIdentity is the digest of the complete kubeconfig cluster entry,
	// including certificate and routing settings.
	ClusterIdentity string
}

type kubeconfigView struct {
	CurrentContext string `json:"current-context" yaml:"current-context"`
	Clusters       []struct {
		Name    string         `json:"name" yaml:"name"`
		Cluster map[string]any `json:"cluster" yaml:"cluster"`
	} `json:"clusters" yaml:"clusters"`
	Contexts []struct {
		Name    string `json:"name" yaml:"name"`
		Context struct {
			Cluster string `json:"cluster" yaml:"cluster"`
		} `json:"context" yaml:"context"`
	} `json:"contexts" yaml:"contexts"`
}

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

func VerifyLocalK3dTarget(ctx context.Context, env *resources.Environment) (VerifiedKubernetesTarget, error) {
	target, _, err := verifyLocalK3dTarget(ctx, env)
	return target, err
}

func verifyLocalK3dTarget(ctx context.Context, env *resources.Environment) (VerifiedKubernetesTarget, []byte, error) {
	if env == nil || env.Cluster == nil || env.Cluster.Kind != "k3d" {
		envName := ""
		kind := ""
		if env != nil {
			envName = env.Name
			if env.Cluster != nil {
				kind = env.Cluster.Kind
			}
		}
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf(
			"direct Kubernetes apply is allowed only for an exact local k3d target; environment %q declares cluster kind %q; use --render-only and publish the rendered manifests through GitOps",
			envName,
			kind,
		)
	}

	configPath, err := GetK8sConfig(ctx, env)
	if err != nil {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("resolve kubeconfig: %w", err)
	}
	if len(filepath.SplitList(configPath)) != 1 {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("direct Kubernetes apply requires exactly one kubeconfig, got %q", configPath)
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("resolve kubeconfig %q: %w", configPath, err)
	}
	kubeContext := env.Cluster.Context
	if kubeContext == "" {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf(
			"environment %q must declare cluster.context to permit direct apply; use --render-only and publish the rendered manifests through GitOps",
			env.Name,
		)
	}

	selected, snapshot, err := readSelectedKubeconfig(ctx, configPath)
	if err != nil {
		return VerifiedKubernetesTarget{}, nil, err
	}
	currentContext := selected.CurrentContext
	if currentContext == "" {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("kubeconfig %q has no current context; refusing direct apply", configPath)
	}
	if kubeContext != currentContext {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf(
			"declared context %q does not match kubeconfig %q current context %q; refusing direct apply",
			kubeContext,
			configPath,
			currentContext,
		)
	}
	if !strings.HasPrefix(kubeContext, "k3d-") || kubeContext == "k3d-" {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("context %q is not a k3d context; refusing direct apply", kubeContext)
	}

	clusterName, apiServer, clusterIdentity, err := selected.target(kubeContext)
	if err != nil {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("resolve context %q from kubeconfig %q: %w", kubeContext, configPath, err)
	}
	k3dCluster := strings.TrimPrefix(kubeContext, "k3d-")
	owned, err := readK3dKubeconfig(ctx, k3dCluster)
	if err != nil {
		return VerifiedKubernetesTarget{}, nil, err
	}
	ownedCluster, ownedServer, ownedIdentity, err := owned.target(kubeContext)
	if err != nil {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf("resolve k3d-owned cluster %q: %w", k3dCluster, err)
	}
	if owned.CurrentContext != kubeContext ||
		ownedCluster != clusterName ||
		ownedServer != apiServer ||
		ownedIdentity != clusterIdentity {
		return VerifiedKubernetesTarget{}, nil, fmt.Errorf(
			"context %q resolves to cluster %q at %q, which does not match k3d-owned cluster %q at %q; refusing direct apply",
			kubeContext,
			clusterName,
			apiServer,
			ownedCluster,
			ownedServer,
		)
	}

	return VerifiedKubernetesTarget{
		Kind:            env.Cluster.Kind,
		Kubeconfig:      filepath.Clean(configPath),
		Context:         kubeContext,
		Cluster:         clusterName,
		APIServer:       apiServer,
		K3dCluster:      k3dCluster,
		ClusterIdentity: clusterIdentity,
	}, snapshot, nil
}

func VerifyLocalK3dTargetUnchanged(ctx context.Context, env *resources.Environment, planned *VerifiedKubernetesTarget) error {
	_, err := verifiedKubeconfigSnapshot(ctx, env, planned)
	return err
}

func verifiedKubeconfigSnapshot(ctx context.Context, env *resources.Environment, planned *VerifiedKubernetesTarget) ([]byte, error) {
	current, snapshot, err := verifyLocalK3dTarget(ctx, env)
	if err != nil {
		return nil, err
	}
	if current != *planned {
		return nil, fmt.Errorf(
			"Kubernetes target changed after validation (planned context %q, cluster %q, server %q; current context %q, cluster %q, server %q); refusing direct apply",
			planned.Context,
			planned.Cluster,
			planned.APIServer,
			current.Context,
			current.Cluster,
			current.APIServer,
		)
	}
	return snapshot, nil
}

func readSelectedKubeconfig(ctx context.Context, configPath string) (kubeconfigView, []byte, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", configPath, "config", "view", "--raw", "--flatten", "--minify", "-o", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return kubeconfigView{}, nil, fmt.Errorf("read kubeconfig %q: %w: %s", configPath, err, strings.TrimSpace(stderr.String()))
	}
	var config kubeconfigView
	if err := json.Unmarshal(stdout.Bytes(), &config); err != nil {
		return kubeconfigView{}, nil, fmt.Errorf("decode kubeconfig %q: %w", configPath, err)
	}
	return config, bytes.Clone(stdout.Bytes()), nil
}

func readK3dKubeconfig(ctx context.Context, cluster string) (kubeconfigView, error) {
	cmd := exec.CommandContext(ctx, "k3d", "kubeconfig", "get", cluster)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return kubeconfigView{}, fmt.Errorf("verify k3d-owned cluster %q: %w: %s", cluster, err, strings.TrimSpace(stderr.String()))
	}
	var config kubeconfigView
	if err := yaml.Unmarshal(stdout.Bytes(), &config); err != nil {
		return kubeconfigView{}, fmt.Errorf("decode k3d-owned cluster %q kubeconfig: %w", cluster, err)
	}
	return config, nil
}

func (config kubeconfigView) target(contextName string) (string, string, string, error) {
	clusterName := ""
	contextMatches := 0
	for _, candidate := range config.Contexts {
		if candidate.Name != contextName {
			continue
		}
		contextMatches++
		if contextMatches > 1 {
			return "", "", "", fmt.Errorf("context %q is declared more than once", contextName)
		}
		clusterName = candidate.Context.Cluster
	}
	if clusterName == "" {
		return "", "", "", fmt.Errorf("context %q does not select a cluster", contextName)
	}

	apiServer := ""
	clusterIdentity := ""
	clusterMatches := 0
	for _, candidate := range config.Clusters {
		if candidate.Name != clusterName {
			continue
		}
		clusterMatches++
		if clusterMatches > 1 {
			return "", "", "", fmt.Errorf("cluster %q is declared more than once", clusterName)
		}
		var ok bool
		apiServer, ok = candidate.Cluster["server"].(string)
		if !ok || apiServer == "" {
			return "", "", "", fmt.Errorf("cluster %q has no API server", clusterName)
		}
		encoded, err := json.Marshal(candidate.Cluster)
		if err != nil {
			return "", "", "", fmt.Errorf("encode cluster %q identity: %w", clusterName, err)
		}
		clusterIdentity = fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	}
	if apiServer == "" {
		return "", "", "", fmt.Errorf("cluster %q has no API server", clusterName)
	}
	return clusterName, apiServer, clusterIdentity, nil
}

func kubectlApply(ctx context.Context, target *VerifiedKubernetesTarget, kubeconfig []byte, resource string) error {
	w := wool.Get(ctx).In("kubectlApply")
	snapshot, err := os.CreateTemp("", "codefly-verified-kubeconfig-*")
	if err != nil {
		return w.Wrapf(err, "cannot create verified kubeconfig snapshot")
	}
	snapshotPath := snapshot.Name()
	defer os.Remove(snapshotPath)
	if _, err := snapshot.Write(kubeconfig); err != nil {
		_ = snapshot.Close()
		return w.Wrapf(err, "cannot write verified kubeconfig snapshot")
	}
	if err := snapshot.Close(); err != nil {
		return w.Wrapf(err, "cannot close verified kubeconfig snapshot")
	}

	args := []string{"--kubeconfig", snapshotPath, "--context", target.Context}
	args = append(args, "apply", "-f", "-")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = bytes.NewBufferString(resource)

	// Capture the output and error if any
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	// Execute the command
	err = cmd.Run()
	if err != nil {
		return w.Wrapf(err, "cannot run kubectl apply: %s", stderr.String())
	}

	if strings.Contains(out.String(), "unchanged") {
		return nil
	}
	w.Info(strings.TrimSpace(out.String()))
	return nil
}

func KubernetesApply(ctx context.Context, env *resources.Environment, target *VerifiedKubernetesTarget, sources ...string) error {
	w := wool.Get(ctx).In("KubernetesApply")
	for _, r := range sources {
		snapshot, err := verifiedKubeconfigSnapshot(ctx, env, target)
		if err != nil {
			return w.Wrapf(err, "cannot verify Kubernetes target before apply")
		}
		if err := kubectlApply(ctx, target, snapshot, r); err != nil {
			return w.Wrapf(err, "cannot apply resource")
		}
	}
	return nil
}
