package run

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// Over the legacy HTTP delivery, a withdrawal the saas host refuses because it
// already holds one for the binding (a module re-added, refused, then dropped
// again) completes, rather than staying pending on every refresh.
func TestAWithdrawalTheHTTPHostAlreadyHoldsCompletes(t *testing.T) {
	cluster, flow, _ := launcherWithFakeFlow(t, func(string) bool { return true })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "module manifest generation conflicts with this binding's history: " +
			solutionhost.ErrWithdrawn.Error() + `: binding "acme.local.model" was withdrawn at generation 2 and the candidate is generation 4`})
	}))
	t.Cleanup(server.Close)
	flow.address = server.URL
	cluster.env = &environments.Environment{Name: "local", Host: &environments.EnvironmentHost{Gateway: "saas/auth-gateway", Delivery: "saas/accounts/rest", TrustDomain: "cluster.local", Audience: "accounts"}}
	cluster.stateDir = t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cluster.signer, err = solutionhost.NewInMemorySigner("test", key)
	require.NoError(t, err)
	cluster.vouch, err = identity.Start("cluster.local", "accounts", t.TempDir())
	require.NoError(t, err)
	cluster.declare(map[string]gitops.NativeIdentity{"model/api": {Module: "model", Service: "api"}})
	cluster.pendingWithdrawals = map[string]bool{}
	cluster.delivered["model"] = &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "acme.local.model", Alias: "model", Kind: solutionhost.KindModule, Generation: 3,
		Host:      solutionhost.HostTarget{Coordinate: "example/local/dev", Domain: "acme"},
		Release:   solutionhost.Release{Publisher: "acme", Name: "model", Version: "0.1.0"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "api"}},
		Authority: &solutionhost.Authority{Principal: "model"},
	}
	done, err := cluster.withdrawModule(context.Background(), "model")
	require.NoError(t, err)
	require.True(t, done)
	require.Empty(t, cluster.pendingWithdrawals, "nothing stays pending")
	require.True(t, cluster.delivered["model"].Removed)
}
