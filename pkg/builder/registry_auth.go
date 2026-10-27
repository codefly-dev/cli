package builder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/codefly-dev/core/wool"
)

// RegistryLogin runs whatever credential-helper sequence is needed for
// `docker push` to authenticate against a remote registry, based on
// env.Registry.Auth. No-op when auth is empty (Docker Hub anonymous,
// or assume creds already in ~/.docker/config.json).
//
// Supported auth values:
//
//	""     — anonymous / pre-existing creds. No-op.
//	"ecr"  — Amazon ECR. Runs `aws ecr get-login-password --region <r>`
//	         and pipes to `docker login <registry-host> -u AWS
//	         --password-stdin`. Region is inferred from the URL
//	         (<account>.dkr.ecr.<region>.amazonaws.com).
//	"acr"  — Azure Container Registry. Runs `az acr login --name <name>
//	         --expose-token` and pipes the token to `docker login
//	         <name>.azurecr.io -u 00000000-0000-0000-0000-000000000000
//	         --password-stdin`. Name is inferred from the URL
//	         (<name>.azurecr.io).
//	"gar"  — Google Artifact Registry. Stubbed (returns ErrUnimplemented)
//	         until someone needs it.
//	"ghcr" — GitHub Container Registry. The token is GITHUB_TOKEN, else
//	         GH_TOKEN, else `gh auth token --hostname github.com`; the user
//	         is GITHUB_ACTOR, else `gh api user --jq .login`. The token is
//	         piped to `docker login ghcr.io --password-stdin`.
//
// Called from cli/cmd/build/service.go before flow.Build runs. Failures
// here are loud — bad to discover at push time after a long build.
func RegistryLogin(ctx context.Context, registryURL, authKind string) error {
	w := wool.Get(ctx).In("RegistryLogin", wool.Field("auth", authKind), wool.Field("registry", registryURL))

	switch strings.ToLower(strings.TrimSpace(authKind)) {
	case "":
		w.Debug("no registry auth declared; assuming pre-existing docker creds")
		return nil
	case "ecr":
		return ecrLogin(ctx, registryURL)
	case "acr":
		return acrLogin(ctx, registryURL)
	case "gar", "gcr":
		return fmt.Errorf("registry auth %q not yet wired — file an issue or add it in cli/pkg/builder/registry_auth.go", authKind)
	case "ghcr":
		return ghcrLogin(ctx)
	default:
		return fmt.Errorf("unknown registry auth %q (supported: ecr, acr, gar, ghcr)", authKind)
	}
}

// ecrRegion extracts the AWS region from an ECR registry URL of the
// form <account>.dkr.ecr.<region>.amazonaws.com[/<repo>...]. Returns
// the empty string when the URL doesn't match the ECR shape; callers
// treat that as "not ECR; bail rather than guess".
var ecrRegionRe = regexp.MustCompile(`^[0-9]+\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com`)

func ecrRegion(registryURL string) string {
	// Strip protocol if any so users can paste either form.
	u := strings.TrimPrefix(registryURL, "https://")
	u = strings.TrimPrefix(u, "http://")
	m := ecrRegionRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return m[1]
}

// ecrHost returns just the host portion of the ECR URL (no path),
// because `docker login` wants the registry host, not the repo path.
func ecrHost(registryURL string) string {
	u := strings.TrimPrefix(registryURL, "https://")
	u = strings.TrimPrefix(u, "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		return u[:i]
	}
	return u
}

func ecrLogin(ctx context.Context, registryURL string) error {
	w := wool.Get(ctx).In("ecrLogin", wool.Field("registry", registryURL))

	region := ecrRegion(registryURL)
	if region == "" {
		return fmt.Errorf("env.Registry.Auth=ecr but URL %q is not a valid ECR registry (expected <account>.dkr.ecr.<region>.amazonaws.com[/repo])", registryURL)
	}
	host := ecrHost(registryURL)

	// `aws ecr get-login-password` produces a short-lived (12h) token
	// on stdout. Pipe it into `docker login --password-stdin` so the
	// password never appears as a process argument (would be visible
	// in `ps`).
	pwCmd := exec.CommandContext(ctx, "aws", "ecr", "get-login-password", "--region", region)
	var pwOut, pwErr bytes.Buffer
	pwCmd.Stdout = &pwOut
	pwCmd.Stderr = &pwErr
	if err := pwCmd.Run(); err != nil {
		return fmt.Errorf("aws ecr get-login-password failed: %v: %s", err, pwErr.String())
	}

	loginCmd := exec.CommandContext(ctx, "docker", "login", "--username", "AWS", "--password-stdin", host)
	loginCmd.Stdin = &pwOut
	var loginOut, loginErr bytes.Buffer
	loginCmd.Stdout = &loginOut
	loginCmd.Stderr = &loginErr
	if err := loginCmd.Run(); err != nil {
		return fmt.Errorf("docker login %s failed: %v: %s", host, err, loginErr.String())
	}

	w.Info("ECR login succeeded", wool.Field("host", host), wool.Field("region", region))
	return nil
}

