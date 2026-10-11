package signing

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/core/solutionhost"
)

// VaultTransit holds public metadata and an authenticated transport; the private
// key never leaves the platform's Transit engine.
type VaultTransit struct {
	address, token, mount, name, keyType string
	version                              int
	public                               crypto.PublicKey
	client                               *http.Client
}

// FromEnvironment is consulted only by release publishing. Missing or invalid
// custody configuration becomes an Unavailable signer with a readable refusal.
func FromEnvironment(lookup func(string) (string, bool)) Signer {
	signer, err := NewVaultTransit(lookup)
	if err != nil {
		return &Unavailable{Reason: err.Error()}
	}
	return signer
}

func NewVaultTransit(lookup func(string) (string, bool)) (*VaultTransit, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	get := func(name string) string { value, _ := lookup(name); return value }
	address, token, name := get("VAULT_ADDR"), get("VAULT_TOKEN"), get("CODEFLY_RELEASE_TRANSIT_KEY")
	if address == "" || token == "" || name == "" {
		return nil, ErrNoIdentity
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("VAULT_ADDR must be an HTTPS URL without credentials, query or fragment")
	}
	host := parsed.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && loopback) {
		return nil, errors.New("VAULT_ADDR requires https (http is allowed only for a loopback host)")
	}
	mount := get("CODEFLY_RELEASE_TRANSIT_MOUNT")
	if mount == "" {
		mount = "transit"
	}
	if !vaultPath(mount, true) || !vaultPath(name, false) {
		return nil, errors.New("Transit mount and key name must be clean path components")
	}
	signer := &VaultTransit{address: strings.TrimRight(address, "/"), token: token, mount: mount, name: name, client: &http.Client{
		Timeout:   30 * time.Second,
		Transport: vaultTransport(parsed.Scheme == "http"),
		// A redirect must never forward the token to a different endpoint.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	var response struct {
		Data struct {
			Type          string `json:"type"`
			LatestVersion int    `json:"latest_version"`
			Keys          map[string]struct {
				PublicKey string `json:"public_key"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err = signer.request(http.MethodGet, "keys", nil, &response); err != nil {
		return nil, err
	}
	signer.keyType, signer.version = response.Data.Type, response.Data.LatestVersion
	if signer.version < 1 {
		return nil, errors.New("Vault Transit returned an invalid latest key version")
	}
	encoded := response.Data.Keys[strconv.Itoa(signer.version)].PublicKey
	var publicPEM string
	switch signer.keyType {
	case "ed25519":
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("Vault Transit returned an unreadable Ed25519 public key")
		}
		signer.public = ed25519.PublicKey(raw)
		publicPEM, err = PublicKeyPEM(signer)
		if err != nil {
			return nil, errors.New("Vault Transit returned an invalid public key")
		}
	case "ecdsa-p256":
		publicPEM = encoded
	default:
		return nil, errors.New("Vault Transit key type must be ed25519 or ecdsa-p256")
	}
	// Core owns PEM shape and key validity. Parse the already validated key only
	// to supply Public(), because PinnedKeySet intentionally hides its key map.
	if _, err = solutionhost.NewPinnedKeySet(solutionhost.PinnedKey{KeyID: signer.KeyID(), PublicKeyPEM: publicPEM, Kind: solutionhost.KeyKindRelease, OwnershipDomains: []string{"transit-custody"}}); err != nil {
		return nil, errors.New("Vault Transit returned an invalid public key")
	}
	block, _ := pem.Decode([]byte(publicPEM))
	signer.public, err = x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("Vault Transit returned an unreadable public key")
	}
	// Enforce the advertised algorithm as well as core's supported-key set.
	if signer.keyType == "ecdsa-p256" {
		if _, ok := signer.public.(ed25519.PublicKey); ok {
			return nil, errors.New("Vault Transit public key does not match ecdsa-p256")
		}
	}
	return signer, nil
}

func vaultPath(value string, nested bool) bool {
	if value == "" {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
	}
	return nested || !strings.Contains(value, "/")
}
func (s *VaultTransit) KeyID() string { return s.name + "-v" + strconv.Itoa(s.version) }
func (s *VaultTransit) Public() crypto.PublicKey {
	der, _ := x509.MarshalPKIXPublicKey(s.public)
	public, _ := x509.ParsePKIXPublicKey(der)
	return public
}
func (s *VaultTransit) Sign(message []byte) ([]byte, error) {
	body := map[string]any{"input": base64.StdEncoding.EncodeToString(message), "key_version": s.version}
	if s.keyType == "ecdsa-p256" {
		body["hash_algorithm"] = "sha2-256"
		body["marshaling_algorithm"] = "asn1"
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("encode Vault Transit signing request")
	}
	var response struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	if err = s.request(http.MethodPost, "sign", encoded, &response); err != nil {
		return nil, err
	}
	prefix := "vault:v" + strconv.Itoa(s.version) + ":"
	if !strings.HasPrefix(response.Data.Signature, prefix) {
		return nil, errors.New("Vault Transit signature version does not match the requested key version")
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(response.Data.Signature, prefix))
	if err != nil || len(signature) == 0 {
		return nil, errors.New("Vault Transit returned an unreadable signature")
	}
	return signature, nil
}
func (s *VaultTransit) request(method, operation string, body []byte, result any) error {
	req, err := http.NewRequest(method, s.address+"/v1/"+s.mount+"/"+operation+"/"+s.name, bytes.NewReader(body))
	if err != nil {
		return errors.New("construct Vault Transit request")
	}
	req.Header.Set("X-Vault-Token", s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(req)
	// Transport errors and response bodies can echo credentials. Neither is
	// copied into an error; status and operation suffice to diagnose the stage.
	if err != nil {
		return fmt.Errorf("Vault Transit %s request failed", operation)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Vault Transit %s returned HTTP %d", operation, response.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
		return fmt.Errorf("Vault Transit %s returned unreadable JSON", operation)
	}
	return nil
}

// vaultTransport keeps a cleartext request on this machine. The environment's
// proxy is never consulted for one — Go's localhost exemption is
// case-sensitive, so http://LOCALHOST would otherwise send the token to a proxy
// in the clear — and every address it dials must be loopback.
func vaultTransport(cleartext bool) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !cleartext {
		return transport
	}
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		// Try every loopback address in order, as the default dialer would,
		// and never a non-loopback one.
		dialErr := errors.New("a cleartext Vault address must resolve to loopback")
		for _, candidate := range addresses {
			if !candidate.IP.IsLoopback() {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, dialErr
	}
	return transport
}
