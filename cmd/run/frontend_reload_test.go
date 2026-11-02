package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/cli/pkg/solutionrun"
	"github.com/codefly-dev/core/agents/manager"
	corecomposition "github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func frontendPage(t *testing.T, cluster *launcher) string {
	t.Helper()
	dir := filepath.Join(cluster.composed().Dir(), "modules", "alpha")
	moduleFile := filepath.Join(dir, resources.ModuleConfigurationName)
	body, err := os.ReadFile(moduleFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(moduleFile, append([]byte("service-entry: api\n"), body...), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "services/api", solutionrun.FrontendFileName), []byte("title: Alpha\norder: 1\nexposed_module: ./Page\nhost_contract: 1\n"), 0600))
	remote := filepath.Join(dir, "services/fe-remote")
	require.NoError(t, os.MkdirAll(remote, 0755))
	failed, _ := json.Marshal(filepath.Join(cluster.stateDir, "frontend-failed"))
	for name, content := range map[string]string{
		"package.json": `{"private":true,"scripts":{"build":"node build.cjs"}}`,
		"Page.tsx":     "before",
		"build.cjs":    `const fs=require('fs');const text=fs.readFileSync('Page.tsx','utf8');if(text==='broken'){fs.writeFileSync(` + string(failed) + `,'failed');process.exit(1);}fs.mkdirSync('dist',{recursive:true});fs.writeFileSync('dist/mf-manifest.json',JSON.stringify({text}));`,
	} {
		require.NoError(t, os.WriteFile(filepath.Join(remote, name), []byte(content), 0600))
	}
	t.Setenv("npm_config_cache", t.TempDir())
	t.Setenv("npm_config_offline", "true")
	t.Setenv("npm_config_audit", "false")
	t.Setenv("npm_config_fund", "false")
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), cluster.composed().Dir())
	require.NoError(t, err)
	cluster.workspace = workspace
	return filepath.Join(remote, "Page.tsx")
}

func reloadPage(t *testing.T, cluster *launcher) string {
	t.Helper()
	file := frontendPage(t, cluster)
	cluster.watchFrontends = true
	cluster.schedule.Deadline = 15 * time.Second
	require.NoError(t, cluster.refreshFrontendWatches(t.Context()))
	return file
}

func latestAlpha(peer *admissionPeer) *solutionhost.ModuleManifest {
	var latest *solutionhost.ModuleManifest
	for _, m := range peer.snapshots() {
		if m.Alias == "alpha" {
			latest = m
		}
	}
	return latest
}

func TestFrontendEditBuildsAndDeliversWithoutRuntimeStart(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	file := reloadPage(t, cluster)
	require.NoError(t, cluster.admitSelected(t.Context()))
	first := latestAlpha(peer)
	require.NotNil(t, first.Frontend.Payload)
	require.NoError(t, os.WriteFile(file, []byte("after"), 0600))
	require.Eventually(t, func() bool {
		m := latestAlpha(peer)
		return m.Generation > first.Generation && m.Frontend.Payload.Digest != first.Frontend.Payload.Digest
	}, 8*time.Second, 20*time.Millisecond)
	cluster.mu.Lock()
	require.Empty(t, cluster.running, "a frontend edit cannot fabricate a Start")
	cluster.mu.Unlock()
	// The signed payload identifies a real retained archive, not a source-only claim.
	last := latestAlpha(peer)
	digest := string(last.Frontend.Payload.Digest)
	require.FileExists(t, filepath.Join(cluster.composed().Dir(), ".codefly/selection-artifacts", digest[len("sha256:"):]))
	archive, err := os.ReadFile(filepath.Join(cluster.composed().Dir(), ".codefly/selection-artifacts", digest[len("sha256:"):]))
	require.NoError(t, err)
	extracted := t.TempDir()
	require.NoError(t, corecomposition.ExtractArchive(t.Context(), archive, extracted))
	payload, err := os.ReadFile(filepath.Join(extracted, "mf-manifest.json"))
	require.NoError(t, err)
	var document struct{ Text string }
	require.NoError(t, json.Unmarshal(payload, &document))
	require.Equal(t, "after", document.Text)
	require.Eventually(t, func() bool {
		cluster.mu.Lock()
		defer cluster.mu.Unlock()
		return !cluster.laneLocked("alpha").scheduled
	}, time.Second, 10*time.Millisecond)
	prior := len(peer.snapshots())
	require.NoError(t, os.MkdirAll(filepath.Join(filepath.Dir(file), "dist"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(file), "dist/mf-manifest.json"), []byte("output only"), 0600))
	time.Sleep(900 * time.Millisecond)
	require.Len(t, peer.snapshots(), prior, "unconditional build output must cause no delivery")
}

