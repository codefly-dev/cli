package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testWorkspaceYAML = `name: demo
layout: modules
modules:
    - name: backend
`

const testWorkspaceYAMLLocalDeclared = testWorkspaceYAML + `environments:
    - name: local
`

const testWorkspaceYAMLOnePassword = testWorkspaceYAML + `environments:
    - name: local
      secrets:
          - kind: 1password
`

func testServiceYAML(name string, workspaceDeps ...string) string {
	yaml := fmt.Sprintf(`kind: service
name: %s
version: 0.0.0
module: backend
agent:
    kind: runtime::service
    name: go-grpc
    version: 0.0.16
    publisher: codefly.ai
`, name)
	if len(workspaceDeps) > 0 {
		yaml += "workspace-configuration-dependencies:\n"
		for _, dep := range workspaceDeps {
			yaml += "    - " + dep + "\n"
		}
	}
	return yaml
}

func testModuleYAML(services ...string) string {
	yaml := "kind: module\nname: backend\nservices:\n"
	for _, svc := range services {
		yaml += "    - name: " + svc + "\n"
	}
	return yaml
}

func writeTestWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func singleServiceWorkspace(t *testing.T, workspaceYAML string, workspaceDeps []string, extra map[string]string) string {
	t.Helper()
	files := map[string]string{
		"workspace.codefly.yaml":                            workspaceYAML,
		"modules/backend/module.codefly.yaml":               testModuleYAML("api"),
		"modules/backend/services/api/service.codefly.yaml": testServiceYAML("api", workspaceDeps...),
	}
	maps.Copy(files, extra)
	return writeTestWorkspace(t, files)
}

func runReadiness(t *testing.T, opts workspaceReadinessOptions) *workspaceReadinessReport {
	t.Helper()
	if opts.env == "" {
		opts.env = "local"
	}
	if opts.timeout == 0 {
		opts.timeout = 30 * time.Second
	}
	return workspaceReadiness(context.Background(), opts)
}

func findDiagnostics(report *workspaceReadinessReport, code string) []workspaceDiagnostic {
	var out []workspaceDiagnostic
	for _, d := range report.Checks {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

func requireCode(t *testing.T, report *workspaceReadinessReport, code, status string) workspaceDiagnostic {
	t.Helper()
	diags := findDiagnostics(report, code)
	if len(diags) == 0 {
		t.Fatalf("no diagnostic with code %q in report: %s", code, reportJSON(t, report))
	}
	if diags[0].Status != status {
		t.Fatalf("diagnostic %q status = %q, want %q", code, diags[0].Status, status)
	}
	return diags[0]
}

func requireNoCode(t *testing.T, report *workspaceReadinessReport, code string) {
	t.Helper()
	if diags := findDiagnostics(report, code); len(diags) > 0 {
		t.Fatalf("unexpected diagnostic %q: %+v", code, diags[0])
	}
}

func reportJSON(t *testing.T, report *workspaceReadinessReport) string {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestDoctorWorkspaceOutsideWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	report := runReadiness(t, workspaceReadinessOptions{})
	if report.Status != readinessStatusNotReady {
		t.Fatalf("status = %q, want not_ready", report.Status)
	}
	requireCode(t, report, codeWorkspaceNotFound, "fail")
}

func TestDoctorWorkspaceReportsResolvedReferencedModule(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "module.codefly.yaml"), []byte("kind: module\nname: host\nservices: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: solution\nlayout: modules\nmodules:\n    - name: host\n      path: " + source + "\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireNoCode(t, report, codeModuleReferenceUnresolved)
	found := false
	for _, d := range report.Checks {
		if d.Name == "referenced module host" && d.Status == "ok" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved referenced module not reported: %s", reportJSON(t, report))
	}
}

func TestDoctorWorkspaceFlagsUnresolvedReferencedModule(t *testing.T) {
	dir := writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: solution\nlayout: modules\nmodules:\n    - name: host\n      path: /nonexistent/host\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, codeModuleReferenceUnresolved, "fail")
	if report.Status != readinessStatusNotReady {
		t.Fatalf("status = %q, want not_ready", report.Status)
	}
}

func TestDoctorWorkspaceMalformedWorkspace(t *testing.T) {
	dir := writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: [unclosed\n  bad yaml::\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, codeWorkspaceInvalid, "fail")
	if report.Status != readinessStatusNotReady {
		t.Fatalf("status = %q, want not_ready", report.Status)
	}
}

func TestDoctorWorkspaceLocalEnvironment(t *testing.T) {
	t.Run("undeclared local is implicit", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, nil)
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		if report.Status != readinessStatusReady {
			t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
		}
		if report.EnvironmentDeclared {
			t.Fatal("undeclared local reported as declared")
		}
	})
	t.Run("declared local", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, nil)
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		if report.Status != readinessStatusReady {
			t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
		}
		if !report.EnvironmentDeclared {
			t.Fatal("declared local reported as undeclared")
		}
	})
}

