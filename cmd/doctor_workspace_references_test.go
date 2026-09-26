package cmd

import (
	"strings"
	"testing"
)

// The doctor runs the plan-time reference check standalone: one failure per
// unresolved ${endpoint:…} reference, naming the consumer, the key and the
// producer, while a reference that resolves passes.
func TestDoctorWorkspaceChecksEndpointReferences(t *testing.T) {
	workerYAML := testServiceYAML("worker") + "endpoints:\n    - name: grpc\n      api: grpc\n"
	files := map[string]string{
		"modules/backend/module.codefly.yaml":                  testModuleYAML("api", "worker"),
		"modules/backend/services/worker/service.codefly.yaml": workerYAML,
	}

	t.Run("resolvable", func(t *testing.T) {
		files["configurations/local/platform.env"] = "worker-endpoint=${endpoint:backend/worker/grpc}\n"
		dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"platform"}, files)
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		if report.Status != readinessStatusReady {
			t.Fatalf("status = %q, want ready: %s", report.Status, reportJSON(t, report))
		}
		requireNoCode(t, report, codeConfigurationReference)
	})

	t.Run("unresolved", func(t *testing.T) {
		files["configurations/local/platform.env"] = "worker-endpoint=${endpoint:backend/worker/grpc}\n" +
			"store-endpoint=${endpoint:store/db/tcp}\n" +
			"worker-admin=${endpoint:backend/worker/admin}\n"
		dir := singleServiceWorkspace(t, testWorkspaceYAML, []string{"platform"}, files)
		report := runReadiness(t, workspaceReadinessOptions{dir: dir})
		if report.Status == readinessStatusReady {
			t.Fatalf("status = ready, want not ready: %s", reportJSON(t, report))
		}
		diagnostics := findDiagnostics(report, codeConfigurationReference)
		if len(diagnostics) != 2 {
			t.Fatalf("want one failure per unresolved reference, got %d: %s", len(diagnostics), reportJSON(t, report))
		}
		for i, want := range []string{"backend/api: platform/store-endpoint = ${endpoint:store/db/tcp} (producer store/db)", "backend/api: platform/worker-admin = ${endpoint:backend/worker/admin} (producer backend/worker)"} {
			if diagnostics[i].Status != "fail" || !strings.Contains(diagnostics[i].Message, want) {
				t.Fatalf("diagnostic %d = %+v, want a failure containing %q", i, diagnostics[i], want)
			}
		}
		// The two references fail for different reasons and need different
		// answers: store/db is in no module of this workspace, while
		// backend/worker is composed and simply declares no `admin` endpoint.
		// Telling the second reader to compose the producer in is wrong advice.
		if want := "compose the module providing store/db into this workspace"; !strings.Contains(diagnostics[0].Remediation, want) {
			t.Fatalf("remediation for an absent producer = %q, want it to contain %q", diagnostics[0].Remediation, want)
		}
		if want := "declare the endpoint on backend/worker"; !strings.Contains(diagnostics[1].Remediation, want) {
			t.Fatalf("remediation for a missing endpoint = %q, want it to contain %q", diagnostics[1].Remediation, want)
		}
		if bad := "compose"; strings.Contains(diagnostics[1].Remediation, bad) {
			t.Fatalf("remediation for a missing endpoint = %q, must not suggest composing an already-composed producer", diagnostics[1].Remediation)
		}
	})
}