func TestPendingFrontendAdmissionRepairsAndTerminalDoesNotRestart(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			cluster, peer := admissionLauncher(t)
			file := reloadPage(t, cluster)
			require.NoError(t, os.WriteFile(file, []byte("broken"), 0600))
			if terminal {
				cluster.schedule.Deadline = 300 * time.Millisecond
			}
			service, err := cluster.composed().LoadService(t.Context(), &resources.ServiceWithModule{Module: "alpha", Name: "api"})
			require.NoError(t, err)
			attempt, err := cluster.beginRuntimeStart(t.Context(), service)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { done <- attempt.Admit(t.Context()) }()
			if terminal {
				require.Error(t, <-done)
				attempt.Complete(false)
				require.NoError(t, os.WriteFile(file, []byte("repaired"), 0600))
				time.Sleep(900 * time.Millisecond)
				require.Empty(t, peer.snapshots(), "repair after terminal admission cannot mint another Start/Admit")
			} else {
				// Observe failure in the actual npm build before repairing its source.
				require.Eventually(t, func() bool { _, err := os.Stat(filepath.Join(cluster.stateDir, "frontend-failed")); return err == nil }, 5*time.Second, 10*time.Millisecond)
				require.NoError(t, os.WriteFile(file, []byte("repaired"), 0600))
				require.NoError(t, <-done)
				require.NotEmpty(t, peer.snapshots())
				attempt.Complete(true)
			}
		})
	}
}

func TestBatchRechecksAfterPriorSendAndLostResponseReplays(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	var calls atomic.Int32
	peer.intercept = func(_ context.Context, req *compositionv1.ApplyManifestRequest, m *solutionhost.ModuleManifest) error {
		require.NotNil(t, req.Carrier.Envelope, "real delivery signs the carrier")
		if calls.Add(1) == 1 {
			cluster.mu.Lock()
			cluster.invalidateLocked("beta")
			cluster.mu.Unlock()
			return status.Error(codes.Unavailable, "response lost after host applied")
		}
		return nil
	}
	require.NoError(t, testDelivery(hostAlwaysReady, cluster.admitSelected).run(t.Context()))
	sent := peer.snapshots()
	require.GreaterOrEqual(t, len(sent), 3)
	require.Equal(t, "alpha", sent[0].Alias)
	require.Equal(t, "alpha", sent[1].Alias)
	require.Equal(t, sent[0].Generation, sent[1].Generation, "lost response replays its reservation")
	require.Equal(t, "beta", sent[len(sent)-1].Alias)
}

func TestAppliedNoProgressRetriesWithinOriginalBound(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	require.NoError(t, cluster.admitSelected(t.Context()))
	manifest := latestAlpha(peer)
	synctest.Test(t, func(t *testing.T) {
		var observed, attempts int
		d := &gitops.NativeDeliverer{Applied: func(context.Context, string) (*gitops.AppliedManifest, bool, error) {
			observed++
			if attempts == 1 {
				return nil, false, nil
			}
			digest, err := manifest.Digest()
			return &gitops.AppliedManifest{Binding: manifest.Binding, Generation: manifest.Generation, Digest: digest, Release: manifest.Release.Identity()}, true, err
		}}
		delivery := testDelivery(hostAlwaysReady, func(ctx context.Context) error { attempts++; return d.WaitForApplied(ctx, manifest) })
		delivery.schedule.Deadline = 2 * time.Minute
		require.NoError(t, delivery.run(t.Context()))
		require.Equal(t, 2, attempts)
		require.Greater(t, observed, 40)
	})
	for _, code := range []codes.Code{codes.Unavailable, codes.PermissionDenied, codes.Canceled} {
		t.Run(code.String(), func(t *testing.T) {
			attempts := 0
			d := &gitops.NativeDeliverer{Applied: func(context.Context, string) (*gitops.AppliedManifest, bool, error) {
				if attempts == 1 {
					return nil, false, status.Error(code, "observation")
				}
				digest, err := manifest.Digest()
				return &gitops.AppliedManifest{Binding: manifest.Binding, Generation: manifest.Generation, Digest: digest, Release: manifest.Release.Identity()}, true, err
			}}
			err := testDelivery(hostAlwaysReady, func(ctx context.Context) error {
				attempts++
				return d.WaitForApplied(ctx, manifest)
			}).run(t.Context())
			if code == codes.Unavailable {
				require.NoError(t, err)
				require.Equal(t, 2, attempts)
			} else {
				require.Error(t, err)
				require.Equal(t, 1, attempts)
			}
		})
	}
}

