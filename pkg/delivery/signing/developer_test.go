package signing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
)

const (
	developerIssuer  = "https://github.com/login/oauth"
	developerSubject = "dev@example.test"
)

// fakeToken is an unsigned JWT carrying claims: enough for everything this
// package reads from a token, which it never verifies (Fulcio does).
func fakeToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(payload) + "." + encode([]byte("signature"))
}

func developerPolicy() *DeveloperPolicy {
	return &DeveloperPolicy{Developers: []DeveloperIdentity{{Issuer: developerIssuer, Subject: developerSubject}}}
}

// TestDeveloperCheckAdmitsOnlyTheListedDevelopers runs the check a local
// publish holds every carrier to against bundles an in-memory Sigstore signed:
// the listed developer verifies; another person, or the listed address
// certified by another provider, is refused as a signature that fails; and a
// bundle with no transparency evidence is refused by its own name.
func TestDeveloperCheckAdmitsOnlyTheListedDevelopers(t *testing.T) {
	payload := []byte("presence document")
	check := func(release virtualRelease) error {
		t.Helper()
		selfCheck, err := developerCheck(developerPolicy(), func(context.Context) (*root.TrustedRoot, error) { return release.trusted, nil })
		if err != nil {
			t.Fatal(err)
		}
		return selfCheck(context.Background(), release.bundle, release.payload)
	}
	listed := signVirtually(t, developerSubject, developerIssuer, payload, false)
	if err := check(listed); err != nil {
		t.Fatalf("the listed developer's carrier must verify: %v", err)
	}
	for name, release := range map[string]virtualRelease{
		"another person":   signVirtually(t, "someone@example.test", developerIssuer, payload, false),
		"another provider": signVirtually(t, developerSubject, "https://accounts.google.com", payload, false),
		"the release":      signVirtually(t, releaseSubject, releaseIssuer, payload, false),
	} {
		if err := check(release); !errors.Is(err, ErrSignature) {
			t.Errorf("%s: a carrier the local host does not admit must be refused as a failing signature: %v", name, err)
		}
	}
	bare := virtualRelease{payload: listed.payload, bundle: withoutTransparencyLog(t, listed.bundle), trusted: listed.trusted}
	if err := check(bare); !errors.Is(err, ErrNoTransparency) {
		t.Fatalf("a carrier with no transparency evidence must be refused by name: %v", err)
	}
	if _, err := DeveloperCheckUnder(&DeveloperPolicy{}); !errors.Is(err, ErrPolicy) {
		t.Fatalf("a policy naming no developer admits nobody and must be refused: %v", err)
	}
}

// fulcioRecorder stands in for Fulcio: it records the requests that reach it
// and refuses them, so a test can tell whether a token was ever sent and
// which one, without a certificate being issued.
type fulcioRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (recorder *fulcioRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(body))
		recorder.mu.Unlock()
		http.Error(w, "refused by the test", http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	return server
}

// TestDeveloperSignerSendsOnlyAnAdmittedTokenToFulcio: the developer signer
// is the release signer with another token source. A token naming a
// developer the local host does not list never reaches Fulcio — nothing is
// certified or logged for it — and an admitted one is the token Fulcio is
// asked to certify.
func TestDeveloperSignerSendsOnlyAnAdmittedTokenToFulcio(t *testing.T) {
	recorder := &fulcioRecorder{}
	server := recorder.server(t)
	exp := time.Now().Add(time.Hour).Unix()
	sign := func(token string) error {
		t.Helper()
		signer, err := NewDeveloper(developerPolicy(), func(context.Context) (string, error) { return token, nil },
			KeylessOptions{FulcioURL: server.URL, RekorURL: server.URL})
		if err != nil {
			t.Fatal(err)
		}
		_, err = signer.Sign(context.Background(), []byte("presence document"))
		return err
	}

	stranger := fakeToken(t, map[string]any{"email": "someone@example.test", "email_verified": true, "exp": exp})
	if err := sign(stranger); !errors.Is(err, ErrDeveloperNotAdmitted) {
		t.Fatalf("a token naming an unlisted developer must be refused before signing: %v", err)
	}
	unverified := fakeToken(t, map[string]any{"email": developerSubject, "email_verified": false, "exp": exp})
	if err := sign(unverified); err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("a token whose email is not verified must be refused before signing: %v", err)
	}
	if err := sign("not-a-jwt"); err == nil || !strings.Contains(err.Error(), "not a JWT") {
		t.Fatalf("a token that is not a JWT must be refused before signing: %v", err)
	}
	if len(recorder.requests) != 0 {
		t.Fatalf("a refused token reached Fulcio: %v", recorder.requests)
	}

	admitted := fakeToken(t, map[string]any{"email": developerSubject, "email_verified": "true", "exp": exp})
	if err := sign(admitted); err == nil {
		t.Fatal("the stand-in Fulcio refuses every request, so signing must fail")
	}
	if len(recorder.requests) == 0 || !strings.Contains(recorder.requests[0], admitted) {
		t.Fatalf("the admitted developer token is not what Fulcio was asked to certify: %v", recorder.requests)
	}
}

