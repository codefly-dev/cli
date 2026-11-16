package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assistantContract asks for two optional bindings, each filled from the
// composition's `assistant` group: a code sandbox and a ratings store. A
// composition that leaves a binding's slots empty omits it, and the feature
// behind it answers "disabled" — the state a local run kept while the hosted
// environment filled both.
const assistantContract = `schema: codefly/module-contract/v1
principal: backend
namespaces: [backend]
queues: []
scope_ceilings: []
bindings:
  - id: sandbox
    optional: true
    operations: [invoke]
    audience: {from: assistant/sandbox-audience}
    binding_key: {from: assistant/sandbox-binding}
    scope_ceiling:
      invoke:
        - resource_kind: sandbox.runs
          actions: [invoke]
  - id: annotations
    optional: true
    operations: [invoke]
    audience: {from: assistant/annotations-audience}
    binding_key: {from: assistant/annotations-binding}
    scope_ceiling:
      invoke:
        - resource_kind: annotations.annotations
          actions: [write]
destinations: []
`

const parityWorkspaceYAML = `name: demo
layout: modules
modules:
    - name: backend
environments:
    - name: local
      configuration-profiles: [local]
      exclude-services:
        - service: backend/worker
    - name: hosted
      configuration-profiles: [hosted]
`

const filledSlots = "sandbox-audience=sandbox\nsandbox-binding=sandbox\nannotations-audience=annotations\nannotations-binding=annotations\n"

func parityWorkspace(t *testing.T, local, hosted, declared string) string {
	t.Helper()
	return parityWorkspaceDeclaring(t, parityWorkspaceYAML, local, hosted, declared)
}

func parityWorkspaceDeclaring(t *testing.T, workspace, local, hosted, declared string) string {
	t.Helper()
	files := map[string]string{
		"modules/backend/module.contract.codefly.yaml":         assistantContract,
		"modules/backend/services/worker/service.codefly.yaml": testServiceYAML("worker"),
		"modules/backend/module.codefly.yaml":                  testModuleYAML("api", "worker"),
		"configurations/local/assistant.env":                   local,
		"configurations/hosted/assistant.env":                  hosted,
	}
	if declared != "" {
		files[ParityFileName] = "schema: " + paritySchema + "\ndifferences:\n" + declared
	}
	return singleServiceWorkspace(t, workspace, nil, files)
}

func parityReport(t *testing.T, dir string) *workspaceReadinessReport {
	t.Helper()
	return environmentParity(context.Background(), dir, "local", "hosted")
}

// The local run left the sandbox and annotations slots empty while the hosted
// environment filled them: the bindings are omitted locally and delivered
// there. A one-way check ("set locally ⇒ set hosted") passes this; both
// directions fail it, naming each binding and each key.
func TestParityRefusesBindingsTheTargetFillsAndLocalLeavesEmpty(t *testing.T) {
	empty := "sandbox-audience=\nsandbox-binding=\nannotations-audience=\nannotations-binding=\n"
	report := parityReport(t, parityWorkspace(t, empty, filledSlots, ""))
	if report.Status != readinessStatusNotReady {
		t.Fatalf("want not ready: %s", reportJSON(t, report))
	}
	bindings := findDiagnostics(report, codeParityBinding)
	if len(bindings) != 2 {
		t.Fatalf("want the sandbox and annotations bindings, got %s", reportJSON(t, report))
	}
	for i, id := range []string{"backend/annotations", "backend/sandbox"} {
		if bindings[i].Name != id || !strings.Contains(bindings[i].Message, "delivered in hosted and omitted in local") {
			t.Fatalf("binding finding %d = %+v, want %s delivered in hosted and omitted in local", i, bindings[i], id)
		}
	}
	if keys := findDiagnostics(report, codeParityConfiguration); len(keys) != 4 {
		t.Fatalf("want one configuration finding per empty slot, got %s", reportJSON(t, report))
	}
}

