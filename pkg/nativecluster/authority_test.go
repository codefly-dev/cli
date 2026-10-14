package nativecluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
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

func claimsOf(t *testing.T, token string) map[string]any {
	t.Helper()
	payload, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	require.NoError(t, err)
	var claims map[string]any
	require.NoError(t, json.Unmarshal(payload, &claims))
	return claims
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
// workload that asked. Its review reports the token's credential id, the
// value the host records so a second mint with the same token is refused.
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
	credential := extra[credentialIDExtra].([]any)
	require.Len(t, credential, 1)
	require.Equal(t, "JTI="+claimsOf(t, token)["jti"].(string), credential[0], "the credential id is the token's JTI, as the API server reports it")

	t.Run("every token has a credential id of its own", func(t *testing.T) {
		other, err := authority.Token("cell-wiki", "backend", "backend-native", "uid-1", "accounts")
		require.NoError(t, err)
		otherStatus := review(t, client, origin, other, "accounts")
		require.Equal(t, true, otherStatus["authenticated"])
		otherExtra := otherStatus["user"].(map[string]any)["extra"].(map[string]any)
		require.NotEqual(t, credential, otherExtra[credentialIDExtra], "a rotated token is a new credential, never the spent one again")
	})
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

// A projected token lives 600 s, the longest mint token a host admits and
// what a cell's render asks the kubelet for, and is reissued at 80% of that,
// as the kubelet rotates it: a rotated token is a new credential.
func TestATokenLivesAndRotatesLikeTheKubelets(t *testing.T) {
	authority, _, _ := started(t)
	token, err := authority.Token("cell-wiki", "backend", "backend-native", "uid-1", "accounts")
	require.NoError(t, err)
	claims := claimsOf(t, token)
	require.Equal(t, float64(600), claims["exp"].(float64)-claims["iat"].(float64))
	require.Equal(t, 600*time.Second, TokenLifetime)
	require.Equal(t, 480*time.Second, TokenRefreshInterval)

	workload := &Workload{Namespace: "cell-wiki", Account: "backend", PodName: "backend-native", PodUID: "uid-1", Audience: "accounts"}
	projection, err := authority.Project(filepath.Join(t.TempDir(), "identity"), workload)
	require.NoError(t, err)
	first, err := os.ReadFile(projection.TokenFile)
	require.NoError(t, err)
	require.NoError(t, authority.RefreshToken(projection.TokenFile, workload))
	second, err := os.ReadFile(projection.TokenFile)
	require.NoError(t, err)
	require.NotEqual(t, claimsOf(t, string(first))["jti"], claimsOf(t, string(second))["jti"], "a refreshed token carries a new JTI")
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

// A projection is the one file a cell's projected volume mounts for a
// workload: its token, owner-only. No certificate, no trust bundle and no
// peer list: transport identity is the mesh's, which a laptop does not have
// and nothing stands in for.
func TestAProjectionIsTheWorkloadsTokenAndNothingElse(t *testing.T) {
	authority, client, origin := started(t)
	dir := filepath.Join(t.TempDir(), "identity")
	projection, err := authority.Project(dir, &Workload{Namespace: "cell-wiki", Account: "backend", PodName: "backend-native", PodUID: "uid-1", Audience: "accounts"})
	require.NoError(t, err)
	info, err := os.Stat(projection.TokenFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the token is readable beyond its owner")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "token", entries[0].Name())
	token, err := os.ReadFile(projection.TokenFile)
	require.NoError(t, err)
	status := review(t, client, origin, string(token), "accounts")
	require.Equal(t, true, status["authenticated"])
	require.Equal(t, "system:serviceaccount:cell-wiki:backend", status["user"].(map[string]any)["username"])
}

// Two runs never share a CA: the host's local profile is bound to one run's
// fingerprint, so a token from another run is a stranger's.
func TestEachRunHasItsOwnCA(t *testing.T) {
	first, _, _ := started(t)
	second, _, _ := started(t)
	require.NotEqual(t, first.CAFingerprint(), second.CAFingerprint())
	require.Len(t, first.CAFingerprint(), 64)
}