func TestDoctorWorkspaceEnvironmentNotFound(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAMLLocalDeclared, nil, nil)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, env: "staging"})
	diag := requireCode(t, report, codeEnvironmentNotFound, "fail")
	if !strings.Contains(diag.Message, "staging") {
		t.Fatalf("message does not name the environment: %q", diag.Message)
	}
}

func TestDoctorWorkspaceNoConfigurationRequired(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, nil)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if report.Status != readinessStatusReady {
		t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
	}
	requireNoCode(t, report, codeConfigurationDirMissing)
}

func TestDoctorWorkspaceRequiredWorkspaceConfiguration(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
			"configurations/local/auth0.env": "CLIENT_ID=public-id\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		if report.Status != readinessStatusReady {
			t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
		}
	})
	t.Run("missing configuration", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
			"configurations/local/other.env": "KEY=value\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		diag := requireCode(t, report, codeConfigurationMissing, "fail")
		if !strings.Contains(diag.Message, "auth0") || !strings.Contains(diag.Message, "backend/api") {
			t.Fatalf("message should name the configuration and requiring service: %q", diag.Message)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, nil)
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		requireCode(t, report, codeConfigurationDirMissing, "fail")
		if dirExists(filepath.Join(dir, "configurations", "local")) {
			t.Fatal("doctor created the missing configurations directory")
		}
	})
	t.Run("empty configuration", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
			"configurations/local/auth0.env": "\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		requireCode(t, report, codeConfigurationMissing, "fail")
	})
}

func TestDoctorWorkspaceServiceConfigurations(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
			"modules/backend/services/api/configurations/local/settings.env": "MODE=dev\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		if report.Status != readinessStatusReady {
			t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
		}
	})
	t.Run("missing for selected environment", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
			"modules/backend/services/api/configurations/dev/settings.env": "MODE=dev\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		diag := requireCode(t, report, codeConfigurationDirMissing, "warn")
		if !strings.Contains(diag.Message, "dev") {
			t.Fatalf("message should list the environments that do exist: %q", diag.Message)
		}
		if report.Status != readinessStatusReady {
			t.Fatalf("warn-only report should stay ready, got %q", report.Status)
		}
	})
}

func TestDoctorWorkspaceDuplicateConfiguration(t *testing.T) {
	t.Run("duplicate structured configuration", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
			"configurations/local/auth0.yaml":        "client: a\n",
			"configurations/local/auth0.secret.yaml": "token: b\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		requireCode(t, report, codeConfigurationDuplicate, "fail")
	})
	t.Run("duplicate key", func(t *testing.T) {
		dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
			"configurations/local/auth0.env":        "TOKEN=public\n",
			"configurations/local/auth0.secret.env": "TOKEN=hidden\n",
		})
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		requireCode(t, report, codeConfigurationDuplicate, "fail")
	})
}

func TestDoctorWorkspaceProviderExecutableMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := singleServiceWorkspace(t, testWorkspaceYAMLOnePassword, nil, map[string]string{
		"configurations/local/auth0.secret.env": "TOKEN=op://vault/item/field\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, codeProviderExecutableMissing, "fail")
	// The unusable backend is reported once; its references are not retried.
	requireNoCode(t, report, codeProviderNotConfigured)
	requireNoCode(t, report, codeProviderResolutionFailed)
}

func TestDoctorWorkspaceProviderNotConfigured(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
		"configurations/local/auth0.secret.env": "TOKEN=op://vault/item/field\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	diag := requireCode(t, report, codeProviderNotConfigured, "fail")
	if strings.Contains(diag.Message+diag.Remediation, "op://vault/item/field") {
		t.Fatalf("diagnostic leaks the raw reference: %+v", diag)
	}
}

func TestDoctorWorkspaceUnknownReferenceScheme(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
		"configurations/local/auth0.secret.env": "TOKEN=vault://kv/data/auth0\nCONNECTION=postgres://user@localhost:5432/db\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	diags := findDiagnostics(report, codeReferenceSchemeUnknown)
	if len(diags) != 1 {
		t.Fatalf("want exactly one unknown-scheme warning (postgres:// must pass), got %d: %s", len(diags), reportJSON(t, report))
	}
	if diags[0].Status != "warn" {
		t.Fatalf("unknown scheme should warn, got %q", diags[0].Status)
	}
	if !strings.Contains(diags[0].Message, `"vault"`) {
		t.Fatalf("warning should name the scheme: %q", diags[0].Message)
	}
	if report.Status != readinessStatusReady {
		t.Fatalf("warn-only report should stay ready, got %q", report.Status)
	}
}

// installFakeOp puts an executable `op` shim on PATH. Used only to exercise
// failure handling (hangs, auth errors) — successful resolution is qualified
// against the real provider, never faked.
func installFakeOp(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "op"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
}

func TestDoctorWorkspaceTimeout(t *testing.T) {
	// exec so the context kill hits the process holding the output pipes;
	// a child would inherit them and stall Run past the deadline.
	installFakeOp(t, "#!/bin/sh\nexec /bin/sleep 5\n")
	dir := singleServiceWorkspace(t, testWorkspaceYAMLOnePassword, nil, map[string]string{
		"configurations/local/auth0.secret.env": "TOKEN=op://vault/item/field\n",
	})
	start := time.Now()
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, timeout: 300 * time.Millisecond})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout not enforced: took %s", elapsed)
	}
	requireCode(t, report, codeTimeout, "fail")
}

