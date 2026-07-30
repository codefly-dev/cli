package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// workspaceFixture writes a real Codefly workspace — the resolution under test
// walks real manifests, so nothing here is stubbed.
func workspaceFixture(t *testing.T) (context.Context, *resources.Workspace, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	files := map[string]string{
		".gitignore": "**/configurations/local/*.secret.env\n",
		"workspace.codefly.yaml": `name: config-fixture
layout: modules
modules:
    - name: platform
environments:
    - name: local
    - name: staging
      configuration-profile: aws
`,
		"modules/platform/module.codefly.yaml": `kind: module
name: platform
project: config-fixture
services:
    - name: api
`,
		"modules/platform/services/api/service.codefly.yaml": `kind: service
name: api
version: 0.0.0
module: platform
agent:
    kind: codefly:service
    name: go-grpc
    version: 0.1.24
    publisher: codefly.dev
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "test@codefly.dev"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	}
	workspace, err := resources.LoadWorkspaceFromDir(ctx, dir)
	require.NoError(t, err)
	return ctx, workspace, dir
}

// resetFlags restores the package-level flag state every test mutates.
func resetFlags(t *testing.T) {
	t.Helper()
	previousService, previousEnv := serviceRef, environment
	t.Cleanup(func() { serviceRef, environment = previousService, previousEnv })
	serviceRef, environment = "", ""
}

func TestWorkspaceScopeResolvesToWorkspaceConfigurations(t *testing.T) {
	ctx, workspace, dir := workspaceFixture(t)
	resetFlags(t)

	tgt, root, err := targetIn(ctx, workspace, "internal-auth", true)
	require.NoError(t, err)
	require.Equal(t, dir, root)
	require.Equal(t, filepath.Join(dir, "configurations", "local", "internal-auth.secret.env"), tgt.Path())
}

func TestServiceScopeResolvesToServiceConfigurations(t *testing.T) {
	ctx, workspace, dir := workspaceFixture(t)
	resetFlags(t)
	serviceRef = "platform/api"

	tgt, _, err := targetIn(ctx, workspace, "runtime", true)
	require.NoError(t, err)
	require.Equal(t,
		filepath.Join(dir, "modules", "platform", "services", "api", "configurations", "local", "runtime.secret.env"),
		tgt.Path())
}

func TestUnknownServiceIsRejected(t *testing.T) {
	ctx, workspace, _ := workspaceFixture(t)
	resetFlags(t)
	serviceRef = "platform/missing"

	_, _, err := targetIn(ctx, workspace, "runtime", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "platform/missing")
}

func TestEnvironmentSelectsItsConfigurationProfile(t *testing.T) {
	ctx, workspace, dir := workspaceFixture(t)
	resetFlags(t)
	environment = "staging"

	tgt, _, err := targetIn(ctx, workspace, "edge", false)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "configurations", "aws", "edge.env"), tgt.Path(),
		"staging declares configuration-profile: aws, so it must not write configurations/staging")
}

func TestUndeclaredEnvironmentIsRejected(t *testing.T) {
	ctx, workspace, _ := workspaceFixture(t)
	resetFlags(t)
	environment = "production"

	_, _, err := targetIn(ctx, workspace, "edge", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "production")
}

func TestConfigurationNameWithPathSeparatorIsRejected(t *testing.T) {
	ctx, _, _ := workspaceFixture(t)
	resetFlags(t)

	_, _, err := resolveTargetName(ctx, "../escape")
	require.Error(t, err)
	require.Contains(t, err.Error(), "path separator")
}

// resolveTargetName exercises only the name validation in resolveTarget, which
// runs before any workspace is loaded.
func resolveTargetName(ctx context.Context, name string) (Target, string, error) {
	return resolveTarget(ctx, name, true)
}

func TestGenerateIsIdempotentAndForceRotates(t *testing.T) {
	ctx, workspace, _ := workspaceFixture(t)
	resetFlags(t)

	tgt, _, err := targetIn(ctx, workspace, "internal-auth", true)
	require.NoError(t, err)

	doc, err := Load(tgt)
	require.NoError(t, err)
	first, err := Generate(FormatHex, 32)
	require.NoError(t, err)
	doc.Set("TOKEN", first)
	require.NoError(t, Write(tgt, doc))

	// Re-running provisioning must keep the existing credential: another
	// service may already hold it.
	reloaded, err := Load(tgt)
	require.NoError(t, err)
	require.True(t, reloaded.Has("TOKEN"))
	stored, err := os.ReadFile(tgt.Path())
	require.NoError(t, err)
	require.Contains(t, string(stored), first)

	// --force is the explicit rotation path.
	second, err := Generate(FormatHex, 32)
	require.NoError(t, err)
	reloaded.Set("TOKEN", second)
	require.NoError(t, Write(tgt, reloaded))
	rotated, err := os.ReadFile(tgt.Path())
	require.NoError(t, err)
	require.Contains(t, string(rotated), second)
	require.NotContains(t, string(rotated), first)
}

func TestConfigurationNamesIgnoreSecretsWhenListingPlaintext(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"edge.env", "auth.secret.env", "notes.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("K=v\n"), 0o600))
	}

	plain, err := configurationNames(dir, ".env")
	require.NoError(t, err)
	require.Equal(t, []string{"edge"}, plain)

	secrets, err := configurationNames(dir, ".secret.env")
	require.NoError(t, err)
	require.Equal(t, []string{"auth"}, secrets)
}

func TestConfigurationNamesOnMissingDirectoryIsEmpty(t *testing.T) {
	names, err := configurationNames(filepath.Join(t.TempDir(), "absent"), ".secret.env")
	require.NoError(t, err)
	require.Empty(t, names)
}
