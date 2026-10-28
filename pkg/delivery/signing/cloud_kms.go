package signing

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/core/solutionhost"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// kmsAlgorithmP256 is Cloud KMS's name for ECDSA P-256 over SHA-256: the one
// algorithm it offers that core verifies (Cloud KMS has no Ed25519).
const kmsAlgorithmP256 = "EC_SIGN_P256_SHA256"

// kmsEndpoint is the public Cloud KMS API. CODEFLY_RELEASE_KMS_ENDPOINT names
// a regional or private endpoint instead.
const kmsEndpoint = "https://cloudkms.googleapis.com"

// kmsScope is what the ambient Google credential is exchanged for.
const kmsScope = "https://www.googleapis.com/auth/cloudkms"

// kmsVersionShape is the cryptoKeyVersion resource name CODEFLY_RELEASE_KMS_KEY takes.
const kmsVersionShape = "projects/<project>/locations/<location>/keyRings/<ring>/cryptoKeys/<name>/cryptoKeyVersions/<n>"

var kmsEnumName = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)
var googleCredentialStatus = regexp.MustCompile(`^oauth2/google(?:/externalaccount)?: status code ([1-5][0-9]{2}):`)

// CloudKMS is the optional adapter the handbook decision admits behind the
// same custody interface as Vault Transit: public metadata and an
// authenticated transport, while the private key never leaves Cloud KMS.
type CloudKMS struct {
	endpoint, version, name string
	number                  int
	public                  crypto.PublicKey
	client                  *http.Client
}

// NewCloudKMS reads the key version's public half once. A nil credential is
// the ambient Google credential (Application Default Credentials); nothing is
// read from a file the CLI chooses.
func NewCloudKMS(lookup func(string) (string, bool), credential oauth2.TokenSource) (*CloudKMS, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(name string) string { value, _ := lookup(name); return value }
	version := get("CODEFLY_RELEASE_KMS_KEY")
	if version == "" {
		return nil, ErrNoIdentity
	}
	name, number, err := kmsKeyVersion(version)
	if err != nil {
		return nil, err
	}
	endpoint := get("CODEFLY_RELEASE_KMS_ENDPOINT")
	if endpoint == "" {
		endpoint = kmsEndpoint
	}
	address, cleartext, err := custodyAddress("CODEFLY_RELEASE_KMS_ENDPOINT", endpoint)
	if err != nil {
		return nil, err
	}
	if !kmsHost(address) {
		return nil, errors.New("CODEFLY_RELEASE_KMS_ENDPOINT must be a googleapis.com host or loopback: the Google credential never reaches another host")
	}
	if credential == nil {
		if credential, err = google.DefaultTokenSource(context.Background(), kmsScope); err != nil {
			return nil, fmt.Errorf("the Cloud KMS custody needs an ambient Google credential: %w", err)
		}
	}
	signer := &CloudKMS{endpoint: address, version: version, name: name, number: number, client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: &oauth2.Transport{Source: credential, Base: custodyTransport(cleartext)},
		// A redirect must never forward the bearer token to a different endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	var response struct {
		PEM       string `json:"pem"`
		Algorithm string `json:"algorithm"`
		PEMCRC32C string `json:"pemCrc32c"`
	}
	if err = signer.request(http.MethodGet, "/publicKey", nil, &response); err != nil {
		return nil, err
	}
	if response.Algorithm != kmsAlgorithmP256 {
		algorithm := "an unnamed algorithm"
		if kmsEnumName.MatchString(response.Algorithm) {
			algorithm = response.Algorithm
		}
		return nil, fmt.Errorf("the Cloud KMS key version signs with %s; release custody requires %s", algorithm, kmsAlgorithmP256)
	}
	// A missing checksum is refused as a failed one: integrity is never optional.
	if !crc32cMatches(response.PEMCRC32C, []byte(response.PEM)) {
		return nil, errors.New("the Cloud KMS public key failed its integrity check")
	}
	// The key behind the claimed algorithm is refused by name first; then
	// core owns PEM shape and key validity, as for Transit.
	block, _ := pem.Decode([]byte(response.PEM))
	if block == nil {
		return nil, errors.New("the Cloud KMS returned an unreadable public key")
	}
	signer.public, err = x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("the Cloud KMS returned an unreadable public key")
	}
	if key, ok := signer.public.(*ecdsa.PublicKey); !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("the Cloud KMS public key does not match " + kmsAlgorithmP256)
	}
	if _, err = solutionhost.NewPinnedKeySet(solutionhost.PinnedKey{KeyID: signer.KeyID(), PublicKeyPEM: response.PEM, Kind: solutionhost.KeyKindRelease, OwnershipDomains: []string{"kms-custody"}}); err != nil {
		return nil, errors.New("the Cloud KMS returned an invalid public key")
	}
	return signer, nil
}

