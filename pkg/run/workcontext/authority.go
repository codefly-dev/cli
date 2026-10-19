// Package workcontext serves the verification key set a native run stands in
// for the host with.
//
// A service that verifies an inbound Work Context loads its key set from an
// endpoint, and the one transport rule for that endpoint already admits
// "plaintext http to a loopback host, on a local run only". Nothing served it,
// so a module whose identity comes from the host could not boot on its own.
// This serves it, over that admitted hop, for the life of the run.
//
// The key set is the run's own and so is the issuer stamped beside it. A
// verifier pins the issuer it accepts, so a capability signed here is
// structurally unacceptable to a deployed service, which pins the host's.
package workcontext

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// KeySetPath is where the run serves its key set. It is the whole route: the
// consumer reads the URL verbatim and appends nothing.
const KeySetPath = "/.well-known/work-context-jwks.json"

// keyID names the run's verification key inside the set.
const keyID = "local-run"

// Authority is the key set a native run serves, and the issuer it stamps.
type Authority struct {
	issuer   string
	public   ed25519.PublicKey
	private  ed25519.PrivateKey
	listener net.Listener
	server   *http.Server
}

// Start mints the run's signing key and serves its public half on loopback.
//
// runID distinguishes one run's issuer from another's, so two runs on one
// machine never accept each other's capabilities.
func Start(runID string) (*Authority, error) {
	if runID == "" {
		return nil, fmt.Errorf("a run's work-context authority is issued under the run's own identity, and this run has none")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("cannot mint this run's work-context key: %w", err)
	}
	// Loopback only: the admitted hop is a loopback one, and a key set reachable
	// from off the machine is not what was admitted.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("cannot serve this run's work-context key set: %w", err)
	}
	authority := &Authority{
		issuer:   fmt.Sprintf("codefly://local-run/%s", runID),
		public:   public,
		private:  private,
		listener: listener,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+KeySetPath, authority.serveKeySet)
	authority.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = authority.server.Serve(listener) }()
	return authority, nil
}

// Issuer is the issuer this run stamps and a verifier must pin.
func (a *Authority) Issuer() string { return a.issuer }

// KeySetURL is the full route the key set is served at.
func (a *Authority) KeySetURL() string {
	return fmt.Sprintf("http://%s%s", a.listener.Addr().String(), KeySetPath)
}

// Signer is the run's private key, for minting capabilities under this issuer.
func (a *Authority) Signer() (string, ed25519.PrivateKey) { return keyID, a.private }

// Close stops serving the key set.
func (a *Authority) Close() error {
	if a == nil || a.server == nil {
		return nil
	}
	return a.server.Close()
}

// keySet is the document a verifier parses: a JWKS carrying Ed25519 signing
// keys, which is the only shape the Work Context verifier admits.
func (a *Authority) keySet() any {
	return map[string]any{"keys": []map[string]string{{
		"kty": "OKP",
		"crv": "Ed25519",
		"use": "sig",
		"alg": "EdDSA",
		"kid": keyID,
		"x":   base64.RawURLEncoding.EncodeToString(a.public),
	}}}
}

func (a *Authority) serveKeySet(w http.ResponseWriter, _ *http.Request) {
	document, err := json.Marshal(a.keySet())
	if err != nil {
		http.Error(w, "cannot encode the key set", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/jwk-set+json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(document)
}
