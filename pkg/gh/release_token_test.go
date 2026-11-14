package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGH puts a gh on PATH that appends its arguments to a file and answers
// with script, clears the environment's tokens so the login is what is asked,
// and points the login check at an API that accepts every token. It answers
// the file gh records its calls in.
func fakeGH(t *testing.T, script string) (calls string) {
	t.Helper()
	bin := t.TempDir()
	calls = filepath.Join(bin, "calls")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + calls + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	githubAPI(t, http.StatusOK)
	return calls
}

// githubAPI answers the login check with status, and counts the checks.
func githubAPI(t *testing.T, status int) *atomic.Int32 {
	t.Helper()
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	previous := releaseAPI
	releaseAPI = server.URL
	t.Cleanup(func() { releaseAPI = previous })
	return &checks
}

func callsOf(t *testing.T, calls string) []string {
	t.Helper()
	recorded, err := os.ReadFile(calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(recorded)), "\n")
}

func TestReleaseTokenSourceReadsTheGHLoginForGitHubOnceWhenTheEnvironmentHasNone(t *testing.T) {
	calls := fakeGH(t, `[ "$*" = "auth token --hostname github.com" ] || exit 1; printf 'gho_fromlogin\n'`)
	source := ReleaseTokenSource(t.Context())
	for range 3 {
		if token, tried := source(); token != "gho_fromlogin" || tried != nil {
			t.Fatalf("source() = %q, %v; want the gh login's token", token, tried)
		}
	}
	if got := callsOf(t, calls); len(got) != 1 || got[0] != "auth token --hostname github.com" {
		t.Fatalf("gh was asked %q; want once, for github.com", got)
	}
}

func TestReleaseTokenSourcePrefersTheEnvironmentAndNeverAsksGH(t *testing.T) {
	for _, variable := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Run(variable, func(t *testing.T) {
			calls := fakeGH(t, `printf 'gho_fromlogin\n'`)
			t.Setenv(variable, "from-environment")
			if token, _ := ReleaseTokenSource(t.Context())(); token != "from-environment" {
				t.Fatalf("source() = %q, want the environment's", token)
			}
			if got := callsOf(t, calls); got != nil {
				t.Fatalf("gh was invoked although %s is set: %q", variable, got)
			}
		})
	}
}

// A revoked login is no credential: handing it over would turn every public
// download into a 401, where the loader would otherwise read anonymously.
func TestReleaseTokenSourceWithholdsALoginGitHubRefuses(t *testing.T) {
	fakeGH(t, `printf 'gho_revoked\n'`)
	githubAPI(t, http.StatusUnauthorized)
	token, tried := ReleaseTokenSource(t.Context())()
	if token != "" {
		t.Fatalf("source() handed over %q, which GitHub refuses", token)
	}
	if joined := strings.Join(tried, ", "); !strings.Contains(joined, "GitHub refused the login gh holds (401)") || strings.Contains(joined, "gho_revoked") {
		t.Fatalf("tried = %q", joined)
	}
}

// What gh printed is never relayed: a gh that echoes a credential and fails
// must not see it in the refusal, which is printed.
func TestReleaseTokenSourceTellsTheFailuresApartAndNeverRelaysWhatGHPrinted(t *testing.T) {
	const leaked = "gho_printedbygh"
	for name, scenario := range map[string]struct{ script, reason string }{
		"not logged in": {"printf 'no oauth token found for github.com " + leaked + "\\n' >&2; printf '" + leaked + "\\n'; exit 1", "gh is not logged in to it; run `gh auth login`"},
		"failed":        {"printf 'keyring locked " + leaked + "\\n' >&2; exit 4", "gh could not answer a token for it (exit status 4)"},
		"empty":         {"exit 0", "gh answered no token"},
	} {
		t.Run(name, func(t *testing.T) {
			fakeGH(t, scenario.script)
			token, tried := ReleaseTokenSource(t.Context())()
			joined := strings.Join(tried, ", ")
			if token != "" || strings.Contains(joined, leaked) {
				t.Fatalf("source() = %q, tried %q", token, joined)
			}
			for _, want := range []string{"GITHUB_TOKEN", "GH_TOKEN", "gh CLI login for github.com", scenario.reason} {
				if !strings.Contains(joined, want) {
					t.Fatalf("tried %q does not name %q", joined, want)
				}
			}
		})
	}
}

func TestReleaseTokenSourceSaysWhenGHIsNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	token, tried := ReleaseTokenSource(t.Context())()
	if token != "" || !strings.Contains(strings.Join(tried, ", "), "gh is not installed") {
		t.Fatalf("source() = %q, %v", token, tried)
	}
}

// The command's context ends the lookup, and a cancelled lookup is asked
// again rather than remembered as "no login".
func TestReleaseTokenSourceStopsWithTheCommandAndForgetsACancelledLookup(t *testing.T) {
	fakeGH(t, `exec /bin/sleep 30`)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	source := ReleaseTokenSource(ctx)
	started := time.Now()
	token, tried := source()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the lookup outlived its command by %s", elapsed)
	}
	if token != "" || !strings.Contains(strings.Join(tried, ", "), "cancelled") {
		t.Fatalf("source() = %q, %v", token, tried)
	}
	if _, again := source(); !strings.Contains(strings.Join(again, ", "), "cancelled") {
		t.Fatalf("a cancelled lookup was remembered as %v", again)
	}
}

// A gh that exits but leaves a child holding its output cannot hold the
// lookup past the bound.
func TestLoginTokenDoesNotWaitForAChildGHLeftBehind(t *testing.T) {
	fakeGH(t, `/bin/sleep 30 & printf 'gho_fromlogin\n'; exit 1`)
	started := time.Now()
	loginToken(t.Context(), releaseHost)
	if elapsed := time.Since(started); elapsed > loginWaitDelay+5*time.Second {
		t.Fatalf("loginToken waited %s for gh's child", elapsed)
	}
}
