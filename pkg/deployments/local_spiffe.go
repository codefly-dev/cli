package deployments

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/environments"
)

const (
	certManagerManifest    = "https://github.com/cert-manager/cert-manager/releases/download/v1.21.2/cert-manager.yaml"
	certManagerSHA256      = "e03b668ec8675214af6b0a671699d088f2601fa3878e0dbe1b41d3feafd1879f"
	spiffeManifestSHA256   = "ae8f275ad94414067b1f781898643d9ab68242c882abfdf04237ce3300178565"
	certManagerNamespace   = "cert-manager"
	spiffeCAName           = "csi-driver-spiffe-ca"
	disableDefaultApprover = "--controllers=*,-certificaterequests-approver"
)

//go:embed manifests/csi-driver-spiffe.yaml
var spiffeManifest []byte

//go:embed manifests/csi-images.txt
var spiffeImagePins string

var certManagerImages = map[string]string{
	"quay.io/jetstack/cert-manager-cainjector:v1.21.2": "sha256:c85268c64f2e0e76684bf5fe8906caff34b82523561c6affe0fae3546bd87562",
	"quay.io/jetstack/cert-manager-controller:v1.21.2": "sha256:70f532fd9cfde0b09d55687200942399d89838bc2d5d5b45152eb799a15912b8",
	"quay.io/jetstack/cert-manager-webhook:v1.21.2":    "sha256:a60e2dac46dbb8a7f3df95c54ce941012f54c2fe022f0ee55aaa1ab40ed957ae",
}

func pinnedSPIFFEManifest(raw []byte, digest, trustDomain string) ([]byte, error) {
	if err := environments.ValidateTrustDomain(trustDomain); err != nil {
		return nil, err
	}
	images := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(spiffeImagePins), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid SPIFFE image pin")
		}
		images[fields[0]] = fields[1]
	}
	manifest, err := pinnedManifest(raw, digest, images)
	if err != nil {
		return nil, err
	}
	const flag = "--trust-domain=cluster.local"
	if strings.Count(string(manifest), flag) != 2 {
		return nil, fmt.Errorf("SPIFFE manifest must declare exactly two trust-domain arguments")
	}
	return []byte(strings.ReplaceAll(string(manifest), flag, "--trust-domain="+trustDomain)), nil
}