func TestAppliedObservationCannotRenewCallerDeadline(t *testing.T) {
	cluster, peer := admissionLauncher(t)
	require.NoError(t, cluster.admitSelected(t.Context()))
	manifest := latestAlpha(peer)
	synctest.Test(t, func(t *testing.T) {
		attempts := 0
		d := &gitops.NativeDeliverer{Applied: func(context.Context, string) (*gitops.AppliedManifest, bool, error) { return nil, false, nil }}
		delivery := testDelivery(hostAlwaysReady, func(ctx context.Context) error {
			attempts++
			return d.WaitForApplied(ctx, manifest)
		})
		delivery.schedule.Deadline = 10 * time.Second
		started := time.Now()
		err := delivery.run(t.Context())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 1, attempts)
		require.Equal(t, 10*time.Second, time.Since(started))
		var uncertain *gitops.AppliedUnconfirmed
		require.False(t, errors.As(err, &uncertain), "caller expiry is terminal, not an internal observation bound")
	})
}

// The real flow publishes its authority participants during manager initialization.
func TestInitialRunInstallsFrontendWatcherAfterParticipantsPublish(t *testing.T) {
	cluster, _ := admissionLauncher(t)
	file := frontendPage(t, cluster)
	workspace := cluster.composed()
	module, err := workspace.LoadModuleFromName(t.Context(), "alpha")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, cluster.env, orchestration.RunMode)
	require.NoError(t, err)
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithStandAlone(true)
	flow.WithExcludeRoot(true)
	require.Empty(t, flow.LocalAuthorityServices())
	cluster.flow = flow
	cluster.watchFrontends = true
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() {
		launcherMu.Lock()
		launcherActive = previous
		launcherMu.Unlock()
		_ = flow.Stop()
		_ = flow.Shutdown()
	})
	require.NoError(t, initializeRunManagers(t.Context(), flow))
	require.Contains(t, flow.LocalAuthorityServices(), "alpha/api")
	cluster.mu.Lock()
	lane := cluster.laneLocked("alpha")
	watching, before := lane.watch != nil, lane.revision
	cluster.mu.Unlock()
	require.True(t, watching, "selected frontend must already be watched before Load or Start")
	require.NoError(t, os.WriteFile(file, []byte("after"), 0600))
	require.Eventually(t, func() bool { cluster.mu.Lock(); defer cluster.mu.Unlock(); return lane.revision > before }, 5*time.Second, 20*time.Millisecond)
}

// A normal native origin exercises InitManagers with a real CLI protocol peer.
func TestInitialNativeRunWatchesSelectedFrontend(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv("CODEFLY_TEST_PEER_ROOT", t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	binary := filepath.Join(t.TempDir(), "protocol-peer")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./testdata/peer")
	build.Dir = "../../pkg/internal/protocoltest"
	build.Env = append(os.Environ(), "GOWORK=off")
	output, err := build.CombinedOutput()
	require.NoError(t, err, "%s", output)
	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "frontend-startup", Version: "0.0.1"}
	path, err := agent.Path(t.Context())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, os.Symlink(binary, path))
	cluster, _ := admissionLauncher(t)
	file := frontendPage(t, cluster)
	serviceFile := filepath.Join(cluster.composed().Dir(), "modules", "alpha", "services", "api", resources.ServiceConfigurationName)
	declaration := "kind: service\nname: api\nversion: 0.0.0\nmodule-identity: true\nagent:\n  kind: codefly:service\n  publisher: example.test\n  name: frontend-startup\n  version: 0.0.1\nendpoints:\n  - name: http\n    api: http\n    visibility: internal\n"
	require.NoError(t, os.WriteFile(serviceFile, []byte(declaration), 0600))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), cluster.composed().Dir())
	require.NoError(t, err)
	cluster.workspace = workspace
	module, err := workspace.LoadModuleFromName(t.Context(), "alpha")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, cluster.env, orchestration.RunMode)
	require.NoError(t, err)
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithStandAlone(true)
	flow.WithTemporaryPorts(true)
	require.Empty(t, flow.LocalAuthorityServices())
	cluster.flow = flow
	cluster.watchFrontends = true
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() {
		launcherMu.Lock()
		launcherActive = previous
		launcherMu.Unlock()
		_ = flow.Stop()
		_ = flow.Shutdown()
		for _, key := range flow.AgentCacheKeys() {
			coreservices.ClearAgent(key)
		}
	})
	require.NoError(t, initializeRunManagers(t.Context(), flow))
	require.Contains(t, flow.LocalAuthorityServices(), "alpha/api")
	cluster.mu.Lock()
	lane := cluster.laneLocked("alpha")
	watching, before := lane.watch != nil, lane.revision
	cluster.mu.Unlock()
	require.True(t, watching, "ordinary selected native service has a frontend watch before Load")
	require.NoError(t, os.WriteFile(file, []byte("after"), 0600))
	require.Eventually(t, func() bool { cluster.mu.Lock(); defer cluster.mu.Unlock(); return lane.revision > before }, 5*time.Second, 20*time.Millisecond)
}

