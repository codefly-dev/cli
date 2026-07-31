package cmd

import (
	"testing"

	hostprovider "github.com/codefly-dev/cli/pkg/provider"
)

func workspaceWithBindings(t *testing.T, bindings string) string {
	t.Helper()
	return singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, map[string]string{
		hostprovider.BindingsFileName: bindings,
	})
}

func TestDoctorWorkspace_NoBindingsFileIsClean(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, nil)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireNoCode(t, report, hostprovider.CodeBindingsUnreadable)
	requireNoCode(t, report, hostprovider.CodeSecretInSpec)
	if report.Status != readinessStatusReady {
		t.Fatalf("workspace without bindings must be ready, got %q", report.Status)
	}
}

func TestDoctorWorkspace_ValidBindingReported(t *testing.T) {
	dir := workspaceWithBindings(t, `schema: codefly.provider-bindings/v0
environments:
  local:
    billing:
      provider: acme/stripe:1.2.3
      mode: managed
`)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireNoCode(t, report, hostprovider.CodeSecretInSpec)
	requireNoCode(t, report, hostprovider.CodeInvalidIdentity)

	var found bool
	for _, check := range report.Checks {
		if check.Name == "provider binding billing" && check.Status == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an ok check for the billing binding, got %s", reportJSON(t, report))
	}
	if report.Status != readinessStatusReady {
		t.Fatalf("a valid binding must leave the workspace ready, got %q", report.Status)
	}
}

func TestDoctorWorkspace_SecretInSpecFailsClosed(t *testing.T) {
	dir := workspaceWithBindings(t, `schema: codefly.provider-bindings/v0
environments:
  local:
    billing:
      provider: acme/stripe:1.2.3
      mode: managed
      spec:
        api_key: client_secret=totally-fake-test-value
`)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, hostprovider.CodeSecretInSpec, "fail")
	if report.Status != readinessStatusNotReady {
		t.Fatalf("a secret in spec must make the workspace not ready, got %q", report.Status)
	}
}

func TestDoctorWorkspace_UnknownSchemaFailsClosed(t *testing.T) {
	dir := workspaceWithBindings(t, "schema: codefly.provider-bindings/v99\nenvironments: {}\n")
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, hostprovider.CodeBindingsSchemaUnknown, "fail")
}
