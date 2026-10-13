package nativecluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func started(t *testing.T) (*Authority, *http.Client, string) {
	t.Helper()
	authority, err := Start(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = authority.Close() })
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(authority.CAPEM()))
	host, port := authority.APIServer()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
	t.Cleanup(client.CloseIdleConnections)
	return authority, client, "https://" + net.JoinHostPort(host, port)
}

func review(t *testing.T, client *http.Client, origin, token string, audiences ...string) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"spec": map[string]any{"token": token, "audiences": audiences}})
	require.NoError(t, err)
	response, err := client.Post(origin+"/apis/authentication.k8s.io/v1/tokenreviews", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var answer struct {
		Status map[string]any `json:"status"`
	}
	require.NoError(t, json.Unmarshal(raw, &answer))
	return answer.Status
}

// The run's API server binds loopback and nothing else: it stands in for a
// cluster on a developer's machine, and nothing off that machine reaches it.
func TestTheAPIServerListensOnLoopbackOnly(t *testing.T) {
	authority, _, _ := started(t)
	host, _ := authority.APIServer()
	require.True(t, net.ParseIP(host).IsLoopback(), "the stand-in listens on %s", host)
}

// A token is good for the identity, the pod and the audience it was projected
// for, and for nothing else: the host's TokenReview is what ties a mint to the
// workload that asked.
func TestATokenReviewsAsTheIdentityAndAudienceItWasProjectedFor(t *testing.T) {
	authority, client, origin := started(t)
	token, err := authority.Token("cell-wiki", "backend", "backend-native", "uid-1", "accounts")
	require.NoError(t, err)

	status := review(t, client, origin, token, "accounts")
	require.Equal(t, true, status["authenticated"])
	user := status["user"].(map[string]any)
	require.Equal(t, "system:serviceaccount:cell-wiki:backend", user["username"])
	extra := user["extra"].(map[string]any)
	require.Equal(t, []any{"backend-native"}, extra["authentication.kubernetes.io/pod-name"])
	require.Equal(t, []any{"uid-1"}, extra["authentication.kubernetes.io/pod-uid"])

	t.Run("another audience", func(t *testing.T) {
		require.Equal(t, false, review(t, client, origin, token, "another-host")["authenticated"])
	})
	t.Run("a tampered claim", func(t *testing.T) {
		parts := strings.Split(token, ".")
		other, err := authority.Token("cell-wiki", "gateway", "gateway-native", "uid-2", "accounts")
		require.NoError(t, err)
		forged := parts[0] + "." + strings.Split(other, ".")[1] + "." + parts[2]
		require.Equal(t, false, review(t, client, origin, forged, "accounts")["authenticated"])
	})
	t.Run("a token another run issued", func(t *testing.T) {
		stranger, err := Start(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() { _ = stranger.Close() })
		foreign, err := stranger.Token("cell-wiki", "backend", "backend-native", "uid-1", "accounts")
		require.NoError(t, err)
		require.Equal(t, false, review(t, client, origin, foreign, "accounts")["authenticated"])
	})
	t.Run("not a token", func(t *testing.T) {
		require.Equal(t, false, review(t, client, origin, "nonsense", "accounts")["authenticated"])
	})
}

// The pod the API server reports is what binds a credential to a build: the
// digest of the executable the run started, under the identity its token names.
func TestAPodReportsTheBuildItWasRegisteredWith(t *testing.T) {
	authority, client, origin := started(t)
	authority.RegisterPod(&Pod{Namespace: "cell-wiki", ServiceAccount: "backend", Name: "backend-native", UID: "uid-1",
		Container: "backend", Repository: "native/cell/wiki/backend", Digest: "sha256:" + strings.Repeat("ab", 32)})

	get := func(path string) (int, map[string]any) {
		response, err := client.Get(origin + path)
		require.NoError(t, err)
		defer response.Body.Close()
		var body map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
		return response.StatusCode, body
	}
	code, pod := get("/api/v1/namespaces/cell-wiki/pods/backend-native")
	require.Equal(t, http.StatusOK, code)
	statuses := pod["status"].(map[string]any)["containerStatuses"].([]any)
	require.Equal(t, "native/cell/wiki/backend@sha256:"+strings.Repeat("ab", 32), statuses[0].(map[string]any)["imageID"])
	require.Equal(t, "backend", pod["spec"].(map[string]any)["serviceAccountName"])

	code, _ = get("/api/v1/namespaces/cell-wiki/pods/somebody-else")
	require.Equal(t, http.StatusNotFound, code)
	code, _ = get("/api/v1/namespaces/another-cell/pods/backend-native")
	require.Equal(t, http.StatusNotFound, code, "a pod is reported only in its own namespace")

	// A rebuilt workload replaces its predecessor under the same identity.
	authority.RegisterPod(&Pod{Namespace: "cell-wiki", ServiceAccount: "backend", Name: "backend-native", UID: "uid-1",
		Container: "backend", Repository: "native/cell/wiki/backend", Digest: "sha256:" + strings.Repeat("cd", 32)})
	_, pod = get("/api/v1/namespaces/cell-wiki/pods/backend-native")
	statuses = pod["status"].(map[string]any)["containerStatuses"].([]any)
	require.Contains(t, statuses[0].(map[string]any)["imageID"], strings.Repeat("cd", 32))
}

// A projection is everything a cell's render mounts for a workload: its token,
// an SVID naming exactly its SPIFFE ID under the run's CA, the trust bundle and
// its peer lists, all owner-only.
func TestAProjectionCarriesAnSVIDForExactlyTheWorkloadsIdentity(t *testing.T) {
	authority, _, _ := started(t)
	const id = "spiffe://cluster.local/ns/cell-wiki/sa/backend"
	projection, err := authority.Project(filepath.Join(t.TempDir(), "identity"), &Workload{
		Namespace: "cell-wiki", Account: "backend", SPIFFEID: id, PodName: "backend-native", PodUID: "uid-1", Audience: "accounts",
		AllowedCallers: []string{"spiffe://cluster.local/ns/cell-saas/sa/auth-gateway"},
	})
	require.NoError(t, err)
	for key, path := range projection.Files {
		info, err := os.Stat(path)
		require.NoError(t, err, key)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "%s is readable beyond its owner", key)
	}
	raw, err := os.ReadFile(projection.Files["CERT_FILE"])
	require.NoError(t, err)
	block, _ := pem.Decode(raw)
	require.NotNil(t, block)
	leaf, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.Len(t, leaf.URIs, 1)
	require.Equal(t, id, leaf.URIs[0].String())
	require.False(t, leaf.IsCA)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(authority.CAPEM()))
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	require.NoError(t, err, "the SVID chains to the run's CA")

	callers, err := os.ReadFile(projection.Files["ALLOWED_CALLERS_FILE"])
	require.NoError(t, err)
	require.Equal(t, "spiffe://cluster.local/ns/cell-saas/sa/auth-gateway\n", string(callers))

	_, _, err = authority.IssueSVID("https://not-a-spiffe-id")
	require.Error(t, err)
}

// Two runs never share a CA: the host's local profile is bound to one run's
// fingerprint, so a token or an SVID from another run is a stranger's.
func TestEachRunHasItsOwnCA(t *testing.T) {
	first, _, _ := started(t)
	second, _, _ := started(t)
	require.NotEqual(t, first.CAFingerprint(), second.CAFingerprint())
	require.Len(t, first.CAFingerprint(), 64)
}