func TestInitialFrontendWatchOptOutAndSetupFailure(t *testing.T) {
	for _, tc := range []struct {
		name               string
		enabled, malformed bool
	}{
		{name: "disabled"}, {name: "invalid declaration", enabled: true, malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cluster, _ := admissionLauncher(t)
			frontendPage(t, cluster)
			if tc.malformed {
				p := filepath.Join(cluster.composed().Dir(), "modules", "alpha", "services", "api", solutionrun.FrontendFileName)
				require.NoError(t, os.WriteFile(p, []byte("title: [invalid\n"), 0600))
			}
			workspace := cluster.composed()
			module, err := workspace.LoadModuleFromName(t.Context(), "alpha")
			require.NoError(t, err)
			service, err := module.LoadServiceFromName(t.Context(), "api")
			require.NoError(t, err)
			flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, cluster.env, orchestration.RunMode)
			require.NoError(t, err)
			flow.WithRuntimeContext(resources.RuntimeContextNative)
			flow.WithStandAlone(true)
			flow.WithExcludeRoot(true)
			cluster.flow = flow
			cluster.watchFrontends = tc.enabled
			launcherMu.Lock()
			previous := launcherActive
			launcherActive = cluster
			launcherMu.Unlock()
			t.Cleanup(func() {
				launcherMu.Lock()
				launcherActive = previous
				launcherMu.Unlock()
				_ = flow.Stop()
				_ = flow.Shutdown()
			})
			err = initializeRunManagers(t.Context(), flow)
			if tc.malformed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			cluster.mu.Lock()
			watching := cluster.laneLocked("alpha").watch != nil
			cluster.mu.Unlock()
			require.False(t, watching)
			cluster.close() // cleanup remains safe after both disabled and failed setup
		})
	}
}

func TestInitialFrontendWatchDoesNotStartWhenManagerInitializationFails(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	t.Setenv(manager.AgentSourceEnv, "local")
	cluster, _ := admissionLauncher(t)
	frontendPage(t, cluster)
	workspace := cluster.composed()
	module, err := workspace.LoadModuleFromName(t.Context(), "alpha")
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(t.Context(), "api")
	require.NoError(t, err)
	flow, err := orchestration.NewFlow(t.Context(), workspace, module, service, cluster.env, orchestration.RunMode)
	require.NoError(t, err)
	flow.WithRuntimeContext(resources.RuntimeContextNative)
	flow.WithStandAlone(true)
	cluster.flow = flow
	cluster.watchFrontends = true
	launcherMu.Lock()
	previous := launcherActive
	launcherActive = cluster
	launcherMu.Unlock()
	t.Cleanup(func() {
		launcherMu.Lock()
		launcherActive = previous
		launcherMu.Unlock()
		_ = flow.Stop()
		_ = flow.Shutdown()
	})
	require.Error(t, initializeRunManagers(t.Context(), flow), "the deliberately absent agent must fail initialization")
	cluster.mu.Lock()
	watching := cluster.laneLocked("alpha").watch != nil
	cluster.mu.Unlock()
	require.False(t, watching, "failed manager initialization must not install watchers")
	cluster.close()
}
