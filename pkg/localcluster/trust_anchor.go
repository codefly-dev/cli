package localcluster

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
)

// The local host's trust anchor (module-saas-starter#1066, SOLUTION_REGISTRATION
// §8 "The local profile's trust anchor"): the public-good trusted root, and a
// verification policy that admits the developers the environment lists as
// person signers, bound to ONE cluster by the SHA-256 of the certificate
// authority its API server serves under. The host proves the binding at boot —
// the CA in the projected bundle, and a TLS handshake to the API server with
// that CA alone — so a profile copied anywhere else refuses the boot, and a
// recreated cluster needs the profile written again, which every run does.

// Trust-anchor file names, as the host reads them from its anchor.
const (
	TrustedRootFile        = "trusted_root.json"
	VerificationPolicyFile = "verification_policy.json"

	// keyMetadata is a manifest's metadata key.
	keyMetadata = "metadata"
)

// ClusterCAFingerprint is the lowercase hex SHA-256 of the DER of the
// cluster's certificate authority, read from the CA bundle Kubernetes projects
// into every pod (kube-root-ca.crt). A k3d cluster's bundle holds its one
// server CA; a bundle holding several cannot say which one the API server
// serves under, and is refused rather than guessed.
func ClusterCAFingerprint(bundle []byte) (string, error) {
	var certificates []*x509.Certificate
	rest := bundle
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("the cluster CA bundle holds a certificate that does not parse: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	switch len(certificates) {
	case 0:
		return "", fmt.Errorf("the cluster CA bundle (kube-root-ca.crt) holds no certificate")
	case 1:
	default:
		return "", fmt.Errorf("the cluster CA bundle (kube-root-ca.crt) holds %d certificates, and the local profile pins the one the API server serves under; a k3d cluster projects one", len(certificates))
	}
	if !certificates[0].IsCA {
		return "", fmt.Errorf("the cluster CA bundle's certificate %q is not a certificate authority", certificates[0].Subject)
	}
	digest := sha256.Sum256(certificates[0].Raw)
	return hex.EncodeToString(digest[:]), nil
}

// verificationPolicy is the host's local-profile schema.
type verificationPolicy struct {
	Local   localBinding   `json:"local"`
	Signers []personSigner `json:"signers"`
}

type localBinding struct {
	ClusterCertificateAuthoritySha256 string `json:"clusterCertificateAuthoritySha256"`
}

type personSigner struct {
	Name                   string   `json:"name"`
	Kind                   string   `json:"kind"`
	Issuer                 string   `json:"issuer"`
	SubjectAlternativeName string   `json:"subjectAlternativeName"`
	Domains                []string `json:"domains"`
}

// VerificationPolicy is the local profile's verification_policy.json: bound
// to the cluster by its CA fingerprint, one person signer per developer the
// environment's host lists — the issuer and the address, exactly — each
// speaking for the environment's ownership domain (the signer → domain map).
func VerificationPolicy(fingerprint string, host *environments.EnvironmentHost) ([]byte, error) {
	if host == nil || len(host.Developers) == 0 {
		return nil, fmt.Errorf("a local profile admits the developers the host lists, and it lists none")
	}
	if len(fingerprint) != sha256.Size*2 || strings.ToLower(fingerprint) != fingerprint {
		return nil, fmt.Errorf("cluster CA fingerprint %q is not a lowercase hex SHA-256", fingerprint)
	}
	policy := verificationPolicy{Local: localBinding{ClusterCertificateAuthoritySha256: fingerprint}}
	for _, developer := range host.Developers {
		policy.Signers = append(policy.Signers, personSigner{
			Name: developer.Subject, Kind: "person",
			Issuer: developer.Issuer, SubjectAlternativeName: developer.Subject,
			Domains: []string{host.Domain},
		})
	}
	sort.Slice(policy.Signers, func(i, j int) bool { return policy.Signers[i].Name < policy.Signers[j].Name })
	encoded, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// TrustAnchorManifest is the local trust anchor as the two objects the
// render mounts into the host's verifier (gitops.LocalTrustAnchorObjects): the
// policy in a ConfigMap and the trusted root in a Secret, each under the file
// name the host reads, in the verifier's namespace.
func TrustAnchorManifest(namespace string, policy, trustedRoot []byte) (string, error) {
	type metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "codefly-local-cluster-run"}
	configMap, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		keyMetadata: metadata{Name: gitops.LocalTrustAnchorObjects, Namespace: namespace, Labels: labels},
		"data":      map[string]string{VerificationPolicyFile: string(policy)},
	})
	if err != nil {
		return "", err
	}
	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		keyMetadata:  metadata{Name: gitops.LocalTrustAnchorObjects, Namespace: namespace, Labels: labels},
		"stringData": map[string]string{TrustedRootFile: string(trustedRoot)},
	})
	if err != nil {
		return "", err
	}
	return string(configMap) + "\n---\n" + string(secret) + "\n", nil
}