func pinnedCertManagerManifest(raw []byte, digest string, images map[string]string) ([]byte, error) {
	manifest, err := pinnedManifest(raw, digest, images)
	if err != nil {
		return nil, err
	}
	const anchor = "- --cluster-resource-namespace=$(POD_NAMESPACE)"
	lines := strings.Split(string(manifest), "\n")
	found := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != anchor {
			continue
		}
		found++
		indent := strings.TrimSuffix(line, anchor)
		lines[i] += "\n" + indent + "- " + disableDefaultApprover
	}
	if found != 1 || strings.Contains(string(manifest), "--controllers=") {
		return nil, fmt.Errorf("cert-manager manifest must contain one controller argument insertion point and no controller override")
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// EnsureWorkloadIssuer provisions local workload identities. Existing components
// are used as installed. A pre-existing cert-manager must already disable its
// default approver before we may add the SPIFFE issuer. Read failures never mean
// absence. Both new manifests are verified before the first mutation.
func EnsureWorkloadIssuer(ctx context.Context, kubectl Kubectl, fetch Fetch, trustDomain string) (bool, error) {
	return ensureWorkloadIssuer(ctx, kubectl, fetch, trustDomain, certManagerSHA256, certManagerImages, spiffeManifest, spiffeManifestSHA256)
}

func ensureWorkloadIssuer(ctx context.Context, kubectl Kubectl, fetch Fetch, trustDomain, cmDigest string, cmImages map[string]string, csiRaw []byte, csiDigest string) (bool, error) {
	csi, err := pinnedSPIFFEManifest(csiRaw, csiDigest, trustDomain)
	if err != nil {
		return false, err
	}
	existing, err := kubectl(ctx, "", "get", "csidriver", "spiffe.csi.cert-manager.io", "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, fmt.Errorf("inspect SPIFFE driver: %w", err)
	}
	if strings.TrimSpace(existing) != "" {
		// Only our managed CA is rotated; an externally provisioned issuer stays
		// entirely under its owner's control.
		cert, err := readLocalSPIFFECA(ctx, kubectl)
		if err != nil {
			return false, err
		}
		if cert != "" {
			if err := ensureLocalSPIFFECA(ctx, kubectl, cert, time.Now()); err != nil {
				return false, err
			}
			return false, waitSPIFFEIssuer(ctx, kubectl)
		}
		return false, nil
	}
	existing, err = kubectl(ctx, "", "get", "crd", "certificaterequests.cert-manager.io", "--ignore-not-found", "-o", "name")
	if err != nil {
		return false, fmt.Errorf("inspect cert-manager: %w", err)
	}
	var cm []byte
	if strings.TrimSpace(existing) == "" {
		if fetch == nil {
			fetch = fetchHTTPS
		}
		raw, err := fetch(ctx, certManagerManifest)
		if err != nil {
			return false, fmt.Errorf("fetch cert-manager: %w", err)
		}
		cm, err = pinnedCertManagerManifest(raw, cmDigest, cmImages)
		if err != nil {
			return false, err
		}
	} else {
		args, err := kubectl(ctx, "", "get", "deployment", "cert-manager", "-n", certManagerNamespace, "-o", "jsonpath={.spec.template.spec.containers[*].args}")
		if err != nil {
			return false, fmt.Errorf("inspect cert-manager controller: %w", err)
		}
		var declared []string
		if err := json.Unmarshal([]byte(args), &declared); err != nil {
			return false, fmt.Errorf("read cert-manager controller arguments: %w", err)
		}
		disabled := false
		for _, arg := range declared {
			if arg == disableDefaultApprover {
				disabled = true
			}
		}
		if !disabled {
			return false, fmt.Errorf("existing cert-manager must declare %s before installing the SPIFFE issuer", disableDefaultApprover)
		}
	}
	if len(cm) > 0 {
		if _, err := kubectl(ctx, string(cm), "apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
			return false, fmt.Errorf("install cert-manager: %w", err)
		}
	}
	// The CA issuer uses the webhook; wait for its Deployment and the injector
	// that populates the admission webhook's CA before creating the issuer.
	for _, deployment := range []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"} {
		if err := issuerRollout(ctx, kubectl, "deployment/"+deployment); err != nil {
			return false, err
		}
	}
	for _, kind := range []string{"mutatingwebhookconfiguration", "validatingwebhookconfiguration"} {
		if _, err := kubectl(ctx, "", "wait", "--for=jsonpath={.webhooks[0].clientConfig.caBundle}", kind+"/cert-manager-webhook", "--timeout=2m"); err != nil {
			return false, fmt.Errorf("wait for cert-manager webhook trust: %w", err)
		}
	}
	cert, err := readLocalSPIFFECA(ctx, kubectl)
	if err != nil {
		return false, err
	}
	if err = ensureLocalSPIFFECA(ctx, kubectl, cert, time.Now()); err != nil {
		return false, err
	}
	if _, err = kubectl(ctx, string(csi), "apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
		return false, fmt.Errorf("install SPIFFE driver: %w", err)
	}
	if err := waitSPIFFEIssuer(ctx, kubectl); err != nil {
		return false, err
	}
	return true, nil
}

func waitSPIFFEIssuer(ctx context.Context, kubectl Kubectl) error {
	for _, workload := range []string{"daemonset/cert-manager-csi-driver-spiffe-driver", "deployment/cert-manager-csi-driver-spiffe-approver"} {
		if err := issuerRollout(ctx, kubectl, workload); err != nil {
			return err
		}
	}
	return nil
}

