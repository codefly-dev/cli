package gh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
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
	source := ReleaseTokenSource(t.Context(), nil)
	for range 3 {
		if token, sources := source(); token != "gho_fromlogin" || len(sources) != 1 || sources[0] != "the gh login (github.com)" {
			t.Fatalf("source() = %q, %v; want the gh login's token, named as its source", token, sources)
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
			if token, sources := ReleaseTokenSource(t.Context(), nil)(); token != "from-environment" || len(sources) != 1 || sources[0] != variable {
				t.Fatalf("source() = %q, %v; want the environment's, named %s", token, sources, variable)
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
	token, tried := ReleaseTokenSource(t.Context(), nil)()
	if token != "" {
		t.Fatalf("source() handed over %q, which GitHub refuses", token)
	}
	if joined := strings.Join(tried, ", "); !strings.Contains(joined, "GitHub refused the login gh holds (401)") || strings.Contains(joined, "gho_revoked") {
		t.Fatalf("tried = %q", joined)
	}
}

// Only GitHub's 401 says a login is no credential. A 403 (a rate limit, a
// policy) or a 404 says nothing about the token, so it is still handed over
// and the download reports what GitHub says.
func TestReleaseTokenSourceWithholdsALoginOnlyOnA401(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fakeGH(t, `printf 'gho_fromlogin\n'`)
			checks := githubAPI(t, status)
			token, _ := ReleaseTokenSource(t.Context(), nil)()
			if checks.Load() != 1 {
				t.Fatalf("the login was checked %d times, want once", checks.Load())
			}
			if withheld := token == ""; withheld != (status == http.StatusUnauthorized) {
				t.Fatalf("GitHub answered %d and the source answered %q", status, token)
			}
		})
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
			token, tried := ReleaseTokenSource(t.Context(), nil)()
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
	token, tried := ReleaseTokenSource(t.Context(), nil)()
	if token != "" || !strings.Contains(strings.Join(tried, ", "), "gh is not installed") {
		t.Fatalf("source() = %q, %v", token, tried)
	}
}

// The command's context ends the lookup.
func TestReleaseTokenSourceStopsWithTheCommand(t *testing.T) {
	fakeGH(t, `exec /bin/sleep 30`)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	started := time.Now()
	token, tried := ReleaseTokenSource(ctx, nil)()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the lookup outlived its command by %s", elapsed)
	}
	if token != "" || !strings.Contains(strings.Join(tried, ", "), "cancelled") {
		t.Fatalf("source() = %q, %v", token, tried)
	}
}

// reopenedContext is cancelled until reopen is called: a context a real
// command never has, and the only way to observe whether the source asked gh
// again after a cancelled lookup rather than answering from memory.
type reopenedContext struct {
	context.Context
	cancelled atomic.Bool
	done      chan struct{}
}

func newReopenedContext() *reopenedContext {
	c := &reopenedContext{Context: context.Background(), done: make(chan struct{})}
	close(c.done)
	c.cancelled.Store(true)
	return c
}

func (c *reopenedContext) reopen() { c.cancelled.Store(false) }

func (c *reopenedContext) Done() <-chan struct{} {
	if c.cancelled.Load() {
		return c.done
	}
	return nil
}

func (c *reopenedContext) Err() error {
	if c.cancelled.Load() {
		return context.Canceled
	}
	return nil
}

// A cancelled lookup is not remembered as "no login": the next ask runs gh.
func TestReleaseTokenSourceDoesNotRememberACancelledLookup(t *testing.T) {
	fakeGH(t, `printf 'gho_fromlogin\n'`)
	ctx := newReopenedContext()
	source := ReleaseTokenSource(ctx, nil)
	if token, tried := source(); token != "" || !strings.Contains(strings.Join(tried, ", "), "cancelled") {
		t.Fatalf("a cancelled lookup answered %q, %v", token, tried)
	}
	ctx.reopen()
	if token, tried := source(); token != "gho_fromlogin" {
		t.Fatalf("after a cancelled lookup the source answered %q, %v; want gh asked again", token, tried)
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

// interruptGH puts a gh on PATH that, asked the first time, marks that it was
// asked and hangs; asked again, it answers a token. It answers the mark.
func interruptGH(t *testing.T) (asked string) {
	t.Helper()
	asked = filepath.Join(t.TempDir(), "asked")
	fakeGH(t, `if [ -e `+asked+` ]; then printf 'gho_fromlogin\n'; exit 0; fi; : > `+asked+`; exec /bin/sleep 30`)
	return asked
}

func waitFor(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A Ctrl-C while gh is asked ends the lookup, in a command that handles the
// signal itself: the signal is not handed back (that handler had it), the
// lookup is not remembered, and the next ask runs gh again.
func TestAnInterruptEndsTheLoginLookup(t *testing.T) {
	asked := interruptGH(t)
	source := ReleaseTokenSource(t.Context(), func() bool { return true })
	go func() {
		waitFor(t, asked)
		_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
	}()
	started := time.Now()
	token, tried := source()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the interrupted lookup took %s", elapsed)
	}
	if token != "" || !strings.Contains(strings.Join(tried, ", "), "cancelled") {
		t.Fatalf("source() = %q, %v; want the lookup cancelled", token, tried)
	}
	if token, _ := source(); token != "gho_fromlogin" {
		t.Fatalf("after an interrupted lookup the source answered %q; want gh asked again", token)
	}
}

// In a command that does not handle signals, a Ctrl-C while gh is asked still
// ends the process at once, as it would have without the lookup listening:
// the lookup hands the signal back to the default action.
func TestAnInterruptDuringTheLookupStillEndsAProcessThatDoesNotHandleIt(t *testing.T) {
	if dir := os.Getenv("GH_INTERRUPT_HELPER"); dir != "" {
		t.Setenv("PATH", dir)
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		ReleaseTokenSource(context.Background(), func() bool { return false })()
		time.Sleep(10 * time.Second) // a command that never reads its context
		return
	}
	asked := interruptGH(t)
	helper := exec.Command(os.Args[0], "-test.run=^TestAnInterruptDuringTheLookupStillEndsAProcessThatDoesNotHandleIt$")
	helper.Env = append(os.Environ(), "GH_INTERRUPT_HELPER="+os.Getenv("PATH"))
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, asked)
	started := time.Now()
	if err := helper.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := helper.Wait()
	status, _ := helper.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGINT {
		t.Fatalf("the helper ended with %v (%v); want it ended by SIGINT", helper.ProcessState, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the helper outlived the SIGINT by %s", elapsed)
	}
}
