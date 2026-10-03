package signing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGitHubActionsPolicyPinsOneWorkflowOnReleaseTags(t *testing.T) {
	policy, err := GitHubActionsPolicy("owner/repo", ".github/workflows/release.yaml", "refs/tags/v.*")
	if err != nil {
		t.Fatalf("GitHubActionsPolicy: %v", err)
	}
	if policy.Issuer != GitHubActionsIssuer {
		t.Fatalf("issuer = %q, want the GitHub Actions issuer %q", policy.Issuer, GitHubActionsIssuer)
	}
	pattern := regexp.MustCompile(policy.SubjectPattern)

	admitted := []string{
		"https://github.com/owner/repo/.github/workflows/release.yaml@refs/tags/v1.2.3",
		"https://github.com/owner/repo/.github/workflows/release.yaml@refs/tags/v0.1.172",
	}
	for _, subject := range admitted {
		if !pattern.MatchString(subject) {
			t.Errorf("pattern %q should admit %q", policy.SubjectPattern, subject)
		}
	}
	refused := map[string]string{
		"a branch, not a tag":           "https://github.com/owner/repo/.github/workflows/release.yaml@refs/heads/main",
		"another repository":            "https://github.com/owner/other/.github/workflows/release.yaml@refs/tags/v1.2.3",
		"a repository sharing a prefix": "https://github.com/owner/repo2/.github/workflows/release.yaml@refs/tags/v1.2.3",
		"another workflow":              "https://github.com/owner/repo/.github/workflows/ci.yaml@refs/tags/v1.2.3",
		"an unquoted dot":               "https://githubXcom/owner/repo/.github/workflows/release.yaml@refs/tags/v1.2.3",
		"a longer host":                 "https://github.com.evil.example/owner/repo/.github/workflows/release.yaml@refs/tags/v1.2.3",
	}
	for why, subject := range refused {
		if pattern.MatchString(subject) {
			t.Errorf("pattern %q should refuse %s: %q", policy.SubjectPattern, why, subject)
		}
	}

	// The ref pattern is anchored at the end: a stricter pattern admits the tag and nothing after it.
	strict, err := GitHubActionsPolicy("owner/repo", ".github/workflows/release.yaml", "refs/tags/v[0-9.]+")
	if err != nil {
		t.Fatalf("GitHubActionsPolicy: %v", err)
	}
	strictPattern := regexp.MustCompile(strict.SubjectPattern)
	if !strictPattern.MatchString("https://github.com/owner/repo/.github/workflows/release.yaml@refs/tags/v1.2.3") {
		t.Errorf("pattern %q should admit the release tag", strict.SubjectPattern)
	}
	for _, subject := range []string{
		"https://github.com/owner/repo/.github/workflows/release.yaml@refs/tags/v1.2.3 extra",
		"https://github.com/owner/repo/.github/workflows/release.yaml@refs/tags/v1.2.3-rc1",
	} {
		if strictPattern.MatchString(subject) {
			t.Errorf("pattern %q should refuse trailing content: %q", strict.SubjectPattern, subject)
		}
	}
}

func TestGitHubActionsPolicyRefusesEmptyParts(t *testing.T) {
	cases := map[string][3]string{
		"empty repository":      {"", ".github/workflows/release.yaml", "refs/tags/v.*"},
		"repository sans owner": {"repo", ".github/workflows/release.yaml", "refs/tags/v.*"},
		"empty workflow path":   {"owner/repo", "", "refs/tags/v.*"},
		"empty ref pattern":     {"owner/repo", ".github/workflows/release.yaml", ""},
		"invalid ref pattern":   {"owner/repo", ".github/workflows/release.yaml", "refs/tags/v(.*"},
	}
	for name, parts := range cases {
		_, err := GitHubActionsPolicy(parts[0], parts[1], parts[2])
		if !errors.Is(err, ErrPolicy) {
			t.Errorf("%s: err = %v, want ErrPolicy", name, err)
		}
	}
}