// acrName extracts the registry name from an ACR registry URL of the
// form <name>.azurecr.io[/<repo>...]. Returns the empty string when the
// URL doesn't match the ACR shape; callers treat that as "not ACR; bail
// rather than guess". The trailing boundary is required so a typo'd host
// like <name>.azurecr.io.example.com is rejected here rather than
// silently accepted and pushed to the wrong place at push time.
var acrNameRe = regexp.MustCompile(`^([a-z0-9]+)\.azurecr\.io(?:[/:]|$)`)

func acrName(registryURL string) string {
	// Strip protocol if any so users can paste either form.
	u := strings.TrimPrefix(registryURL, "https://")
	u = strings.TrimPrefix(u, "http://")
	m := acrNameRe.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return m[1]
}

// acrTokenUser is the fixed username ACR expects for token-based
// `docker login`; the token itself carries the identity.
const acrTokenUser = "00000000-0000-0000-0000-000000000000"

func acrLogin(ctx context.Context, registryURL string) error {
	w := wool.Get(ctx).In("acrLogin", wool.Field("registry", registryURL))

	name := acrName(registryURL)
	if name == "" {
		return fmt.Errorf("env.Registry.Auth=acr but URL %q is not a valid ACR registry (expected <name>.azurecr.io[/repo])", registryURL)
	}
	host := name + ".azurecr.io"

	// `az acr login --expose-token` mints a short-lived ACR refresh token
	// from the caller's Azure identity (CLI login / OIDC) without touching
	// a docker credential helper, so it works in bare CI environments. The
	// token lands on stdout; pipe it into `docker login --password-stdin`
	// so it never appears as a process argument (would be visible in `ps`).
	tokenCmd := exec.CommandContext(ctx, "az", "acr", "login", "--name", name, "--expose-token", "--query", "accessToken", "--output", "tsv")
	var tokenOut, tokenErr bytes.Buffer
	tokenCmd.Stdout = &tokenOut
	tokenCmd.Stderr = &tokenErr
	if err := tokenCmd.Run(); err != nil {
		return fmt.Errorf("az acr login --name %s --expose-token failed: %v: %s", name, err, tokenErr.String())
	}

	loginCmd := exec.CommandContext(ctx, "docker", "login", "--username", acrTokenUser, "--password-stdin", host)
	loginCmd.Stdin = &tokenOut
	var loginOut, loginErr bytes.Buffer
	loginCmd.Stdout = &loginOut
	loginCmd.Stderr = &loginErr
	if err := loginCmd.Run(); err != nil {
		return fmt.Errorf("docker login %s failed: %v: %s", host, err, loginErr.String())
	}

	w.Info("ACR login succeeded", wool.Field("host", host), wool.Field("name", name))
	return nil
}

// ghcrHost is the registry the ghcr auth logs in to.
const ghcrHost = "ghcr.io"

// runRegistryCommand runs one credential command with stdin and returns its
// stdout; its stderr is folded into the error. A seam so a test needs neither
// gh nor docker.
var runRegistryCommand = func(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// lookupRegistryEnv reads the environment; a seam for tests.
var lookupRegistryEnv = os.Getenv

// ghcrLogin logs docker in to ghcr.io with the caller's GitHub identity: a CI
// token from the environment, or the GitHub CLI's login. The token only ever
// reaches docker on stdin and is never logged.
func ghcrLogin(ctx context.Context) error {
	w := wool.Get(ctx).In("ghcrLogin")
	token := firstNonEmpty(lookupRegistryEnv("GITHUB_TOKEN"), lookupRegistryEnv("GH_TOKEN"))
	if token == "" {
		out, err := runRegistryCommand(ctx, nil, "gh", "auth", "token", "--hostname", "github.com")
		if err != nil {
			return fmt.Errorf("registry auth ghcr: no GITHUB_TOKEN or GH_TOKEN, and `gh auth token` failed: %w", err)
		}
		token = strings.TrimSpace(string(out))
	}
	if token == "" {
		return fmt.Errorf("registry auth ghcr: no GitHub token (set GITHUB_TOKEN or run `gh auth login`)")
	}
	user := lookupRegistryEnv("GITHUB_ACTOR")
	if user == "" {
		out, err := runRegistryCommand(ctx, nil, "gh", "api", "user", "--jq", ".login")
		if err != nil {
			return fmt.Errorf("registry auth ghcr: no GITHUB_ACTOR, and `gh api user` failed: %w", err)
		}
		user = strings.TrimSpace(string(out))
	}
	if user == "" {
		return fmt.Errorf("registry auth ghcr: no GitHub user (set GITHUB_ACTOR or run `gh auth login`)")
	}
	if _, err := runRegistryCommand(ctx, strings.NewReader(token), "docker", "login", "--username", user, "--password-stdin", ghcrHost); err != nil {
		return fmt.Errorf("docker login %s failed: %w", ghcrHost, err)
	}
	w.Info("GHCR login succeeded", wool.Field("host", ghcrHost), wool.Field("user", user))
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