func issuerRollout(ctx context.Context, kubectl Kubectl, workload string) error {
	if _, err := kubectl(ctx, "", "rollout", "status", workload, "-n", certManagerNamespace, "--timeout=5m"); err != nil {
		return fmt.Errorf("wait for %s: %w", workload, err)
	}
	return nil
}

// Read only the public certificate: reusing a CA never exports its private key.
func readLocalSPIFFECA(ctx context.Context, kubectl Kubectl) (string, error) {
	cert, err := kubectl(ctx, "", "get", "secret", spiffeCAName, "-n", certManagerNamespace, "--ignore-not-found", "-o", `jsonpath={.type}{"\n"}{.data.tls\.crt}`)
	if err != nil {
		return "", fmt.Errorf("read local SPIFFE CA certificate: %w", err)
	}
	if cert == "" {
		return "", nil
	}
	kind, encoded, found := strings.Cut(cert, "\n")
	if !found || kind != "kubernetes.io/tls" || strings.TrimSpace(encoded) == "" {
		return "", fmt.Errorf("existing local SPIFFE CA Secret must be type kubernetes.io/tls with a CA certificate")
	}
	return strings.TrimSpace(encoded), nil
}

func ensureLocalSPIFFECA(ctx context.Context, kubectl Kubectl, encoded string, now time.Time) error {
	var certPEM []byte
	rotate := encoded == ""
	if !rotate {
		var err error
		certPEM, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("invalid local SPIFFE CA certificate encoding")
		}
		block, _ := pem.Decode(certPEM)
		if block == nil {
			return fmt.Errorf("local SPIFFE CA has no PEM certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || cert.CheckSignatureFrom(cert) != nil {
			return fmt.Errorf("local SPIFFE CA is not a self-signed CA certificate")
		}
		if now.Before(cert.NotBefore) {
			return fmt.Errorf("local SPIFFE CA is not valid yet")
		}
		rotate = !cert.NotAfter.After(now.Add(24 * time.Hour))
	}
	if rotate {
		var keyPEM []byte
		var err error
		certPEM, keyPEM, err = newLocalSPIFFECA(now)
		if err != nil {
			return err
		}
		secret := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": spiffeCAName, "namespace": certManagerNamespace}, "type": "kubernetes.io/tls", "data": map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM}}
		body, err := json.Marshal(secret)
		if err != nil {
			return err
		}
		// No last-applied annotation, no file, no logged output. kubectl errors
		// can echo a rejected Secret patch, so do not propagate them here.
		if _, err := kubectl(ctx, string(body), "apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
			return fmt.Errorf("apply local SPIFFE CA Secret failed (details withheld because they may contain the private key)")
		}
	}
	bundle := map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "spiffe-trust-bundle", "namespace": certManagerNamespace}, "data": map[string][]byte{"ca.pem": certPEM}}
	body, err := json.Marshal(bundle)
	if err != nil {
		return err
	}
	if _, err = kubectl(ctx, string(body), "apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
		return fmt.Errorf("apply SPIFFE trust bundle: %w", err)
	}
	issuer := "apiVersion: cert-manager.io/v1\nkind: ClusterIssuer\nmetadata:\n  name: " + spiffeCAName + "\nspec:\n  ca:\n    secretName: " + spiffeCAName + "\n"
	if _, err = kubectl(ctx, issuer, "apply", "--server-side", "--force-conflicts", "-f", "-"); err != nil {
		return fmt.Errorf("apply SPIFFE ClusterIssuer: %w", err)
	}
	if _, err = kubectl(ctx, "", "wait", "--for=condition=Ready", "clusterissuer/"+spiffeCAName, "--timeout=2m"); err != nil {
		return fmt.Errorf("wait for SPIFFE ClusterIssuer: %w", err)
	}
	return nil
}

func newLocalSPIFFECA(now time.Time) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	cert := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "codefly local SPIFFE CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(30 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, MaxPathLenZero: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), nil
}