func TestFromEnvironmentWithoutAnIdentityCannotSign(t *testing.T) {
	signer := FromEnvironment(func(string) (string, bool) { return "", false })
	if _, isUnavailable := signer.(*Unavailable); !isUnavailable {
		t.Fatalf("signer = %T, want *Unavailable", signer)
	}
	_, err := signer.Sign(context.Background(), []byte("document"))
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("Sign err = %v, want ErrNoIdentity", err)
	}
	for _, want := range []string{"ACTIONS_ID_TOKEN_REQUEST_URL", "release workflow", "OIDC identity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %q", err, want)
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "private key") {
		t.Errorf("error %q must not suggest supplying a key", err)
	}
}

func TestFromEnvironmentInAGitHubActionsJobSignsKeylessly(t *testing.T) {
	env := map[string]string{
		"ACTIONS_ID_TOKEN_REQUEST_URL":   "https://actions.example/token?api-version=2.0",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "runtime-authorization",
	}
	signer := FromEnvironment(func(name string) (string, bool) {
		value, found := env[name]
		return value, found
	})
	keyless, isKeyless := signer.(*Keyless)
	if !isKeyless {
		t.Fatalf("signer = %T, want *Keyless", signer)
	}
	if keyless.options.FulcioURL != DefaultFulcioURL || keyless.options.RekorURL != DefaultRekorURL {
		t.Errorf("options = %+v, want the public-good defaults", keyless.options)
	}
	if keyless.options.TimestampURL != "" {
		t.Errorf("TimestampURL = %q, want none by default", keyless.options.TimestampURL)
	}
	if keyless.options.IDToken == nil || keyless.options.HTTPClient == nil {
		t.Errorf("IDToken and HTTPClient must be set: %+v", keyless.options)
	}

	// An empty value is as good as unset: GitHub never exports an empty endpoint.
	env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = ""
	if _, isUnavailable := FromEnvironment(func(name string) (string, bool) {
		value, found := env[name]
		return value, found
	}).(*Unavailable); !isUnavailable {
		t.Errorf("an empty ACTIONS_ID_TOKEN_REQUEST_TOKEN must leave the process without an identity")
	}
}