func TestDoctorWorkspaceProviderAuthenticationRequired(t *testing.T) {
	installFakeOp(t, "#!/bin/sh\necho 'you are not currently signed in' >&2\nexit 1\n")
	dir := singleServiceWorkspace(t, testWorkspaceYAMLOnePassword, nil, map[string]string{
		"configurations/local/auth0.secret.env": "TOKEN=op://vault/item/field\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	requireCode(t, report, codeProviderAuthRequired, "fail")
}

func TestDoctorWorkspaceProviderResolutionFailed(t *testing.T) {
	installFakeOp(t, "#!/bin/sh\necho 'isn'\\''t an item' >&2\nexit 1\n")
	dir := singleServiceWorkspace(t, testWorkspaceYAMLOnePassword, nil, map[string]string{
		"configurations/local/auth0.secret.env": "TOKEN=op://vault/item/field\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	diag := requireCode(t, report, codeProviderResolutionFailed, "fail")
	if strings.Contains(reportJSON(t, report), "op://vault/item/field") {
		t.Fatalf("report leaks the raw reference: %+v", diag)
	}
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if entry.IsDir() {
			snapshot[rel] = "dir"
			return nil
		}
		content, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(content)
		snapshot[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDoctorWorkspaceIsReadOnly(t *testing.T) {
	// Exercise every branch that could plausibly write: missing workspace
	// configuration directory, service configurations, secret references.
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
		"modules/backend/services/api/configurations/dev/settings.env": "MODE=dev\n",
	})
	before := snapshotTree(t, dir)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if report.Status != readinessStatusNotReady {
		t.Fatalf("fixture should not be ready: %s", reportJSON(t, report))
	}
	after := snapshotTree(t, dir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace tree changed:\nbefore: %v\nafter:  %v", before, after)
	}
}

func TestDoctorWorkspaceDoesNotStartAnything(t *testing.T) {
	// The fixture's agent does not exist locally and no daemon is running: the
	// check must still complete because it never spawns agents or services.
	home := t.TempDir()
	t.Setenv("CODEFLY_HOME", home)
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, map[string]string{
		"configurations/local/auth0.env": "CLIENT_ID=abc\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if report.Status != readinessStatusReady {
		t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("codefly home touched during readiness check: %v", entries)
	}
}

func TestDoctorWorkspaceOutputContainsNoValues(t *testing.T) {
	const plaintextSecret = "SUPER-SECRET-VALUE-8f2a"
	const rawReference = "op://team-vault/auth0/client-secret"
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
		"configurations/local/auth0.secret.env":                            "PASSWORD=" + plaintextSecret + "\n",
		"configurations/local/token.secret.env":                            "TOKEN=" + rawReference + "\n",
		"configurations/local/blob.secret.yaml":                            "nested:\n    key: " + plaintextSecret + "\n",
		"configurations/local/public.env":                                  "URL=https://example.com\n",
		"modules/backend/services/api/configurations/local/svc.secret.env": "API_KEY=" + plaintextSecret + "\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	payload := reportJSON(t, report)
	if strings.Contains(payload, plaintextSecret) {
		t.Fatal("JSON report contains a secret value")
	}
	if strings.Contains(payload, rawReference) {
		t.Fatal("JSON report contains a raw secret reference")
	}
}

func TestDoctorWorkspaceDeterministicOutput(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0", "stripe"}, map[string]string{
		"configurations/local/auth0.env":        "CLIENT_ID=abc\n",
		"configurations/local/stripe.env":       "MODE=test\n",
		"configurations/local/extra.env":        "A=1\n",
		"configurations/local/blob.secret.yaml": "a: x\nb:\n    - y\n    - z\n",
	})
	first := reportJSON(t, runReadiness(t, workspaceReadinessOptions{dir: dir}))
	for range 5 {
		next := reportJSON(t, runReadiness(t, workspaceReadinessOptions{dir: dir}))
		if next != first {
			t.Fatalf("report is not deterministic:\nfirst: %s\nnext:  %s", first, next)
		}
	}
	var decoded workspaceReadinessReport
	if err := json.Unmarshal([]byte(first), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != doctorWorkspaceSchemaVersion {
		t.Fatalf("schema_version = %d, want %d", decoded.SchemaVersion, doctorWorkspaceSchemaVersion)
	}
}

func twoServiceWorkspace(t *testing.T) string {
	t.Helper()
	return writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                               testWorkspaceYAML,
		"modules/backend/module.codefly.yaml":                  testModuleYAML("api", "worker"),
		"modules/backend/services/api/service.codefly.yaml":    testServiceYAML("api", "shared"),
		"modules/backend/services/worker/service.codefly.yaml": testServiceYAML("worker"),
		"configurations/local/shared.env":                      "REGION=eu\n",
		// Unrelated configurations that fail if resolved: op:// references
		// with no backend configured for the environment.
		"configurations/local/unrelated.secret.env":                           "TOKEN=op://vault/other/token\n",
		"modules/backend/services/worker/configurations/local/bad.secret.env": "KEY=op://vault/worker/key\n",
	})
}

func TestDoctorWorkspaceServiceScopedDoesNotResolveUnrelated(t *testing.T) {
	dir := twoServiceWorkspace(t)

	unscoped := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if unscoped.Status != readinessStatusNotReady {
		t.Fatalf("unscoped run should fail on the unrelated references: %s", reportJSON(t, unscoped))
	}
	requireCode(t, unscoped, codeProviderNotConfigured, "fail")

	scoped := runReadiness(t, workspaceReadinessOptions{dir: dir, service: "api"})
	if scoped.Status != readinessStatusReady {
		t.Fatalf("scoped run must not resolve unrelated configurations: %s", reportJSON(t, scoped))
	}
	requireNoCode(t, scoped, codeProviderNotConfigured)
	if scoped.Service != "backend/api" {
		t.Fatalf("scoped service = %q, want backend/api", scoped.Service)
	}
	// Discovery may list configuration names, but no warning or failure may
	// come from configurations outside the service's declared dependencies.
	for _, diag := range scoped.Checks {
		if diag.Status != "ok" && (strings.Contains(diag.Message, "unrelated") || strings.Contains(diag.Message, "bad")) {
			t.Fatalf("scoped run diagnosed an unrelated configuration: %+v", diag)
		}
	}
}

func TestDoctorWorkspaceServiceNotFound(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, nil)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, service: "ghost"})
	requireCode(t, report, codeServiceNotFound, "fail")
}

