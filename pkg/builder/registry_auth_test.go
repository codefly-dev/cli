package builder

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestACRName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"acmestaging.azurecr.io", "acmestaging"},
		{"acmestaging.azurecr.io/team/app:tag", "acmestaging"},
		{"acmestaging.azurecr.io:443/team/app", "acmestaging"},
		{"https://acmestaging.azurecr.io/team/app", "acmestaging"},
		{"http://acmestaging.azurecr.io", "acmestaging"},
		{"123abc.dkr.ecr.us-east-1.amazonaws.com", ""},
		{"docker.io/library/nginx", ""},
		// Typo'd hosts must be rejected, not loosely matched: the trailing
		// boundary stops <name>.azurecr.io from matching inside a longer host.
		{"acmestaging.azurecr.io.evil.com", ""},
		{"acmestaging.azurecr.iox.com", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := acrName(c.url); got != c.want {
			t.Errorf("acrName(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

type registryCall struct {
	argv  string
	stdin string
}

// stubGHCR replaces the environment and every credential command: gh answers
// with ghToken and ghUser, docker login succeeds.
func stubGHCR(t *testing.T, env map[string]string, ghToken, ghUser string) *[]registryCall {
	t.Helper()
	previousRun, previousEnv := runRegistryCommand, lookupRegistryEnv
	t.Cleanup(func() { runRegistryCommand, lookupRegistryEnv = previousRun, previousEnv })
	lookupRegistryEnv = func(key string) string { return env[key] }
	calls := &[]registryCall{}
	runRegistryCommand = func(_ context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
		call := registryCall{argv: name + " " + strings.Join(args, " ")}
		if stdin != nil {
			payload, _ := io.ReadAll(stdin)
			call.stdin = string(payload)
		}
		*calls = append(*calls, call)
		switch {
		case strings.HasPrefix(call.argv, "gh auth token"):
			return []byte(ghToken + "\n"), nil
		case strings.HasPrefix(call.argv, "gh api user"):
			return []byte(ghUser + "\n"), nil
		case strings.HasPrefix(call.argv, "docker login"):
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected %s", call.argv)
	}
	return calls
}

func TestGHCRLoginReadsTheTokenFromCIThenTheGitHubCLI(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		calls []string
		user  string
		token string
	}{
		{"ci", map[string]string{"GITHUB_TOKEN": "env-token", "GH_TOKEN": "other", "GITHUB_ACTOR": "ci-bot"}, nil, "ci-bot", "env-token"},
		{"gh token", map[string]string{"GH_TOKEN": "gh-env-token"}, []string{"gh api user --jq .login"}, "octocat", "gh-env-token"},
		{"gh cli", map[string]string{}, []string{"gh auth token --hostname github.com", "gh api user --jq .login"}, "octocat", "cli-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubGHCR(t, tc.env, "cli-token", "octocat")
			if err := RegistryLogin(t.Context(), "ghcr.io/obin-ai", "ghcr"); err != nil {
				t.Fatal(err)
			}
			var argv []string
			for _, call := range *calls {
				argv = append(argv, call.argv)
			}
			want := append(append([]string(nil), tc.calls...), "docker login --username "+tc.user+" --password-stdin ghcr.io")
			if strings.Join(argv, "\n") != strings.Join(want, "\n") {
				t.Fatalf("commands = %q, want %q", argv, want)
			}
			login := (*calls)[len(*calls)-1]
			if login.stdin != tc.token || strings.Contains(login.argv, tc.token) {
				t.Fatalf("the token must reach docker on stdin only: %+v", login)
			}
		})
	}
}

func TestGHCRLoginWithoutATokenFailsWithoutLeakingOne(t *testing.T) {
	stubGHCR(t, map[string]string{}, "", "octocat")
	err := RegistryLogin(t.Context(), "ghcr.io/obin-ai", "ghcr")
	if err == nil || !strings.Contains(err.Error(), "no GitHub token") {
		t.Fatalf("want a missing-token refusal, got %v", err)
	}
	stubGHCR(t, map[string]string{"GITHUB_TOKEN": "secret-token"}, "", "")
	err = RegistryLogin(t.Context(), "ghcr.io/obin-ai", "ghcr")
	if err == nil || !strings.Contains(err.Error(), "no GitHub user") || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("want a missing-user refusal naming no token, got %v", err)
	}
}
