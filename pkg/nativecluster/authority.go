// Package nativecluster makes a laptop run a cell for the solution host.
//
// A host accepts a delivery and mints a workload credential only on what a
// cluster attests: an API server that reviews a kubelet-projected token and
// reports the pod's running image. A native `codefly run` has no cluster, so
// the CLI is one: it starts each process, so it is the kubelet that projects
// the token and the API server that reviews it. The host, the gateway and
// every module run the code they run on a cell, against the same files and
// the same calls; only who vouches differs, and on a laptop that is the
// developer in every case.
//
// Transport identity is not here. In a cell the mesh carries it — a workload's
// principal is its ServiceAccount's SPIFFE ID, presented by the mesh and never
// by the workload — and a laptop has no mesh to stand in for: every hop is
// plain HTTP on loopback, and nothing issues or serves a certificate.
package nativecluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The object fields the stand-in's answers share with the API it stands in for.
const (
	fieldName = "name"
	fieldKind = "kind"
)

// TokenLifetime is a projected token's validity: the 600 s a cell's render
// asks the kubelet for (the longest mint token a host admits), so a token
// copied off a laptop is replayable for exactly as long as one copied off a
// pod — and, since the host records each token's credential id and refuses
// a second mint with it, not even that.
const TokenLifetime = 600 * time.Second

// TokenRefreshInterval is how often a projected token is reissued: at 80% of
// its lifetime, as the kubelet rotates a projected token. A mint client that
// re-reads the projection before every mint therefore always presents a
// token the host has not seen.
const TokenRefreshInterval = TokenLifetime * 4 / 5

// credentialIDExtra is the TokenReview extra a kubelet-projected token's
// review carries: the token's JTI, which the host records to make each
// projected token single-use.
const credentialIDExtra = "authentication.kubernetes.io/credential-id"

// Pod is one native process as the API server reports it: the identity its
// token names and the build it runs.
type Pod struct {
	Namespace      string
	ServiceAccount string
	Name           string
	UID            string
	Container      string
	Repository     string
	Digest         string
}

// Authority is the run's cluster: its CA, its API server and its token signer.
type Authority struct {
	caCert   *x509.Certificate
	caKey    *ecdsa.PrivateKey
	caPEM    []byte
	tokenKey ed25519.PrivateKey
	issuer   string

	server   *http.Server
	listener net.Listener

	mu   sync.Mutex
	pods map[string]*Pod
}

// Start brings the API server up on a loopback port with a fresh CA. The CA
// is per run: the host's local profile is bound to its fingerprint, and the
// run rewrites that profile, as a cluster run does when its cluster is new.
func Start(_ context.Context) (*Authority, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "codefly native run CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	_, tokenKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	authority := &Authority{
		caCert: caCert, caKey: caKey,
		caPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tokenKey: tokenKey,
		issuer:   "https://kubernetes.default.svc.cluster.local",
		pods:     map[string]*Pod{},
	}
	serverCert, serverKey, err := authority.serverCertificate()
	if err != nil {
		return nil, err
	}
	keyPair, err := tls.X509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for the native run's API server: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apis/authentication.k8s.io/v1/tokenreviews", authority.handleTokenReview)
	mux.HandleFunc("GET /api/v1/namespaces/{namespace}/pods/{pod}", authority.handleGetPod)
	authority.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{keyPair}, MinVersion: tls.VersionTLS13},
	}
	authority.listener = listener
	go func() { _ = authority.server.ServeTLS(listener, "", "") }()
	return authority, nil
}

// Close stops the API server.
func (a *Authority) Close() error {
	if a == nil || a.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return a.server.Shutdown(ctx)
}

// APIServer is the loopback address the host reaches the API server at. The
// run writes it beside the CA as the stand-in's own https origin.
func (a *Authority) APIServer() (host, port string) {
	host, port, _ = net.SplitHostPort(a.listener.Addr().String())
	return host, port
}

// CAPEM is the cluster CA, as projected into every pod's ca.crt.
func (a *Authority) CAPEM() []byte { return append([]byte(nil), a.caPEM...) }

// CAFingerprint is the lowercase hex SHA-256 of the CA's DER, the value a
// local verification profile binds itself to.
func (a *Authority) CAFingerprint() string {
	digest := sha256.Sum256(a.caCert.Raw)
	return hex.EncodeToString(digest[:])
}

// RegisterPod records a process as a pod, keyed by namespace and account: a
// later registration of the same identity replaces the earlier one, as a
// rebuilt pod replaces its predecessor.
func (a *Authority) RegisterPod(pod *Pod) {
	a.mu.Lock()
	defer a.mu.Unlock()
	copied := *pod
	a.pods[pod.Namespace+"/"+pod.ServiceAccount] = &copied
}

// Pod returns the registered pod for an identity.
func (a *Authority) Pod(namespace, account string) (Pod, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	pod, ok := a.pods[namespace+"/"+account]
	if !ok {
		return Pod{}, false
	}
	return *pod, true
}

