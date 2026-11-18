package update

import (
	"bytes"
	"context"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFreshnessNeverDowngradesOrMovesDevelopmentPins(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            bool
	}{
		{"1.0.0", "1.0.1", true}, {"1.0.1", "1.0.1", false},
		{"2.0.0", "1.0.1", false}, {"1.0.0-dev.abc", "1.0.1", false},
		{"1.0.0", "1.0.1-beta.1", false},
	} {
		got, err := newerRelease(tc.current, tc.latest)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "%s -> %s", tc.current, tc.latest)
	}
	_, err := newerRelease("invalid", "1.0.0")
	require.Error(t, err)
}

func TestFreshnessDeploymentCannotMutateSelections(t *testing.T) {
	require.Error(t, ValidatePolicy("auto", true))
	require.NoError(t, ValidatePolicy("auto", false))
	for _, policy := range []string{"check", "require-current", "off"} {
		require.NoError(t, ValidatePolicy(policy, true))
	}
	require.Error(t, ValidatePolicy("latest", false))
}

func TestFreshnessPoliciesPreserveOrUpdateRealManifests(t *testing.T) {
	previous := resolveLatestAgent
	t.Cleanup(func() { resolveLatestAgent = previous })
	resolveLatestAgent = func(_ context.Context, a *resources.Agent) (string, error) {
		a.Version = "99.0.0"
		return "github", nil
	}
	fakeInspection(t)
	for _, policy := range []string{"check", "require-current", "auto"} {
		t.Run(policy, func(t *testing.T) {
			dir := writePinWorkspace(t, "")
			workspace, err := resources.LoadWorkspaceFromDir(t.Context(), dir)
			require.NoError(t, err)
			file := filepath.Join(dir, "modules/acme/services/accounts/service.codefly.yaml")
			before, err := os.ReadFile(file)
			require.NoError(t, err)
			var report bytes.Buffer
			err = Preflight(t.Context(), workspace, policy, &report)
			if policy == "require-current" {
				require.ErrorContains(t, err, "outdated")
			} else {
				require.NoError(t, err)
			}
			after, err := os.ReadFile(file)
			require.NoError(t, err)
			if policy == "auto" {
				require.Contains(t, string(after), "version: 99.0.0")
			} else {
				require.Equal(t, before, after)
			}
			require.Contains(t, report.String(), "Agent update available")
		})
	}
}

func TestFreshnessRejectsLocalFallbackAndFailedAdmission(t *testing.T) {
	previous := resolveLatestAgent
	t.Cleanup(func() { resolveLatestAgent = previous })
	for _, source := range []string{"local", "github"} {
		t.Run(source, func(t *testing.T) {
			resolveLatestAgent = func(_ context.Context, a *resources.Agent) (string, error) { a.Version = "99.0.0"; return source, nil }
			fakeInspection(t, "99.0.0")
			dir := writePinWorkspace(t, "")
			workspace, err := resources.LoadWorkspaceFromDir(t.Context(), dir)
			require.NoError(t, err)
			file := filepath.Join(dir, "modules/acme/services/accounts/service.codefly.yaml")
			before, err := os.ReadFile(file)
			require.NoError(t, err)
			err = Preflight(t.Context(), workspace, "auto", io.Discard)
			require.Error(t, err)
			after, err := os.ReadFile(file)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestFreshnessUpdatesExistingOverrideWithoutEditingModule(t *testing.T) {
	previous := resolveLatestAgent
	t.Cleanup(func() { resolveLatestAgent = previous })
	resolveLatestAgent = func(_ context.Context, a *resources.Agent) (string, error) {
		a.Version = "99.0.0"
		return "github", nil
	}
	fakeInspection(t)
	dir := writePinWorkspace(t, "agent-overrides:\n  codefly.dev/go-grpc: 0.1.47\n")
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), dir)
	require.NoError(t, err)
	file := filepath.Join(dir, "modules/acme/services/accounts/service.codefly.yaml")
	before, err := os.ReadFile(file)
	require.NoError(t, err)
	require.NoError(t, Preflight(t.Context(), workspace, "auto", io.Discard))
	after, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, before, after)
	reloaded, err := resources.LoadWorkspaceFromDir(t.Context(), dir)
	require.NoError(t, err)
	overrides, err := reloaded.AgentOverrides()
	require.NoError(t, err)
	require.Equal(t, "99.0.0", overrides[0].Version)
}

func TestFreshnessOffDoesNotLoadWorkspace(t *testing.T) {
	require.NoError(t, Preflight(t.Context(), nil, "off", io.Discard))
	require.Error(t, Preflight(t.Context(), nil, "typo", io.Discard))
}