func TestGitHubActionsIDTokenRequestsTheSigstoreAudience(t *testing.T) {
	const token = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyZXBvOm93bmVyL3JlcG8ifQ.signature"
	var seenAuthorization, seenQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuthorization = r.Header.Get("Authorization")
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"value":"` + token + `"}`))
	}))
	defer server.Close()
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", server.URL+"/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "runtime-authorization")

	got, err := GitHubActionsIDToken(context.Background(), server.Client())
	if err != nil {
		t.Fatalf("GitHubActionsIDToken: %v", err)
	}
	if got != token {
		t.Errorf("token = %q, want %q", got, token)
	}
	if seenAuthorization != "bearer runtime-authorization" {
		t.Errorf("Authorization = %q, want the runtime token as a bearer", seenAuthorization)
	}
	query := regexp.MustCompile(`(^|&)audience=sigstore(&|$)`)
	if !query.MatchString(seenQuery) || !strings.Contains(seenQuery, "api-version=2.0") {
		t.Errorf("query = %q, want audience=sigstore added to the endpoint's own query", seenQuery)
	}
}

func TestGitHubActionsIDTokenReportsARefusal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"bad audience"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	_, err := requestIDToken(context.Background(), server.Client(), server.URL+"/token?api-version=2.0", "runtime-authorization")
	if err == nil {
		t.Fatal("a 401 from the token endpoint must be an error")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad audience") {
		t.Errorf("error %q should carry the status and the endpoint's answer", err)
	}
	if errors.Is(err, ErrNoIdentity) {
		t.Errorf("a refused request is not a missing identity: %v", err)
	}

	_, err = gitHubActionsIDToken(context.Background(), server.Client(), func(string) (string, bool) { return "", false })
	if !errors.Is(err, ErrNoIdentity) {
		t.Errorf("without the variables err = %v, want ErrNoIdentity", err)
	}
}

func TestNewKeylessAppliesDefaultsAndKeepsOverrides(t *testing.T) {
	defaults := NewKeyless(KeylessOptions{})
	if defaults.options.FulcioURL != DefaultFulcioURL || defaults.options.RekorURL != DefaultRekorURL {
		t.Errorf("defaults = %+v, want the public-good Fulcio and Rekor", defaults.options)
	}
	if defaults.options.HTTPClient != http.DefaultClient || defaults.options.IDToken == nil {
		t.Errorf("defaults must use http.DefaultClient and the GitHub Actions token: %+v", defaults.options)
	}

	client := &http.Client{}
	custom := NewKeyless(KeylessOptions{
		FulcioURL:    "https://fulcio.internal",
		RekorURL:     "https://rekor.internal",
		TimestampURL: "https://tsa.internal/api/v1/timestamp",
		HTTPClient:   client,
		IDToken:      func(context.Context) (string, error) { return "jwt", nil },
	})
	if custom.options.FulcioURL != "https://fulcio.internal" || custom.options.RekorURL != "https://rekor.internal" ||
		custom.options.TimestampURL != "https://tsa.internal/api/v1/timestamp" || custom.options.HTTPClient != client {
		t.Errorf("overrides were not kept: %+v", custom.options)
	}
}

// countingHandler fails the test's expectation of silence: any request is counted.
type countingHandler struct{ hits atomic.Int32 }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.hits.Add(1)
	http.Error(w, "unexpected request", http.StatusInternalServerError)
}

func TestKeylessSignStopsBeforeAnyNetworkCallWithoutAToken(t *testing.T) {
	handler := &countingHandler{}
	server := httptest.NewServer(handler)
	defer server.Close()

	tokenErr := errors.New("ACTIONS_ID_TOKEN_REQUEST_URL is not set")
	signer := NewKeyless(KeylessOptions{
		FulcioURL: server.URL,
		RekorURL:  server.URL,
		IDToken: func(context.Context) (string, error) {
			return "", errors.Join(tokenErr, ErrNoIdentity)
		},
	})
	_, err := signer.Sign(context.Background(), []byte("document"))
	if !errors.Is(err, ErrNoIdentity) || !errors.Is(err, tokenErr) {
		t.Fatalf("Sign err = %v, want the token failure wrapped", err)
	}
	if hits := handler.hits.Load(); hits != 0 {
		t.Errorf("Fulcio/Rekor were contacted %d time(s) without a token", hits)
	}

	empty := NewKeyless(KeylessOptions{
		FulcioURL: server.URL,
		RekorURL:  server.URL,
		IDToken:   func(context.Context) (string, error) { return "", nil },
	})
	if _, err := empty.Sign(context.Background(), []byte("document")); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("an empty token err = %v, want ErrNoIdentity", err)
	}
	if _, err := empty.Sign(context.Background(), nil); err == nil {
		t.Error("an empty document must not be signed")
	}
	if hits := handler.hits.Load(); hits != 0 {
		t.Errorf("Fulcio/Rekor were contacted %d time(s)", hits)
	}
}

func TestUnavailableSignNamesTheReason(t *testing.T) {
	_, err := Unavailable{Reason: "running on a laptop"}.Sign(context.Background(), []byte("document"))
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("err = %v, want ErrNoIdentity", err)
	}
	if !strings.HasPrefix(err.Error(), "running on a laptop: ") {
		t.Errorf("error %q should lead with the reason", err)
	}
	if _, err := (&Unavailable{}).Sign(context.Background(), nil); !errors.Is(err, ErrNoIdentity) {
		t.Errorf("a reasonless Unavailable err = %v, want ErrNoIdentity", err)
	}
}