// Token projects a token for an identity, as the kubelet does: a JWT whose
// claims name the namespace, the account and the pod, for one audience, with
// a JTI of its own so that its review can report a credential id.
func (a *Authority) Token(namespace, account, podName, podUID, audience string) (string, error) {
	now := time.Now()
	claims := map[string]any{
		"iss": a.issuer,
		"sub": "system:serviceaccount:" + namespace + ":" + account,
		"aud": []string{audience},
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(TokenLifetime).Unix(),
		"jti": randomID(),
		"kubernetes.io": map[string]any{
			"namespace":      namespace,
			"serviceaccount": map[string]any{fieldName: account},
			"pod":            map[string]any{fieldName: podName, "uid": podUID},
		},
	}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(a.tokenKey, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type tokenClaims struct {
	Audience   []string `json:"aud"`
	IssuedAt   int64    `json:"iat"`
	Expires    int64    `json:"exp"`
	ID         string   `json:"jti"`
	Kubernetes struct {
		Namespace      string `json:"namespace"`
		ServiceAccount struct {
			Name string `json:"name"`
		} `json:"serviceaccount"`
		Pod struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
	} `json:"kubernetes.io"`
}

// review authenticates a token this authority issued.
func (a *Authority) review(token string, audiences []string) (tokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return tokenClaims{}, errors.New("not a JWT")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	public, isKey := a.tokenKey.Public().(ed25519.PublicKey)
	if err != nil || !isKey || !ed25519.Verify(public, []byte(parts[0]+"."+parts[1]), signature) {
		return tokenClaims{}, errors.New("signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}, err
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return tokenClaims{}, err
	}
	now := time.Now().Unix()
	if claims.Expires <= now {
		return tokenClaims{}, errors.New("token expired")
	}
	if claims.ID == "" {
		return tokenClaims{}, errors.New("token carries no JTI, so its review would report no credential id")
	}
	if len(audiences) > 0 {
		matched := false
		for _, wanted := range audiences {
			for _, aud := range claims.Audience {
				if aud == wanted {
					matched = true
				}
			}
		}
		if !matched {
			return tokenClaims{}, errors.New("token audience does not match")
		}
	}
	return claims, nil
}

func (a *Authority) handleTokenReview(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Spec struct {
			Token     string   `json:"token"`
			Audiences []string `json:"audiences"`
		} `json:"spec"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&request); err != nil {
		http.Error(w, "unreadable TokenReview", http.StatusBadRequest)
		return
	}
	response := map[string]any{"apiVersion": "authentication.k8s.io/v1", fieldKind: "TokenReview"}
	claims, err := a.review(request.Spec.Token, request.Spec.Audiences)
	if err != nil {
		response["status"] = map[string]any{"authenticated": false, "error": err.Error()}
	} else {
		response["status"] = map[string]any{
			"authenticated": true,
			"audiences":     claims.Audience,
			"user": map[string]any{
				"username": "system:serviceaccount:" + claims.Kubernetes.Namespace + ":" + claims.Kubernetes.ServiceAccount.Name,
				"extra": map[string][]string{
					"authentication.kubernetes.io/pod-name": {claims.Kubernetes.Pod.Name},
					"authentication.kubernetes.io/pod-uid":  {claims.Kubernetes.Pod.UID},
					// As the API server reports it from a token's JTI: the
					// value the host records to refuse a replayed token.
					credentialIDExtra: {"JTI=" + claims.ID},
				},
			},
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (a *Authority) handleGetPod(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("pod")
	a.mu.Lock()
	var found *Pod
	for _, pod := range a.pods {
		if pod.Namespace == namespace && pod.Name == name {
			found = pod
			break
		}
	}
	a.mu.Unlock()
	if found == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{fieldKind: "Status", "status": "Failure", "code": 404, "reason": "NotFound"})
		return
	}
	image := found.Repository + "@" + found.Digest
	container := map[string]any{fieldName: found.Container, "image": image}
	status := map[string]any{fieldName: found.Container, "imageID": image, "state": map[string]any{"running": map[string]any{}}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": "v1", fieldKind: "Pod",
		"metadata": map[string]any{fieldName: found.Name, "namespace": found.Namespace, "uid": found.UID},
		"spec":     map[string]any{"containers": []any{container}, "serviceAccountName": found.ServiceAccount},
		"status":   map[string]any{"phase": "Running", "containerStatuses": []any{status}},
	})
}

// serverCertificate signs the API server's own leaf under the run's CA: the
// one certificate the run issues, for the one TLS listener it has, which the
// host dials by the loopback names below.
func (a *Authority) serverCertificate() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "codefly native run API server"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"kubernetes.default.svc", "localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.caCert, &key.PublicKey, a.caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func serial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

// randomID is a token's JTI: 128 random bits, hex.
func randomID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// Projection is what one workload is handed: the file a cell's projected
// volume would carry.
type Projection struct {
	// TokenFile is the path of the workload's projected token.
	TokenFile string
}

// Workload is one native process to project a token for.
type Workload struct {
	Namespace string
	Account   string
	PodName   string
	PodUID    string
	// Audience is the audience its projected token is minted for: the host's.
	Audience string
}

// Project writes a workload's projected token under dir, owner-only. The
// token is rewritten by RefreshToken.
func (a *Authority) Project(dir string, workload *Workload) (Projection, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Projection{}, err
	}
	projection := Projection{TokenFile: filepath.Join(dir, "token")}
	return projection, a.RefreshToken(projection.TokenFile, workload)
}

// RefreshToken rewrites a workload's token, as the kubelet rotates a projected
// token before it lapses.
func (a *Authority) RefreshToken(path string, workload *Workload) error {
	token, err := a.Token(workload.Namespace, workload.Account, workload.PodName, workload.PodUID, workload.Audience)
	if err != nil {
		return err
	}
	return writePrivate(path, []byte(token))
}

// writePrivate writes a file atomically with owner-only permissions, so a
// reader never sees a half-written token.
func writePrivate(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