// TestDeveloperTokenIsReusedUntilItNearlyExpires: one publish signs several
// documents, and a browser login per document is not a flow anyone runs, so a
// token is reused until it is within the margin of its expiry; a token whose
// expiry cannot be read is never reused.
func TestDeveloperTokenIsReusedUntilItNearlyExpires(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	obtained := 0
	var next string
	source := reusedToken(func(context.Context) (string, error) { obtained++; return next, nil }, clock)
	ctx := context.Background()

	next = fakeToken(t, map[string]any{"sub": "a", "exp": now.Add(10 * time.Minute).Unix()})
	for range 3 {
		if token, err := source(ctx); err != nil || token != next {
			t.Fatalf("token %q err %v", token, err)
		}
	}
	if obtained != 1 {
		t.Fatalf("a valid token was obtained %d times, want once", obtained)
	}
	now = now.Add(10*time.Minute - tokenReuseMargin + time.Second)
	if _, err := source(ctx); err != nil || obtained != 2 {
		t.Fatalf("a token about to expire was reused (obtained %d, err %v)", obtained, err)
	}
	next = fakeToken(t, map[string]any{"sub": "a"})
	_, _ = source(ctx)
	_, _ = source(ctx)
	_, _ = source(ctx)
	if obtained != 5 {
		t.Fatalf("a token with no expiry was reused (obtained %d, want 5)", obtained)
	}
}

// TestDeveloperIDTokenPrefersTheDevelopersOwnToken: a token the developer put
// in SIGSTORE_ID_TOKEN is used as is — the browser login is for when there is
// none — and the login preselects the provider only when every listed
// developer is certified by the same one the public-good login federates.
func TestDeveloperIDTokenPrefersTheDevelopersOwnToken(t *testing.T) {
	token, err := DeveloperIDToken(lookupFrom(map[string]string{envSigstoreIDToken: " a.b.c \n"}), developerPolicy())(context.Background())
	if err != nil || token != "a.b.c" {
		t.Fatalf("token %q err %v", token, err)
	}
	if got := connectorFor(developerPolicy()); got != developerIssuer {
		t.Fatalf("connector %q, want the one provider every developer uses", got)
	}
	mixed := &DeveloperPolicy{Developers: []DeveloperIdentity{
		{Issuer: developerIssuer, Subject: developerSubject},
		{Issuer: "https://accounts.google.com", Subject: "other@example.test"},
	}}
	if got := connectorFor(mixed); got != "" {
		t.Fatalf("connector %q preselected for developers of two providers", got)
	}
	if got := connectorFor(&DeveloperPolicy{Developers: []DeveloperIdentity{{Issuer: "https://idp.example.test", Subject: "x"}}}); got != "" {
		t.Fatalf("connector %q preselected for a provider the public-good login does not federate", got)
	}
}

// TestADeveloperSignerReadsSIGSTORE_ID_TOKEN: with no token source given, a
// developer signer takes the token the developer exported — no browser — and
// that token, held to the policy, is the one Fulcio is asked to certify.
func TestADeveloperSignerReadsSIGSTORE_ID_TOKEN(t *testing.T) {
	recorder := &fulcioRecorder{}
	server := recorder.server(t)
	token := fakeToken(t, map[string]any{"email": developerSubject, "email_verified": true, "exp": time.Now().Add(time.Hour).Unix()})
	t.Setenv(envSigstoreIDToken, token)
	signer, err := NewDeveloper(developerPolicy(), nil, KeylessOptions{FulcioURL: server.URL, RekorURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = signer.Sign(context.Background(), []byte("authority document")); err == nil {
		t.Fatal("the stand-in Fulcio refuses every request, so signing must fail")
	}
	if len(recorder.requests) == 0 || !strings.Contains(recorder.requests[0], token) {
		t.Fatalf("SIGSTORE_ID_TOKEN is not the token Fulcio was asked to certify: %v", recorder.requests)
	}
}
