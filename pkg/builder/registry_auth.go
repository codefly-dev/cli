package builder

import (
	"bytes"
	"context"
	"fmt"
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
//	"gar"  — Google Artifact Registry. Stubbed (returns ErrUnimplemented)
//	         until someone needs it.
//	"ghcr" — GitHub Container Registry. Stubbed.
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
	case "gar", "gcr":
		return fmt.Errorf("registry auth %q not yet wired — file an issue or add it in cli/pkg/builder/registry_auth.go", authKind)
	case "ghcr":
		return fmt.Errorf("registry auth %q not yet wired — set GITHUB_TOKEN and run `echo $GITHUB_TOKEN | docker login ghcr.io -u <user> --password-stdin` manually for now", authKind)
	default:
		return fmt.Errorf("unknown registry auth %q (supported: ecr, gar, ghcr)", authKind)
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
