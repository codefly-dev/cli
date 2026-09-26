package environments_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

func managedEntry(externalName string) environments.EnvironmentManagedService {
	return environments.EnvironmentManagedService{Kind: "redis", ExternalName: externalName, Port: 6379}
}

// A key naming the service's module wins over a bare one, so an environment can
// manage one module's service and leave a same-named service of another module
// deploying as its module declares it.
func TestManagedServiceResolvesModuleQualifiedKeyFirst(t *testing.T) {
	env := &environments.Environment{
		ManagedServices: map[string]environments.EnvironmentManagedService{
			"sessions/redis": managedEntry("sessions.cache.example"),
			"redis":          managedEntry("shared.cache.example"),
		},
	}

	qualified, replaced := env.ManagedService("sessions", "redis")
	if !replaced || qualified.ExternalName != "sessions.cache.example" {
		t.Fatalf("ManagedService(sessions, redis) = %+v, %v", qualified, replaced)
	}

	// No entry for this module, so the bare one still answers.
	bare, replaced := env.ManagedService("catalog", "redis")
	if !replaced || bare.ExternalName != "shared.cache.example" {
		t.Fatalf("ManagedService(catalog, redis) = %+v, %v", bare, replaced)
	}

	if _, replaced := env.ManagedService("sessions", "accounts"); replaced {
		t.Error("a service nothing manages resolved to a managed replacement")
	}
}

// A qualified key manages only the module it names: the same service name in
// another module deploys as its module declares it.
func TestManagedServiceLeavesOtherModulesUnmanaged(t *testing.T) {
	env := &environments.Environment{
		ManagedServices: map[string]environments.EnvironmentManagedService{
			"sessions/redis": managedEntry("sessions.cache.example"),
		},
	}

	if _, replaced := env.ManagedService("catalog", "redis"); replaced {
		t.Error("catalog/redis was replaced by the entry qualifying sessions")
	}
}

func TestManagedServiceOnNilEnvironment(t *testing.T) {
	var env *environments.Environment
	if _, replaced := env.ManagedService("sessions", "redis"); replaced {
		t.Error("a nil environment reported a managed service")
	}
}

// writeTwoModuleWorkspace lays down a modules-layout workspace where both
// modules declare a service named "redis", the collision a bare managed-services
// key cannot resolve.
func writeTwoModuleWorkspace(t *testing.T, envDeclarations string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName: `name: platform
layout: modules
modules:
  - name: sessions
  - name: catalog
environments:
  - name: prod
    namespace: platform
` + envDeclarations,
	}
	for _, module := range []string{"sessions", "catalog"} {
		files[filepath.Join("modules", module, resources.ModuleConfigurationName)] = `kind: module
name: ` + module + `
services:
    - name: redis
`
		files[filepath.Join("modules", module, "services", "redis", resources.ServiceConfigurationName)] = `kind: service
name: redis
version: 0.0.0
agent:
  kind: runtime::service
  name: go-grpc
  version: 0.0.1
  publisher: codefly.ai
`
	}
	for rel, content := range files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func managedServicesDeclaration(key string) string {
	return `    managed-services:
      ` + key + `:
        kind: redis
        external-name: cache.internal.example
        port: 6379
`
}

// A bare key covers every same-named service, so both would render with this
// entry's address and secrets while only one was meant. The refusal has to name
// the candidates: the declaration is the only place that can say which.
func TestValidateEnvironmentsRejectsAmbiguousBareManagedService(t *testing.T) {
	ctx := context.Background()
	ws, err := loadDeploymentWorkspace(ctx, writeTwoModuleWorkspace(t, managedServicesDeclaration("redis")))
	if err != nil {
		t.Fatal(err)
	}

	err = ws.ValidateEnvironments(ctx)
	if err == nil {
		t.Fatal("expected ValidateEnvironments to reject the ambiguous bare managed service")
	}
	for _, want := range []string{"ambiguous", "sessions/redis", "catalog/redis"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// Qualifying the entry is the resolution the refusal asks for.
func TestValidateEnvironmentsAllowsQualifiedManagedService(t *testing.T) {
	ctx := context.Background()
	ws, err := loadDeploymentWorkspace(ctx, writeTwoModuleWorkspace(t, managedServicesDeclaration("sessions/redis")))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ValidateEnvironments(ctx); err != nil {
		t.Fatalf("ValidateEnvironments = %v, want nil", err)
	}
}

// A qualified key is new syntax with no installed base, so a typo in either
// component is refused rather than left inert — the service it should have
// replaced would otherwise deploy as its module declares it, with nothing
// reporting the entry went unused.
func TestValidateEnvironmentsRejectsUnknownQualifiedManagedService(t *testing.T) {
	for name, key := range map[string]string{
		"unknown module":  "sesions/redis",
		"unknown service": "sessions/redys",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ws, err := loadDeploymentWorkspace(ctx, writeTwoModuleWorkspace(t, managedServicesDeclaration(key)))
			if err != nil {
				t.Fatal(err)
			}
			err = ws.ValidateEnvironments(ctx)
			if err == nil {
				t.Fatalf("expected ValidateEnvironments to reject %q", key)
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "unknown service") {
				t.Fatalf("error = %v, want it to name the unknown service %q", err, key)
			}
		})
	}
}

// An imported coordinate contract declares a fleet's managed services, and a
// workspace composing none of them carries the entry unused. That stays inert
// rather than becoming a load failure.
func TestValidateEnvironmentsAllowsBareManagedServiceMatchingNoService(t *testing.T) {
	ctx := context.Background()
	ws, err := loadDeploymentWorkspace(ctx, writeTwoModuleWorkspace(t, managedServicesDeclaration("warehouse")))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ValidateEnvironments(ctx); err != nil {
		t.Fatalf("ValidateEnvironments = %v, want nil", err)
	}
}

// One module declaring the name means a bare key is unambiguous, the shape every
// workspace and imported contract already uses.
func TestValidateEnvironmentsAllowsUnambiguousBareManagedService(t *testing.T) {
	ctx := context.Background()
	ws, err := loadDeploymentWorkspace(ctx, writeServiceScopedWorkspace(t, managedServicesDeclaration("accounts")))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.ValidateEnvironments(ctx); err != nil {
		t.Fatalf("ValidateEnvironments = %v, want nil", err)
	}
}

// Key shape is validated wherever an environment is loaded, not only on the
// gitops paths that reach ValidateWorkspace: a run resolves managed services
// through the same keys and never reaches the graph cross-check.
func TestEnvironmentValidateRejectsMalformedManagedServiceKey(t *testing.T) {
	for _, key := range []string{"sessions/redis/primary", "/redis", "sessions/", ""} {
		env := &environments.Environment{
			Name:            "prod",
			ManagedServices: map[string]environments.EnvironmentManagedService{key: managedEntry("cache.example")},
		}
		if err := env.Validate(); err == nil {
			t.Errorf("Validate accepted managed service key %q", key)
		}
	}
}

func TestEnvironmentValidateAcceptsBothManagedServiceKeyShapes(t *testing.T) {
	env := &environments.Environment{
		Name: "prod",
		ManagedServices: map[string]environments.EnvironmentManagedService{
			"sessions/redis": managedEntry("sessions.cache.example"),
			"warehouse":      managedEntry("warehouse.example"),
		},
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}
