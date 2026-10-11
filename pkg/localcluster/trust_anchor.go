package localcluster

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/cli/pkg/delivery/signing"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/core/solutionhost"
)

// The local host's trust anchor (module-saas-starter#1066, SOLUTION_REGISTRATION
// §8 "The local profile's trust anchor"): a verification policy that admits
// the locally held developer key the environment lists as a
// person signers, bound to ONE cluster by the SHA-256 of the certificate
// authority its API server serves under. The host proves the binding at boot —
// the CA in the projected bundle, and a TLS handshake to the API server with
// that CA alone — so a profile copied anywhere else refuses the boot, and a
// recreated cluster needs the profile written again, which every run does.

// Trust-anchor file names, as the host reads them from its anchor.
const (
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
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	KeyID     string   `json:"keyId"`
	PublicKey string   `json:"publicKey"`
	Domains   []string `json:"domains"`
}

var localDeveloperKey = signing.LoadDeveloperKey

// VerificationPolicy pins only the locally held key if the host declares it.
// Other developers' public keys cannot be inferred from their key IDs.
func VerificationPolicy(fingerprint string, host *environments.EnvironmentHost) ([]byte, error) {
	if host == nil || len(host.Developers) == 0 {
		return nil, fmt.Errorf("a local profile admits the developers the host lists, and it lists none")
	}
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(fingerprint) != fingerprint {
		return nil, fmt.Errorf("cluster CA fingerprint %q is not a lowercase hex SHA-256", fingerprint)
	}
	policy := verificationPolicy{Local: localBinding{ClusterCertificateAuthoritySha256: fingerprint}, Signers: []personSigner{}}
	signer, err := localDeveloperKey()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		for _, developer := range host.Developers {
			if developer.KeyID != signer.KeyID() {
				continue
			}
			public, err := signing.PublicKeyPEM(signer)
			if err != nil {
				return nil, err
			}
			if _, err = solutionhost.NewPinnedKeySet(solutionhost.PinnedKey{KeyID: signer.KeyID(), PublicKeyPEM: public, Kind: solutionhost.KeyKindPerson, OwnershipDomains: []string{host.Domain}}); err != nil {
				return nil, err
			}
			policy.Signers = append(policy.Signers, personSigner{Name: signer.KeyID(), Kind: "person", KeyID: signer.KeyID(), PublicKey: public, Domains: []string{host.Domain}})
			break
		}
	}
	encoded, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

// TrustAnchorManifest mounts the public verification policy in one ConfigMap.
func TrustAnchorManifest(namespace string, policy []byte) (string, error) {
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
	return string(configMap) + "\n", nil
}
