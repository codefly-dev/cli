package signing

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	"github.com/codefly-dev/cli/pkg/internal/azureauth"
	"github.com/google/uuid"
)

var azureReleaseHost = regexp.MustCompile(`^[a-z][a-z0-9-]{1,22}[a-z0-9]\.(vault|managedhsm)\.azure\.net$`)
var azureReleasePath = regexp.MustCompile(`^/keys/([A-Za-z0-9-]{1,127})/([a-f0-9]{32})$`)

// AzureKeyVault signs with one explicitly versioned P-256 key. Only the public
// key leaves the vault; Core still owns delivery envelopes and admission.
type AzureKeyVault struct {
	client                *azkeys.Client
	keyURI, name, version string
	public                *ecdsa.PublicKey
	publicBytes           []byte
}

// NewAzureKeyVault uses the explicitly selected tenant-bound publication credential.
func NewAzureKeyVault(lookup func(string) (string, bool)) (*AzureKeyVault, error) {
	return newAzureKeyVault(lookup, nil, nil)
}

func newAzureKeyVault(lookup func(string) (string, bool), credential azcore.TokenCredential, transport policy.Transporter) (*AzureKeyVault, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	keyURI, _ := lookup("CODEFLY_RELEASE_AZURE_KEY")
	if keyURI == "" {
		return nil, ErrNoIdentity
	}
	u, err := url.Parse(keyURI)
	if err != nil || u.Scheme != "https" || !azureReleaseHost.MatchString(u.Host) || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("CODEFLY_RELEASE_AZURE_KEY must be an HTTPS public-cloud Azure Key Vault or Managed HSM key-version URL")
	}
	parts := azureReleasePath.FindStringSubmatch(u.Path)
	if parts == nil {
		return nil, errors.New("CODEFLY_RELEASE_AZURE_KEY requires an exact key name and 32-digit version")
	}
	if credential == nil {
		tenant, _ := lookup("CODEFLY_RELEASE_AZURE_TENANT")
		id, parseErr := uuid.Parse(tenant)
		if parseErr != nil || id == uuid.Nil || id.String() != tenant {
			return nil, errors.New("CODEFLY_RELEASE_AZURE_TENANT must name the declared tenant UUID")
		}
		credential, err = azureauth.Credential(tenant, lookup)
		if err != nil {
			return nil, err
		}
	}
	if transport == nil {
		transport = &http.Client{Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	client, err := azkeys.NewClient("https://"+u.Host, credential, &azkeys.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: transport, Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		return nil, errors.New("cannot initialize Azure release custody")
	}
	signer := &AzureKeyVault{client: client, keyURI: keyURI, name: parts[1], version: parts[2]}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := client.GetKey(ctx, signer.name, signer.version, nil)
	if err != nil {
		return nil, azureSigningError(err)
	}
	signer.public, err = azureReleasePublicKey(keyURI, response.Key, response.Attributes)
	if err != nil {
		return nil, err
	}
	signer.publicBytes, err = signer.public.Bytes()
	if err != nil {
		return nil, errors.New("invalid Azure release public key encoding")
	}
	return signer, nil
}

func azureReleasePublicKey(keyURI string, key *azkeys.JSONWebKey, attrs *azkeys.KeyAttributes) (*ecdsa.PublicKey, error) {
	now := time.Now()
	if attrs == nil || attrs.Enabled == nil || !*attrs.Enabled ||
		(attrs.NotBefore != nil && now.Before(*attrs.NotBefore)) || (attrs.Expires != nil && !now.Before(*attrs.Expires)) {
		return nil, errors.New("the Azure release key is disabled or outside its validity period")
	}
	if key == nil || key.KID == nil || string(*key.KID) != keyURI || key.Kty == nil ||
		(*key.Kty != azkeys.KeyTypeEC && *key.Kty != azkeys.KeyTypeECHSM) || key.Crv == nil || *key.Crv != azkeys.CurveNameP256 ||
		len(key.X) != 32 || len(key.Y) != 32 || len(key.D) != 0 || len(key.K) != 0 {
		return nil, errors.New("the Azure release custody requires the requested public P-256 key version")
	}
	canSign := false
	for _, op := range key.KeyOps {
		if op != nil && *op == azkeys.KeyOperationSign {
			canSign = true
		}
	}
	if !canSign {
		return nil, errors.New("the Azure release key does not permit signing")
	}
	encoded := make([]byte, 65)
	encoded[0] = 4 // SEC 1 uncompressed point.
	copy(encoded[1:33], key.X)
	copy(encoded[33:], key.Y)
	public, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), encoded)
	if err != nil {
		return nil, errors.New("invalid Azure P-256 public key")
	}
	return public, nil
}

func (s *AzureKeyVault) KeyID() string { return s.keyURI }
func (s *AzureKeyVault) Public() crypto.PublicKey {
	// This immutable encoding was validated during construction.
	public, _ := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), s.publicBytes)
	return public
}

func (s *AzureKeyVault) Sign(message []byte) ([]byte, error) {
	digest := sha256.Sum256(message)
	algorithm := azkeys.SignatureAlgorithmES256
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := s.client.Sign(ctx, s.name, s.version, azkeys.SignParameters{Algorithm: &algorithm, Value: digest[:]}, nil)
	if err != nil {
		return nil, azureSigningError(err)
	}
	if response.KID == nil || string(*response.KID) != s.keyURI || len(response.Result) != 64 {
		return nil, errors.New("invalid Azure signature or a different key version")
	}
	// Azure ES256 returns fixed-width R || S; Core verifies ASN.1 DER.
	r, scalarS := new(big.Int).SetBytes(response.Result[:32]), new(big.Int).SetBytes(response.Result[32:])
	if !ecdsa.Verify(s.public, digest[:], r, scalarS) {
		return nil, ErrSignature
	}
	return asn1.Marshal(struct{ R, S *big.Int }{r, scalarS})
}

func azureSigningError(err error) error {
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("the Azure release custody request failed (HTTP %d; backend output withheld)", response.StatusCode)
	}
	return errors.New("the Azure release custody authentication or transport failed (backend output withheld)")
}

func azureCustodyConfigured(lookup func(string) (string, bool)) bool {
	key, _ := lookup("CODEFLY_RELEASE_AZURE_KEY")
	tenant, _ := lookup("CODEFLY_RELEASE_AZURE_TENANT")
	return key != "" || tenant != ""
}
