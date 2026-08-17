package orchestration

import (
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestOutputEnvironmentDefaultsToOriginServiceOnly(t *testing.T) {
	origin := outputEnvironmentTestService("saas", "frontend")
	dependency := outputEnvironmentTestService("platform", "warden")
	flow := &Flow{originService: origin}
	flow.WithOutputEnv(filepath.Join(t.TempDir(), "runtime.env"))

	if !flow.exportsRuntimeEnvironmentFor(origin) {
		t.Fatal("origin service should own the default runtime environment export")
	}
	if flow.exportsRuntimeEnvironmentFor(dependency) {
		t.Fatal("dependency must not overwrite the origin runtime environment export")
	}
}

func TestOutputEnvironmentCanSelectOneDependency(t *testing.T) {
	origin := outputEnvironmentTestService("saas", "frontend")
	dependency := outputEnvironmentTestService("platform", "warden")
	flow := &Flow{originService: origin}
	flow.WithOutputEnv(filepath.Join(t.TempDir(), "runtime.env"))
	flow.WithOutputEnvService("platform/warden")

	if flow.exportsRuntimeEnvironmentFor(origin) {
		t.Fatal("origin must not overwrite an explicitly selected dependency export")
	}
	if !flow.exportsRuntimeEnvironmentFor(dependency) {
		t.Fatal("selected dependency should own the runtime environment export")
	}
}

func TestEmptyOutputEnvironmentDisablesEveryExport(t *testing.T) {
	origin := outputEnvironmentTestService("saas", "frontend")
	flow := &Flow{originService: origin}
	flow.WithOutputEnv("")
	flow.WithOutputEnvService("saas/frontend")

	if flow.exportsRuntimeEnvironmentFor(origin) {
		t.Fatal("empty output path must disable runtime environment export")
	}
}

func TestExcludedRootOwnsDefaultOutputEnvironmentWithoutRunning(t *testing.T) {
	origin := outputEnvironmentTestService("saas", "frontend")
	flow := &Flow{originService: origin}
	flow.WithOutputEnv(filepath.Join(t.TempDir(), "runtime.env"))
	flow.WithExcludeRoot(true)

	if !flow.exportsExcludedOriginEnvironment() {
		t.Fatal("excluded root should compose the default output environment")
	}

	dependency := outputEnvironmentTestService("platform", "warden")
	flow.WithOutputEnvService("platform/warden")
	if flow.exportsExcludedOriginEnvironment() {
		t.Fatal("excluded root must not compose an explicitly selected dependency environment")
	}
	if !flow.exportsRuntimeEnvironmentFor(dependency) {
		t.Fatal("selected dependency should remain the output environment owner")
	}
}

func outputEnvironmentTestService(module, name string) *resources.Service {
	service := &resources.Service{Name: name}
	service.WithModule(module)
	return service
}