// The converse: what local fills and the target does not is the one-way rule
// the old gate had, and it still holds.
func TestParityRefusesWhatLocalFillsAndTheTargetDoesNot(t *testing.T) {
	report := parityReport(t, parityWorkspace(t, filledSlots+"feature=on\nproviders={}\n", filledSlots+"feature=off\nproviders=\n", `  - service: backend/worker
    reason: the worker runs only on a cell
`))
	got := requireCode(t, report, codeParityConfiguration, checkStatusFail)
	if got.Name != "assistant/feature" || !strings.Contains(got.Message, "filled in local and empty, off or absent in hosted") {
		t.Fatalf("finding = %+v", got)
	}
	if n := len(findDiagnostics(report, codeParityConfiguration)); n != 1 {
		t.Fatalf("an empty JSON document is as empty as an empty value; want one finding, got %d: %s", n, reportJSON(t, report))
	}
	requireNoCode(t, report, codeParityBinding)
}

func TestParityComparesTheComposedServices(t *testing.T) {
	report := parityReport(t, parityWorkspace(t, filledSlots, filledSlots, ""))
	got := requireCode(t, report, codeParityComposition, checkStatusFail)
	if got.Name != "backend/worker" || !strings.Contains(got.Message, "hosted composes backend/worker and local withdraws it") {
		t.Fatalf("finding = %+v", got)
	}
}

func TestParityAcceptsDeclaredDifferencesAndRefusesStaleOnes(t *testing.T) {
	declared := `  - service: backend/worker
    reason: the worker runs only on a cell
  - against: hosted
    configuration: assistant/*
    reason: the ratings store is not composed locally
  - binding: backend/annotations
    reason: the ratings store is not composed locally
  - binding: backend/sandbox
    against: [staging, hosted]
    reason: no longer true
  - binding: backend/sandbox
    against: staging
    reason: declared against another environment, so not judged here
`
	local := "sandbox-audience=sandbox\nsandbox-binding=sandbox\nannotations-audience=\nannotations-binding=\n"
	report := parityReport(t, parityWorkspace(t, local, filledSlots, declared))
	requireNoCode(t, report, codeParityBinding)
	requireNoCode(t, report, codeParityConfiguration)
	requireNoCode(t, report, codeParityComposition)
	stale := requireCode(t, report, codeParityDifferenceStale, checkStatusFail)
	if !strings.Contains(stale.Message, "backend/sandbox") {
		t.Fatalf("stale = %+v", stale)
	}
}

func TestParityRefusesADifferenceWithoutAReason(t *testing.T) {
	report := parityReport(t, parityWorkspace(t, filledSlots, filledSlots, "  - service: backend/worker\n"))
	requireCode(t, report, codeParityDifferenceBroken, checkStatusFail)
}

func TestParityPassesWhenTheEnvironmentsAgree(t *testing.T) {
	report := parityReport(t, parityWorkspace(t, filledSlots, filledSlots, `  - service: backend/worker
    reason: the worker runs only on a cell
`))
	if report.Status != readinessStatusReady {
		t.Fatalf("want ready: %s", reportJSON(t, report))
	}
}

// The generated runnable-bindings group is not compared (codefly writes it per
// run); the selections it is generated from are.
func TestParityComparesRunnableSelectionsNotTheGeneratedGroup(t *testing.T) {
	workspace := parityWorkspaceYAML + "      runnable-scope-selections:\n          BACKEND__API_GRPC_RUN: []\n"
	dir := parityWorkspaceDeclaring(t, workspace, filledSlots, filledSlots, `  - service: backend/worker
    reason: the worker runs only on a cell
`)
	if err := os.WriteFile(filepath.Join(dir, "configurations/hosted/runnable-bindings.env"), []byte("BACKEND__API_GRPC_RUN=generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := parityReport(t, dir)
	got := requireCode(t, report, codeParityBinding, checkStatusFail)
	if got.Name != "runnable/BACKEND__API_GRPC_RUN" || !strings.Contains(got.Message, "hosted selects runnable binding") {
		t.Fatalf("finding = %+v", got)
	}
	requireNoCode(t, report, codeParityConfiguration)
}
