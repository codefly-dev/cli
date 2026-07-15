// Package dockerstart auto-starts a local Docker engine (OrbStack, Docker
// Desktop, colima, …) when a run needs Docker but the daemon is not reachable.
// It is best-effort and opt-outable: the CLI calls EnsureRunning only when a
// service can run under no backend other than Docker.
package dockerstart

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/codefly-dev/core/wool"
)

const (
	// readyTimeout bounds how long we wait for the engine to become reachable
	// after launching a provider (Docker Desktop can take ~30s cold).
	readyTimeout = 120 * time.Second
	// pollInterval is how often we re-check reachability while waiting.
	pollInterval = 2 * time.Second
)

// provider is a Docker engine we know how to detect and start on this OS.
type provider struct {
	name         string
	contextHints []string // docker context names that indicate this provider is active
	detect       func() bool
	start        func(context.Context) error
}

func onPath(bin string) func() bool {
	return func() bool { _, err := exec.LookPath(bin); return err == nil }
}

func appExists(app string) func() bool {
	return func() bool {
		_, err := os.Stat(filepath.Join("/Applications", app))
		return err == nil
	}
}

func openApp(app string) func(context.Context) error {
	return func(ctx context.Context) error {
		// -g: don't steal focus, -a: application by name.
		return exec.CommandContext(ctx, "open", "-ga", app).Run()
	}
}

func runCmd(name string, args ...string) func(context.Context) error {
	return func(ctx context.Context) error {
		return exec.CommandContext(ctx, name, args...).Run()
	}
}

// providers returns the known providers for the current OS, in preference order.
func providers() []provider {
	switch runtime.GOOS {
	case "darwin":
		return []provider{
			{
				name:         "OrbStack",
				contextHints: []string{"orbstack"},
				detect:       func() bool { return onPath("orb")() || appExists("OrbStack.app")() },
				start: func(ctx context.Context) error {
					if _, err := exec.LookPath("orb"); err == nil {
						return exec.CommandContext(ctx, "orb", "start").Run()
					}
					return openApp("OrbStack")(ctx)
				},
			},
			{
				name:         "Docker Desktop",
				contextHints: []string{"desktop-linux", "default"},
				detect:       appExists("Docker.app"),
				start:        openApp("Docker"),
			},
			{
				name:         "colima",
				contextHints: []string{"colima"},
				detect:       onPath("colima"),
				start:        runCmd("colima", "start"),
			},
			{
				name:         "Rancher Desktop",
				contextHints: []string{"rancher-desktop"},
				detect:       appExists("Rancher Desktop.app"),
				start:        openApp("Rancher Desktop"),
			},
			{
				name:         "Podman",
				contextHints: []string{"podman", "podman-machine-default"},
				detect:       onPath("podman"),
				start:        runCmd("podman", "machine", "start"),
			},
		}
	case "linux":
		// No sudo/systemd assumptions: only rootless engines we can start as the user.
		return []provider{
			{
				name:         "colima",
				contextHints: []string{"colima"},
				detect:       onPath("colima"),
				start:        runCmd("colima", "start"),
			},
			{
				name:         "Podman",
				contextHints: []string{"podman", "podman-machine-default"},
				detect:       onPath("podman"),
				start:        runCmd("podman", "machine", "start"),
			},
		}
	default:
		return nil
	}
}

// selectProvider picks the provider to start: the one matching the active docker
// context if it is installed (that is what the user configured), otherwise the
// first installed provider in preference order. Returns nil if none is installed.
func selectProvider(activeContext string) *provider {
	return selectFrom(providers(), activeContext)
}

// selectFrom is the pure selection logic over a given provider list: an active
// context that names an installed provider wins, else the first installed one.
func selectFrom(provs []provider, activeContext string) *provider {
	if activeContext != "" {
		for i := range provs {
			if !provs[i].detect() {
				continue
			}
			for _, hint := range provs[i].contextHints {
				if strings.EqualFold(hint, activeContext) {
					return &provs[i]
				}
			}
		}
	}
	for i := range provs {
		if provs[i].detect() {
			return &provs[i]
		}
	}
	return nil
}

// EnsureRunning brings the Docker engine up if it is not already reachable.
// It returns (started, providerName, err):
//   - started=true  → we launched a provider and the engine became reachable;
//   - started=false, err=nil → the engine was already running (no-op);
//   - err!=nil → no provider is installed, launching failed, or it did not become
//     ready within the timeout. Callers treat this as best-effort and fall back.
func EnsureRunning(ctx context.Context, activeContext string) (started bool, providerName string, err error) {
	w := wool.Get(ctx).In("dockerstart.EnsureRunning")
	if dockerrun.DockerEngineRunning(ctx) {
		return false, "", nil
	}
	p := selectProvider(activeContext)
	if p == nil {
		return false, "", w.NewError("no known Docker provider (OrbStack, Docker Desktop, colima, Rancher Desktop, Podman) is installed to auto-start")
	}
	w.Info(fmt.Sprintf("Docker engine not reachable — starting %s…", p.name))
	if serr := p.start(ctx); serr != nil {
		return false, p.name, w.Wrapf(serr, "failed to launch %s", p.name)
	}

	timeout := time.After(readyTimeout)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, p.name, ctx.Err()
		case <-timeout:
			return false, p.name, w.NewError("%s did not become ready within %s", p.name, readyTimeout)
		case <-tick.C:
			if dockerrun.DockerEngineRunning(ctx) {
				w.Info(fmt.Sprintf("Docker engine is up (%s).", p.name))
				return true, p.name, nil
			}
		}
	}
}