// kmsKeyVersion derives the key name and version number the host's pin spells
// as <name>-v<n>, exactly as the Transit custody does.
func kmsKeyVersion(resource string) (string, int, error) {
	refusal := errors.New("CODEFLY_RELEASE_KMS_KEY must be a cryptoKeyVersion resource name: " + kmsVersionShape)
	parts := strings.Split(resource, "/")
	if len(parts) != 10 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "keyRings" || parts[6] != "cryptoKeys" || parts[8] != "cryptoKeyVersions" {
		return "", 0, refusal
	}
	for _, part := range []string{parts[1], parts[3], parts[5], parts[7]} {
		if !cleanPath(part, false) {
			return "", 0, refusal
		}
	}
	number, err := strconv.Atoi(parts[9])
	if err != nil || number < 1 || strconv.Itoa(number) != parts[9] {
		return "", 0, refusal
	}
	return parts[7], number, nil
}

func (s *CloudKMS) KeyID() string { return s.name + "-v" + strconv.Itoa(s.number) }
func (s *CloudKMS) Public() crypto.PublicKey {
	der, _ := x509.MarshalPKIXPublicKey(s.public)
	public, _ := x509.ParsePKIXPublicKey(der)
	return public
}

// Sign hands Cloud KMS the SHA-256 digest of the message and returns the ASN.1
// DER signature core's verifier expects for ECDSA P-256.
func (s *CloudKMS) Sign(message []byte) ([]byte, error) {
	digest := sha256.Sum256(message)
	encoded, err := json.Marshal(map[string]any{
		"digest":       map[string]string{"sha256": base64.StdEncoding.EncodeToString(digest[:])},
		"digestCrc32c": crc32c(digest[:]),
	})
	if err != nil {
		return nil, errors.New("encode Cloud KMS signing request")
	}
	var response struct {
		Name                 string `json:"name"`
		Signature            string `json:"signature"`
		SignatureCRC32C      string `json:"signatureCrc32c"`
		VerifiedDigestCRC32C bool   `json:"verifiedDigestCrc32c"`
	}
	if err = s.request(http.MethodPost, ":asymmetricSign", encoded, &response); err != nil {
		return nil, err
	}
	if response.Name != s.version {
		return nil, errors.New("the Cloud KMS signature does not come from the requested key version")
	}
	if !response.VerifiedDigestCRC32C {
		return nil, errors.New("the Cloud KMS did not confirm the digest it signed")
	}
	signature, err := base64.StdEncoding.DecodeString(response.Signature)
	if err != nil || len(signature) == 0 {
		return nil, errors.New("the Cloud KMS returned an unreadable signature")
	}
	if !crc32cMatches(response.SignatureCRC32C, signature) {
		return nil, errors.New("the Cloud KMS signature failed its integrity check")
	}
	return signature, nil
}

func (s *CloudKMS) request(method, operation string, body []byte, result any) error {
	stage := operation[1:]
	req, err := http.NewRequest(method, s.endpoint+"/v1/"+s.version+operation, bytes.NewReader(body))
	if err != nil {
		return errors.New("construct Cloud KMS request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(req)
	// Transport errors and response bodies can echo credentials. Neither is
	// copied into an error; status and stage suffice to diagnose.
	if err != nil {
		var exchange *oauth2.RetrieveError
		if errors.As(err, &exchange) && exchange.Response != nil {
			return kmsCredentialRefusal(strconv.Itoa(exchange.Response.StatusCode), string(exchange.Body))
		}
		// Google's impersonation/STS token sources return an untyped error
		// containing the response body. Retain only its known status prefix.
		for cause := err; cause != nil; cause = errors.Unwrap(cause) {
			if status := googleCredentialStatus.FindStringSubmatch(cause.Error()); status != nil {
				return kmsCredentialRefusal(status[1], strings.TrimPrefix(cause.Error(), status[0]))
			}
		}
		return fmt.Errorf("the Cloud KMS %s request failed", stage)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("the Cloud KMS %s returned HTTP %d", stage, response.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
		return fmt.Errorf("the Cloud KMS %s returned unreadable JSON", stage)
	}
	return nil
}

// Only named Google error reasons may leave a credential response; messages and
// metadata can echo credentials or request bodies.
func kmsCredentialRefusal(status, body string) error {
	message := fmt.Sprintf("the Google credential was not exchanged for a Cloud KMS token (HTTP %s)", status)
	var response struct {
		Error struct {
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if len(body) <= 65536 && json.Unmarshal([]byte(body), &response) == nil {
		for _, detail := range response.Error.Details {
			switch detail.Reason {
			case "ACCESS_TOKEN_SCOPE_INSUFFICIENT", "IAM_PERMISSION_DENIED", "SERVICE_DISABLED", "SECURITY_POLICY_VIOLATED":
				return fmt.Errorf("%s: %s", message, detail.Reason)
			}
		}
	}
	return errors.New(message)
}

// kmsHost admits only Google's API hosts, where the ambient credential's
// bearer token belongs, and loopback for a fake.
func kmsHost(address string) bool {
	parsed, err := url.Parse(address)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(host)
	return strings.HasSuffix(host, ".googleapis.com") || host == "localhost" || ip != nil && ip.IsLoopback()
}

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// crc32c is the integrity checksum Cloud KMS carries as a decimal string.
func crc32c(data []byte) string {
	return strconv.FormatUint(uint64(crc32.Checksum(data, crc32cTable)), 10)
}

func crc32cMatches(claimed string, data []byte) bool { return claimed == crc32c(data) }
