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

// A composition-root group's references are checked too — the groups no service
// declares.
//
// It is the case the doctor was blind to, and the worst one: a root group is
// provided to every service in the run, so a typo in it goes missing from all of
// them, and nothing declares it for a declared-groups-only check to find. The
// doctor now derives the root names from core's own loader and checks that half
// against the whole workspace.
func TestDoctorWorkspaceChecksCompositionRootGroupReferences(t *testing.T) {
	workerYAML := testServiceYAML("worker") + "endpoints:\n    - name: grpc\n      api: grpc\n"
	files := map[string]string{
		"modules/backend/module.codefly.yaml":                  testModuleYAML("api", "worker"),
		"modules/backend/services/worker/service.codefly.yaml": workerYAML,
		// No service declares this group: it is the composition root's own.
		"configurations/local/work-context.env": "worker-endpoint=${endpoint:bakcend/worker/grpc}\n",
	}
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, files)

	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if report.Status == readinessStatusReady {
		t.Fatalf("status = ready, want not ready: %s", reportJSON(t, report))
	}
	diagnostics := findDiagnostics(report, codeConfigurationReference)
	if len(diagnostics) != 1 {
		t.Fatalf("want the root group's typo reported once, got %d: %s", len(diagnostics), reportJSON(t, report))
	}
	for _, want := range []string{"work-context/worker-endpoint", "bakcend/worker"} {
		if !strings.Contains(diagnostics[0].Message, want) {
			t.Fatalf("diagnostic = %q, want it to contain %q", diagnostics[0].Message, want)
		}
	}
}

// When the workspace configurations cannot be loaded, the doctor says it did not
// check the references instead of reporting that they all resolve.
//
// This is the regression the layer-5 round-four review reproduced. The reference
// check used to fall back to a plain disk read when the loader could not load,
// and that read carries no composition-root names — so the check silently
// narrowed to declared groups and then printed "every endpoint reference
// resolves, in the groups each service declares and in the composition root's
// own". One unsupplied ${profile} value in an unrelated group was enough: the
// workspace below holds a typo'd producer in a root group and was reported
// ready.
//
// The doctor's own contract is the standard here: nothing returns silently. A
// check that could not run says so.
func TestDoctorWorkspaceSaysWhenItCouldNotCheckReferences(t *testing.T) {
	workerYAML := testServiceYAML("worker") + "endpoints:\n    - name: grpc\n      api: grpc\n"
	files := map[string]string{
		"modules/backend/module.codefly.yaml":                  testModuleYAML("api", "worker"),
		"modules/backend/services/worker/service.codefly.yaml": workerYAML,
		"configurations/local/work-context.env":                "worker-endpoint=${endpoint:bakcend/worker/grpc}\n",
		// Unrelated to the reference, and enough to fail the load: a value the
		// group declares as supplied per profile that this profile does not
		// supply.
		"configurations/local/pending.env": "MODE=${profile}\n",
	}
	dir := singleServiceWorkspace(t, testWorkspaceYAML, nil, files)

	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	if report.Status == readinessStatusReady {
		t.Fatalf("status = ready, want not ready: %s", reportJSON(t, report))
	}
	check := findCheck(report, "configuration references")
	if check == nil {
		t.Fatalf("no configuration references check in report: %s", reportJSON(t, report))
	}
	if check.Status == "ok" {
		t.Fatalf("the doctor reported a clean bill it never established: %+v", *check)
	}
	if !strings.Contains(check.Message, "not checked") {
		t.Fatalf("message = %q, want it to say the check did not run", check.Message)
	}
}
