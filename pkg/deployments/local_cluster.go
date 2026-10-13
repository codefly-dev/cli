package deployments

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
)

// A local cluster run needs the platform's prerequisites: Argo CD, which
// syncs the promoted tree and runs delivery Jobs, the authority namespace
// with its delivery account, and the SPIFFE issuer in local_spiffe.go.
// They are provisioned for a verified local k3d target only.

// The Argo CD release a local cluster run (and the k3d qualification, through
// the same installer) installs: v3.4.1, pinned by what cannot move — the
// commit its tag names, the SHA-256 of the manifest at that commit (verified
// before anything is applied), and every image the manifest runs by its
// manifest-list digest (rewritten into the manifest before it is applied; an
// image the table does not pin refuses the install). A moved tag, a changed
// file or a re-pushed image is a refusal, never a different install.
const (
	argoCDCommit = "bafd59bd37138ed731d3c3aad8e18731d72aed46" // refs/tags/v3.4.1
	// ArgoCDInstallManifest is the install manifest at that commit.
	ArgoCDInstallManifest = "https://raw.githubusercontent.com/argoproj/argo-cd/" + argoCDCommit + "/manifests/install.yaml"
	argoCDInstallSHA256   = "17f17c65a93a1d5e63822d4e6adfc62d172e677a8f870a07a3f45f182eed657b"
	// argoCDManifestLimit bounds what is read; the reviewed manifest is ~1.9 MB.
	argoCDManifestLimit = 8 << 20
)

// argoCDImages pins every image the reviewed manifest runs, by digest.
var argoCDImages = map[string]string{
	"quay.io/argoproj/argocd:v3.4.1":                   "sha256:097c922fb910e6bb0278b0e26382f88de4fa5c966564111bddd334051b0e63d7",
	"ghcr.io/dexidp/dex:v2.45.0":                       "sha256:b8469881d3cb3a73001506f0d3aaefecb9c45d2311c1e0f405d8ac538316c59d",
	"public.ecr.aws/docker/library/redis:8.2.3-alpine": "sha256:08ad0b1d280850169a790dba1393ff7a90aef951fc19632cf4d3ce4f78e679ba",
}

// imageLine is one container image reference in a manifest.
var imageLine = regexp.MustCompile(`(?m)^(\s*(?:-\s+)?image:\s*)["']?([^\s"']+)["']?\s*$`)

// Fetch reads a URL; nil means an HTTPS GET with a timeout.
type Fetch func(ctx context.Context, url string) ([]byte, error)

// pinnedManifest verifies the manifest's digest and rewrites each image to
// its pinned digest, refusing an image the table does not pin.
func pinnedManifest(raw []byte, wantSHA256 string, images map[string]string) ([]byte, error) {
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != wantSHA256 {
		return nil, fmt.Errorf("the install manifest has SHA-256 %s, not the reviewed %s; nothing was applied", got, wantSHA256)
	}
	var unpinned []string
	pinned := imageLine.ReplaceAllFunc(raw, func(line []byte) []byte {
		match := imageLine.FindSubmatch(line)
		reference := string(match[2])
		tagged, digest, hasDigest := strings.Cut(reference, "@")
		pin, known := images[tagged]
		if !known || (hasDigest && digest != pin) {
			unpinned = append(unpinned, reference)
			return line
		}
		return []byte(string(match[1]) + tagged + "@" + pin)
	})
	if len(unpinned) > 0 {
		return nil, fmt.Errorf("the install manifest runs images no digest pins (%s); nothing was applied", strings.Join(unpinned, ", "))
	}
	return pinned, nil
}

func fetchHTTPS(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %s", url, response.Status)
	}
	return io.ReadAll(io.LimitReader(response.Body, argoCDManifestLimit))
}

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
// namespace and waits for it to be ready: the manifest is fetched, verified
// against its reviewed digest and image-pinned BEFORE anything is applied,
// and applied from those bytes. A nil fetch is an HTTPS GET.
func InstallArgoCD(ctx context.Context, kubectl Kubectl, fetch Fetch) error {
	return installArgoCD(ctx, kubectl, fetch, argoCDInstallSHA256, argoCDImages)
}

func installArgoCD(ctx context.Context, kubectl Kubectl, fetch Fetch, wantSHA256 string, images map[string]string) error {
	if fetch == nil {
		fetch = fetchHTTPS
	}
	raw, err := fetch(ctx, ArgoCDInstallManifest)
	if err != nil {
		return fmt.Errorf("fetch the Argo CD install manifest %s: %w", ArgoCDInstallManifest, err)
	}
	manifest, err := pinnedManifest(raw, wantSHA256, images)
	if err != nil {
		return err
	}
	namespace := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + argoCDNamespace + "\n"
	if _, err = kubectl(ctx, namespace, "apply", "-f", "-"); err != nil {
		return fmt.Errorf("create the %s namespace: %w", argoCDNamespace, err)
	}
	if _, err = kubectl(ctx, string(manifest), "apply", "--server-side", "--force-conflicts", "-n", argoCDNamespace, "-f", "-"); err != nil {
		return fmt.Errorf("install Argo CD (%s at %s): %w", wantSHA256, argoCDCommit, err)
	}
	if _, err := kubectl(ctx, "", "wait", "--for=condition=Ready", "pod", "--all", "-n", argoCDNamespace, "--timeout=5m"); err != nil {
		return fmt.Errorf("wait for Argo CD to be ready: %w", err)
	}
	return nil
}

// EnsureArgoCD installs Argo CD unless the cluster already serves its
// ApplicationSet API, and reports whether it installed it. An Argo CD the
// developer installed is used as it is.
func EnsureArgoCD(ctx context.Context, kubectl Kubectl, fetch Fetch) (bool, error) {
	if _, err := kubectl(ctx, "", "get", "crd", "applicationsets.argoproj.io"); err == nil {
		return false, nil
	}
	return true, InstallArgoCD(ctx, kubectl, fetch)
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
