package composition

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/codefly-dev/core/composition"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

func TestOCIArtifactRequiresExactReferenceBeforeCacheUse(t *testing.T) {
	session := &SelectionSession{Root: t.TempDir()}
	data := []byte("retained bytes")
	digest := contentDigest(data)
	dir := filepath.Join(session.Root, ".codefly", "selection-artifacts")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, strings.TrimPrefix(digest, "sha256:")), data, 0o600))
	for _, uri := range []string{"oci://example.test/team/app:latest", "oci://example.test/team/app", "oci://example.test/team/app@" + contentDigest(nil), "oci://user:secret@example.test/team/app@" + digest} {
		_, err := session.acquireArtifact(t.Context(), nil, core.ReleaseArtifact{URI: uri, Digest: digest, MediaType: "application/octet-stream"})
		require.Error(t, err, uri)
	}
	_, err := session.acquireArtifact(t.Context(), nil, core.ReleaseArtifact{URI: "oci://example.test/team/app@" + digest, Digest: digest})
	require.ErrorContains(t, err, "media type")
}

func TestOCIAuthRefusesPlaintextTokenEndpoint(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	var contacted atomic.Int32
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		contacted.Add(1)
		_, _ = w.Write([]byte(`{"token":"must-not-request"}`))
	}))
	t.Cleanup(token.Close)
	registry := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+token.URL+`",service="registry",scope="repository:team/app:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(registry.Close)
	value := contentDigest([]byte("selected"))
	session := &SelectionSession{Root: t.TempDir()}
	_, err := session.acquireArtifact(t.Context(), registry.Client(), core.ReleaseArtifact{URI: "oci://" + strings.TrimPrefix(registry.URL, "https://") + "/team/app@" + value, Digest: value, MediaType: "application/octet-stream"})
	require.Error(t, err)
	require.Zero(t, contacted.Load(), "registry challenges cannot downgrade token requests to plaintext")
}

// Uses the unmodified Distribution registry, real TLS and registry auth. It
// intentionally has no insecure-registry or mock protocol fallback.
func TestOCIArtifactAcquisitionRealRegistry(t *testing.T) {
	if os.Getenv("CODEFLY_COMPOSITION_OCI_QUALIFY") != "1" {
		t.Skip("set CODEFLY_COMPOSITION_OCI_QUALIFY=1 for a disposable TLS Distribution registry")
	}
	endpoint, client, repository, _, _ := startArtifactRegistry(t)
	data := []byte("selected raw runtime bytes")
	descriptor := ocispec.Descriptor{MediaType: "application/octet-stream", Digest: digest.FromBytes(data), Size: int64(len(data))}
	require.NoError(t, repository.Push(t.Context(), descriptor, bytes.NewReader(data)))
	manifest := ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: descriptor, Layers: []ocispec.Descriptor{descriptor}}
	manifestBytes, err := json.Marshal(manifest)
	require.NoError(t, err)
	manifestDescriptor := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.FromBytes(manifestBytes), Size: int64(len(manifestBytes))}
	require.NoError(t, repository.Push(t.Context(), manifestDescriptor, bytes.NewReader(manifestBytes)))

	r := newSelectionRegistry(t)
	owner := selectionManifest("team/foo", "1.0.0")
	owner.ReleaseArtifacts = []core.ReleaseArtifact{{Name: "runtime", Purpose: core.ArtifactRuntime, URI: "oci://" + endpoint + "/team/app@" + manifestDescriptor.Digest.String(), Digest: manifestDescriptor.Digest.String(), MediaType: manifestDescriptor.MediaType}}
	owner.Services = []core.ProvidedService{{Name: "api", RuntimeArtifacts: []string{"runtime"}}}
	release := r.publish(t, owner)
	session := r.session(t, &core.Descriptor{Kind: core.DescriptorKind, Name: "product", Base: core.Base{ID: release.ID, Version: release.Version}, Services: core.Services{Include: []string{"api"}}})
	_, err = session.Initialize(t.Context(), &SelectionInputs{Root: release})
	require.NoError(t, err)
	acquired, err := session.Acquire(t.Context(), client)
	require.NoError(t, err)
	require.Len(t, acquired, 1)
	retained, err := os.ReadFile(acquired[0].Path)
	require.NoError(t, err)
	require.Equal(t, manifestBytes, retained)
	cache := filepath.Dir(acquired[0].Path)
	entries, err := os.ReadDir(cache)
	require.NoError(t, err)
	require.Len(t, entries, 1, "must not recursively collect config, layers or source")

	blob := core.ReleaseArtifact{Name: "raw", URI: "oci://" + endpoint + "/team/app@" + descriptor.Digest.String(), Digest: descriptor.Digest.String(), MediaType: descriptor.MediaType}
	path, err := session.acquireArtifact(t.Context(), client, blob)
	require.NoError(t, err)
	retained, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, retained)
	require.NoError(t, os.WriteFile(path, []byte("private patch"), 0o600))
	_, err = session.acquireArtifact(t.Context(), client, blob)
	require.NoError(t, err)
	require.True(t, artifactMatches(t.Context(), path, blob.Digest))
	missing := blob
	missing.Digest = contentDigest([]byte("missing"))
	missing.URI = "oci://" + endpoint + "/team/app@" + missing.Digest
	_, err = session.acquireArtifact(t.Context(), client, missing)
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	_, err = session.acquireArtifact(t.Context(), client, blob)
	require.ErrorContains(t, err, "authorization")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.acquireArtifact(ctx, client, blob)
	require.ErrorIs(t, err, context.Canceled)
	entries, err = os.ReadDir(cache)
	require.NoError(t, err)
	require.Len(t, entries, 1, "failed acquisitions must not retain partial bytes")
}

func startArtifactRegistry(t *testing.T) (string, *http.Client, *remote.Repository, string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	directory := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "composition-registry"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, key.Public(), key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "certificate.pem"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0o600))
	password := rand.Text()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "htpasswd"), []byte("reader:"+string(hash)+"\n"), 0o600))
	name := "cli-composition-registry-" + strings.ToLower(rand.Text()[:10])
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		output, cleanupErr := exec.CommandContext(cleanup, "docker", "rm", "-f", name).CombinedOutput()
		require.NoError(t, cleanupErr, string(output))
	})
	output, err := exec.CommandContext(ctx, "docker", "run", "-d", "--init", "--name", name, "-p", "127.0.0.1::5000", "-v", directory+":/credentials:ro", "-e", "REGISTRY_HTTP_TLS_CERTIFICATE=/credentials/certificate.pem", "-e", "REGISTRY_HTTP_TLS_KEY=/credentials/key.pem", "-e", "REGISTRY_AUTH=htpasswd", "-e", "REGISTRY_AUTH_HTPASSWD_REALM=composition", "-e", "REGISTRY_AUTH_HTPASSWD_PATH=/credentials/htpasswd", "registry:2").CombinedOutput()
	require.NoError(t, err, string(output))
	output, err = exec.CommandContext(ctx, "docker", "port", name, "5000/tcp").Output()
	require.NoError(t, err)
	endpoint := strings.TrimSpace(string(output))
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	for {
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+endpoint+"/v2/", nil)
		require.NoError(t, requestErr)
		response, readErr := client.Do(request)
		if readErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusUnauthorized {
				break
			}
		}
		require.NoError(t, ctx.Err(), "registry failed to start: %v", readErr)
		time.Sleep(100 * time.Millisecond)
	}
	config := t.TempDir()
	t.Setenv("DOCKER_CONFIG", config)
	data, err := json.Marshal(map[string]any{"auths": map[string]any{endpoint: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("reader:" + password))}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(config, "config.json"), data, 0o600))
	repository, err := remote.NewRepository(fmt.Sprintf("%s/team/app", endpoint))
	require.NoError(t, err)
	repository.Client = &auth.Client{Client: client, Cache: auth.NewCache(), Credential: auth.StaticCredential(endpoint, auth.Credential{Username: "reader", Password: password})}
	collect := func() {
		t.Helper()
		// No requests run concurrently with this offline storage walk.
		output, collectErr := exec.CommandContext(t.Context(), "docker", "exec", name, "registry", "garbage-collect", "--delete-untagged", "/etc/docker/registry/config.yml").CombinedOutput()
		require.NoError(t, collectErr, string(output))
		t.Log(string(output))
	}
	return endpoint, client, repository, filepath.Join(directory, "certificate.pem"), collect
}
