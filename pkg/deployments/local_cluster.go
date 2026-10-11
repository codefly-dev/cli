package deployments

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
)

// A local cluster run (cli#918) needs two things a cell's platform provides
// and a developer's k3d cluster does not: Argo CD, which syncs the promoted
// tree and runs the delivery Jobs as its Sync hooks, and the authority
// namespace with the delivery account the authority Job runs as. Both are
// provisioned here for a verified local k3d target only.

// ArgoCDInstallManifest is the Argo CD release the local k3d qualification
// has always installed; the qualification and a local cluster run share this
// one installer.
const ArgoCDInstallManifest = "https://raw.githubusercontent.com/argoproj/argo-cd/v3.4.1/manifests/install.yaml"

const (
	argoCDNamespace = "argocd"
	// AuthorityNamespace and DeliveryAccount are the platform's: the
	// authority Job runs in that namespace as that account, which a cell's
	// platform provisions and the rendered tree deliberately cannot (the
	// authority AppProject admits ConfigMaps and Jobs there, nothing else).
	AuthorityNamespace = "platform-authority"
	DeliveryAccount    = "delivery"
	localRunManagedBy  = "codefly-local-cluster-run"
)

// Kubectl runs one kubectl invocation against a target, with stdin.
type Kubectl func(ctx context.Context, stdin string, args ...string) (string, error)

// VerifiedKubectl runs kubectl against the verified local k3d target,
// re-verifying the target before every call, from a snapshot of the exact
// kubeconfig bytes that were verified.
func VerifiedKubectl(env *environments.Environment, target *VerifiedKubernetesTarget) Kubectl {
	return func(ctx context.Context, stdin string, args ...string) (string, error) {
		snapshot, err := verifiedKubeconfigSnapshot(ctx, env, target)
		if err != nil {
			return "", err
		}
		return runKubectl(ctx, target, snapshot, stdin, args...)
	}
}

// InstallArgoCD installs the pinned Argo CD release into the argocd
// namespace and waits for it to be ready.
func InstallArgoCD(ctx context.Context, kubectl Kubectl) error {
	namespace := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + argoCDNamespace + "\n"
	if _, err := kubectl(ctx, namespace, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create the %s namespace: %w", argoCDNamespace, err)
	}
	if _, err := kubectl(ctx, "", "apply", "--server-side", "--force-conflicts", "-n", argoCDNamespace, "-f", ArgoCDInstallManifest); err != nil {
		return fmt.Errorf("install Argo CD from %s: %w", ArgoCDInstallManifest, err)
	}
	if _, err := kubectl(ctx, "", "wait", "--for=condition=Ready", "pod", "--all", "-n", argoCDNamespace, "--timeout=5m"); err != nil {
		return fmt.Errorf("wait for Argo CD to be ready: %w", err)
	}
	return nil
}

// EnsureArgoCD installs Argo CD unless the cluster already serves its
// ApplicationSet API, and reports whether it installed it. An Argo CD the
// developer installed is used as it is.
func EnsureArgoCD(ctx context.Context, kubectl Kubectl) (bool, error) {
	if _, err := kubectl(ctx, "", "get", "crd", "applicationsets.argoproj.io"); err == nil {
		return false, nil
	}
	return true, InstallArgoCD(ctx, kubectl)
}

// DeliveryAccountManifest is the authority namespace and its delivery
// ServiceAccount, as a local cluster run provisions them.
func DeliveryAccountManifest() string {
	labels := "  labels:\n    app.kubernetes.io/managed-by: " + localRunManagedBy + "\n"
	return strings.Join([]string{
		"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + AuthorityNamespace + "\n" + labels,
		"apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: " + DeliveryAccount + "\n  namespace: " + AuthorityNamespace + "\n" + labels +
			"automountServiceAccountToken: false\n",
	}, "---\n")
}

// EnsureDeliveryAccount applies the authority namespace and its delivery
// ServiceAccount. The account mounts no token by default: the delivery Job
// projects its own, for the host's audience, as it does in a cell.
func EnsureDeliveryAccount(ctx context.Context, kubectl Kubectl) error {
	if _, err := kubectl(ctx, DeliveryAccountManifest(), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("provision the %s namespace and its %s account: %w", AuthorityNamespace, DeliveryAccount, err)
	}
	return nil
}