func TestDoctorWorkspaceConcurrentChecksAreIndependent(t *testing.T) {
	readyDir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
		"configurations/local/auth0.env": "CLIENT_ID=abc\n",
	})
	failingDir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"missing"}, nil)

	readyBaseline := reportJSON(t, runReadiness(t, workspaceReadinessOptions{dir: readyDir}))
	failingBaseline := reportJSON(t, runReadiness(t, workspaceReadinessOptions{dir: failingDir}))

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		dir, baseline := readyDir, readyBaseline
		if i%2 == 1 {
			dir, baseline = failingDir, failingBaseline
		}
		wg.Go(func() {
			report := workspaceReadiness(context.Background(), workspaceReadinessOptions{dir: dir, env: "local", timeout: 30 * time.Second})
			data, err := json.Marshal(report)
			if err != nil {
				errs <- err
				return
			}
			if string(data) != baseline {
				errs <- fmt.Errorf("concurrent report diverged from baseline:\ngot:  %s\nwant: %s", data, baseline)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// captureStdout runs fn while collecting everything written to os.Stdout.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		done <- string(data)
	}()
	runErr := fn()
	os.Stdout = old
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out, runErr
}

func runDoctorWorkspaceCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	doctorWorkspaceEnv = "local"
	doctorWorkspaceService = ""
	doctorWorkspaceJSON = false
	doctorWorkspaceTimeout = 30 * time.Second
	RootCmd.SetArgs(append([]string{"doctor", "workspace"}, args...))
	defer RootCmd.SetArgs(nil)
	return captureStdout(t, func() error {
		return RootCmd.ExecuteContext(context.Background())
	})
}

func TestDoctorWorkspaceCommandJSON(t *testing.T) {
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"auth0"}, map[string]string{
		"configurations/local/auth0.env": "CLIENT_ID=abc\n",
	})
	t.Chdir(dir)
	out, err := runDoctorWorkspaceCommand(t, "--json")
	if err != nil {
		t.Fatalf("ready workspace returned error: %v\noutput: %s", err, out)
	}
	var report workspaceReadinessReport
	if jsonErr := json.Unmarshal([]byte(out), &report); jsonErr != nil {
		t.Fatalf("stdout is not a JSON report: %v\noutput: %s", jsonErr, out)
	}
	if report.SchemaVersion != doctorWorkspaceSchemaVersion || report.Status != readinessStatusReady {
		t.Fatalf("unexpected report: %s", out)
	}
}

func TestDoctorWorkspaceCommandFailureIsMachineReadable(t *testing.T) {
	const plaintextSecret = "SUPER-SECRET-VALUE-77c1"
	dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"missing"}, map[string]string{
		"configurations/local/auth0.secret.env": "PASSWORD=" + plaintextSecret + "\nTOKEN=op://team-vault/x/y\n",
	})
	t.Chdir(dir)

	out, err := runDoctorWorkspaceCommand(t, "--json")
	if err == nil {
		t.Fatal("not-ready workspace exited without error")
	}
	if !IsMachineReadableError(err) {
		t.Fatalf("JSON failure should be machine-readable, got %v", err)
	}
	var report workspaceReadinessReport
	if jsonErr := json.Unmarshal([]byte(out), &report); jsonErr != nil {
		t.Fatalf("stdout is not a JSON report: %v\noutput: %s", jsonErr, out)
	}
	if strings.Contains(out, plaintextSecret) || strings.Contains(out, "op://team-vault/x/y") {
		t.Fatal("JSON output leaks a secret value or raw reference")
	}

	humanOut, humanErr := runDoctorWorkspaceCommand(t)
	if humanErr == nil {
		t.Fatal("not-ready workspace exited without error in human mode")
	}
	if IsMachineReadableError(humanErr) {
		t.Fatal("human-mode failure must not be marked machine-readable")
	}
	if strings.Contains(humanOut, plaintextSecret) || strings.Contains(humanOut, "op://team-vault/x/y") {
		t.Fatal("human output leaks a secret value or raw reference")
	}
	if !strings.Contains(humanOut, codeConfigurationMissing) {
		t.Fatalf("human output should show the diagnostic code, got:\n%s", humanOut)
	}
}
