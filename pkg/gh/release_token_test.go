package gh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH puts a gh on PATH that records its arguments and answers with script,
// and clears the environment's tokens so the login is what is asked.
func fakeGH(t *testing.T, script string) (argsFile string) {
	t.Helper()
	bin := t.TempDir()
	argsFile = filepath.Join(bin, "args")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + argsFile + "\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return argsFile
}

func TestReleaseTokenReadsTheGHLoginForGitHubWhenTheEnvironmentHasNone(t *testing.T) {
	args := fakeGH(t, `[ "$*" = "auth token --hostname github.com" ] || exit 1; printf 'gho_fromlogin\n'`)
	token, tried := ReleaseToken()
	if token != "gho_fromlogin" || tried != nil {
		t.Fatalf("ReleaseToken() = %q, %v; want the gh login's token", token, tried)
	}
	recorded, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(recorded)); got != "auth token --hostname github.com" {
		t.Fatalf("gh was asked %q", got)
	}
}

func TestReleaseTokenPrefersTheEnvironmentAndNeverAsksGH(t *testing.T) {
	for _, variable := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		t.Run(variable, func(t *testing.T) {
			args := fakeGH(t, `printf 'gho_fromlogin\n'`)
			t.Setenv(variable, "from-environment")
			if token, _ := ReleaseToken(); token != "from-environment" {
				t.Fatalf("ReleaseToken() = %q, want the environment's", token)
			}
			if _, err := os.Stat(args); !os.IsNotExist(err) {
				t.Fatalf("gh was invoked although %s is set", variable)
			}
		})
	}
}

// A failing gh says nothing it printed: a gh that echoes a credential on its
// standard error must not see it relayed into the refusal, which is printed.
func TestReleaseTokenNamesEverySourceTriedAndNeverRelaysWhatGHPrinted(t *testing.T) {
	const leaked = "gho_printedonstderr"
	fakeGH(t, "printf '"+leaked+"\\n' >&2; printf '"+leaked+"\\n'; exit 1")
	token, tried := ReleaseToken()
	if token != "" {
		t.Fatalf("ReleaseToken() = %q from a gh that failed", token)
	}
	joined := strings.Join(tried, ", ")
	for _, source := range []string{"GITHUB_TOKEN", "GH_TOKEN", "gh CLI login for github.com", "gh auth login"} {
		if !strings.Contains(joined, source) {
			t.Fatalf("tried %q does not name %q", joined, source)
		}
	}
	if strings.Contains(joined, leaked) {
		t.Fatalf("tried %q relays what gh printed", joined)
	}
}

func TestReleaseTokenSaysWhenGHIsNotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	token, tried := ReleaseToken()
	if token != "" || !strings.Contains(strings.Join(tried, ", "), "gh is not installed") {
		t.Fatalf("ReleaseToken() = %q, %v", token, tried)
	}
}
