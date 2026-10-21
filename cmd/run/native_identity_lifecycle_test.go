package run

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/run/identity"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

type withdrawalFlow struct {
	fakeLauncherFlow
	address string
}

func (f *withdrawalFlow) GetAddressForEndpoint(context.Context, string, string, string) (string, error) {
	return f.address, nil
}

func TestWithdrawalCleanupFailureStillDeliversAndRetriesWithoutStoppedRoots(t *testing.T) {
	workspace, env := writeHostedNativeWorkspace(t)
	env.Host.Gateway = "model/model"
	issuer, err := identity.Start(env.Host.TrustDomain, env.Host.Audience, t.TempDir())
	require.NoError(t, err)
	signer, err := solutionhost.NewInMemorySigner("dev-0123456789abcdef", ed25519.NewKeyFromSeed(make([]byte, 32)))
	require.NoError(t, err)
	var mu sync.Mutex
	posted := map[string]*solutionhost.ModuleManifest{}
	refuse := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			signed, err := solutionhost.ParseSigned(body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			manifest, err := solutionhost.ManifestFromVerified(signed.Document)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			posted[manifest.Binding] = manifest
			if refuse {
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"disposition": "accepted", "generation": manifest.Generation})
			return
		}
		manifest := posted[filepath.Base(r.URL.Path)]
		if manifest == nil {
			w.WriteHeader(404)
			return
		}
		digest, err := manifest.Digest()
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": manifest.Binding, "generation": manifest.Generation, "digest": digest, "release": manifest.Release.Identity(), "removed": manifest.Removed})
	}))
	defer server.Close()
	cluster := &launcher{vouch: issuer, workspace: workspace, env: env, stateDir: t.TempDir(), signer: signer,
		flow: &withdrawalFlow{address: server.URL}, hostModule: "host", identities: map[string]gitops.NativeIdentity{},
		running: map[string]bool{}, delivered: map[string]*solutionhost.ModuleManifest{}, pendingWithdrawals: map[string]bool{}}
	paths := map[string]string{}
	for _, module := range []string{"first", "second"} {
		unique := module + "/api"
		cluster.identities[unique] = gitops.NativeIdentity{Module: module, Service: "api"}
		cluster.running[unique] = true
		cluster.delivered[module] = &solutionhost.ModuleManifest{Schema: solutionhost.SchemaModuleManifestV1, Binding: "acme.local." + module, Alias: module, Kind: solutionhost.KindModule, Generation: 1, Host: solutionhost.HostTarget{Coordinate: env.Host.Coordinate, Domain: env.Host.Domain}, Release: solutionhost.Release{Publisher: "acme", Name: module, Version: "1.0.0"}, Workloads: []solutionhost.Workload{{Service: "api", Container: "api"}}}
		issuer.DeclareModule(module, "api")
		for _, audience := range []string{env.Host.Audience, "second-audience"} {
			credential, err := issuer.ProjectForAudience(unique, issuer.Workload(module, "api"), audience)
			require.NoError(t, err)
			paths[unique+audience] = credential.File
		}
	}
	issuer.Declare(issuer.Observer())
	bad := paths["first/api"+env.Host.Audience]
	require.NoError(t, os.Remove(bad))
	require.NoError(t, os.Mkdir(bad, 0o700))
	blocker := filepath.Join(bad, "busy")
	require.NoError(t, os.WriteFile(blocker, []byte("fixture"), 0o600))
	_, err = cluster.withdraw(t.Context(), []string{"first/api", "second/api"})
	require.ErrorContains(t, err, "remove native credential")
	require.ErrorContains(t, err, "503")
	require.Len(t, cluster.pendingWithdrawals, 2)
	mu.Lock()
	require.Len(t, posted, 2, "file cleanup failure must not skip either host reconciliation")
	for _, manifest := range posted {
		require.True(t, manifest.Removed)
	}
	refuse = false
	mu.Unlock()
	require.NoError(t, issuer.Refresh())
	for _, path := range paths {
		if path != bad {
			require.NoFileExists(t, path)
		}
	}
	require.NoError(t, os.Remove(blocker))
	withdrawn, err := cluster.withdraw(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, []string{"first", "second"}, withdrawn)
	require.Empty(t, cluster.pendingWithdrawals)
	for _, path := range paths {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.FileExists(t, filepath.Join(cluster.stateDir, "delivery", "generations.json"), "credential cleanup preserves delivery state")
}

func TestStopLauncherJoinsRefreshRemovesAllAndReportsCleanupFailure(t *testing.T) {
	issuer, err := identity.Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	workload := issuer.Workload("example", "api")
	issuer.Declare(workload)
	var paths []string
	for _, audience := range []string{"host", "second"} {
		credential, err := issuer.ProjectForAudience("example/api", workload, audience)
		require.NoError(t, err)
		paths = append(paths, credential.File)
	}
	cluster := &launcher{vouch: issuer, stop: make(chan struct{})}
	cluster.refresh.Add(1)
	finished := make(chan struct{})
	go func() { defer cluster.refresh.Done(); defer close(finished); cluster.refreshTokens() }()
	require.NoError(t, os.Remove(paths[0]))
	require.NoError(t, os.Mkdir(paths[0], 0o700))
	blocker := filepath.Join(paths[0], "busy")
	require.NoError(t, os.WriteFile(blocker, []byte("fixture"), 0o600))
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() { launcherMu.Lock(); launcherActive = previous; launcherMu.Unlock() })
	require.ErrorContains(t, stopLauncher(), "remove native credential")
	select {
	case <-finished:
	default:
		t.Fatal("shutdown returned before refresh joined")
	}
	require.NoFileExists(t, paths[1])
	require.NoError(t, issuer.Refresh())
	require.NoFileExists(t, paths[1])
	_, err = issuer.Project("example/api", workload)
	require.ErrorContains(t, err, "closed")
	require.NoError(t, os.Remove(blocker))
	require.NoError(t, stopLauncher(), "failed shutdown cleanup remains retryable")
	require.Nil(t, launcherActive)
	_, err = os.Stat(paths[0])
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStoppedWorkloadIgnoresQueuedRunningHookAndKeepsDeclaration(t *testing.T) {
	cluster, flow, unique := launcherWithFakeFlow(t, func(string) bool { return true })
	issuer, err := identity.Start("example.test", "host", t.TempDir())
	require.NoError(t, err)
	cluster.vouch = issuer
	workload := issuer.Workload("model", "api")
	issuer.Declare(workload)
	credential, err := issuer.Project(unique, workload)
	require.NoError(t, err)
	cluster.running[unique] = true
	require.NoError(t, cluster.stopWorkload(unique))
	cluster.onRunning(t.Context(), unique)
	require.Empty(t, cluster.running)
	require.Zero(t, flow.attempts)
	_, declared := cluster.identity(unique)
	require.True(t, declared, "individual stop preserves the composition for restart")
	require.NoError(t, issuer.Refresh())
	require.NoFileExists(t, credential.File)
	_, err = issuer.Project(unique, workload)
	require.NoError(t, err)
	cluster.onRunning(t.Context(), unique)
	require.True(t, cluster.running[unique])
	require.Equal(t, 1, flow.attempts, "restart can deliver through the original lifecycle")
	require.NoError(t, cluster.close())
}
