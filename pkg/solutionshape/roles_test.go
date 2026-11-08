package solutionshape

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// notesRoutes are a unit's published routes: a read, a write, a publish no
// role grants, and a relay any checked caller may use.
const notesRoutes = `{"swagger":"2.0","basePath":"/","info":{"title":"notes backend","version":"0.0.1"},"paths":{` +
	`"/v1/notes/items":{` +
	`"get":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"notes.item","actions":["read"]}]}},` +
	`"post":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"notes.item","actions":["write"]}]}}},` +
	`"/v1/notes/items/publish":{"post":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"notes.item","actions":["publish"]}]}}},` +
	`"/v1/notes/modules/files/{path=**}":{"post":{"x-codefly-authority-policy":{"any_checked_caller":true}}}}}`

// notesContract is a contract with three roles, a spelled binding and one
// whose ceiling its calls decide; each test replaces what it is about.
const notesContract = `schema: codefly/module-contract/v1
principal: notes
owner_aware: true
installation_role: none
namespaces: []
queues: []
scope_ceilings:
    - resource_kind: notes.item
      actions: [read, write, publish]
roles:
    - name: installer
      scopes:
          - resource_kind: notes.item
            actions: [read, write, publish]
    - name: editor
      scopes:
          - resource_kind: notes.item
            actions: [read, write]
          - resource_kind: files.file
            actions: [read, write]
    - name: viewer
      scopes:
          - resource_kind: notes.item
            actions: [read]
          - resource_kind: files.file
            actions: [read]
bindings:
    - id: files
      operations: [invoke]
      audience: {from: notes-audiences/files-audience}
      binding_key: {from: notes/FILES_BINDING}
      scope_ceiling:
          invoke:
              - resource_kind: files.file
                actions: [read, write]
`

// writeUnit writes a unit's package (one service publishing routes) and its
// contract into dir.
func writeUnit(t *testing.T, dir, service, routes, contract string) {
	t.Helper()
	path := filepath.Join("contracts", "api", service, "http", "openapi.json")
	write(t, filepath.Join(dir, path), routes)
	sum := sha256.Sum256([]byte(routes))
	write(t, filepath.Join(dir, "module.package.codefly.yaml"), `kind: module-package
schema: codefly/module-package/v2
id: acme/unit
version: 0.1.0
minimum-codefly-version: ">=0.1.220 <1.0.0"
artifact-roots:
    - contracts
    - services
services:
    - name: `+service+`
      endpoints: [http]
      api-contracts:
        - endpoint: http
          kind: openapi
          package: notes backend
          path: `+path+`
          digest: sha256:`+hex.EncodeToString(sum[:])+`
contracts:
    composition: ">=2.0.0 <3.0.0"
`)
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), contract)
}

// modulePage writes a module that ships no solution manifest and declares a
// page beside its one service.
func modulePage(t *testing.T, contract string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "module.codefly.yaml"), "kind: module\nname: notes\nservice-entry: api\nservices:\n    - name: api\n")
	write(t, filepath.Join(dir, "services", "api", FrontendFileName), "title: Notes\norder: 40\nexposed_module: ./Page\nhost_contract: 1\n")
	writeUnit(t, dir, "api", notesRoutes, contract)
	return dir
}

func roleNamed(t *testing.T, report *Report, name string) RoleReach {
	t.Helper()
	for _, role := range report.Roles {
		if role.Role == name {
			return role
		}
	}
	t.Fatalf("no role %q in %+v", name, report.Roles)
	return RoleReach{}
}

// Each role reaches exactly the operations the authority would admit for it,
// and an operation any checked caller may use is counted once, not listed
// under every role.
func TestEachRoleReachesWhatTheAuthorityAdmits(t *testing.T) {
	report := Inspect("notes", modulePage(t, notesContract))
	if got := roleNamed(t, report, "viewer").Operations; !slices.Equal(got, []string{"GET /v1/notes/items"}) {
		t.Errorf("viewer reaches %v", got)
	}
	if got := roleNamed(t, report, "editor").Operations; !slices.Equal(got, []string{"GET /v1/notes/items", "POST /v1/notes/items"}) {
		t.Errorf("editor reaches %v", got)
	}
	if got := roleNamed(t, report, "installer").Operations; len(got) != 3 {
		t.Errorf("installer reaches %v, want all three role-gated operations", got)
	}
	if report.OpenOperations != 1 {
		t.Errorf("open operations = %d, want the one relay", report.OpenOperations)
	}
	viewer := roleNamed(t, report, "viewer")
	if len(viewer.Bindings) != 1 || !slices.Equal(viewer.Bindings[0].Carries, []string{"files.file:read"}) {
		t.Errorf("viewer carries %+v through the binding, want files.file:read only", viewer.Bindings)
	}
	if roleNamed(t, report, "installer").Bindings != nil {
		t.Error("the installer grants nothing the binding spells, so it carries nothing through it")
	}
	for _, code := range []string{CodeOperationReachedByNoRole, CodeBindingWiderThanRoles} {
		if check := checkFor(t, report, code); check.Status != StatusOK {
			t.Errorf("%s: %s %q", code, check.Status, check.Message)
		}
	}
}

// An operation requiring what no role carries can never be called by a
// person; the authority refuses it at every call. Reported before boot.
func TestAnOperationNoRoleReachesWarns(t *testing.T) {
	contract := strings.Replace(notesContract, "actions: [read, write, publish]\n    - name: editor", "actions: [read, write]\n    - name: editor", 1)
	report := Inspect("notes", modulePage(t, contract))
	check := checkFor(t, report, CodeOperationReachedByNoRole)
	if check.Status != StatusWarn || !strings.Contains(check.Message, "POST /v1/notes/items/publish") {
		t.Fatalf("got %s %q, want a warning naming the publish route", check.Status, check.Message)
	}
	if strings.Contains(check.Message, "modules/files") {
		t.Error("an operation any checked caller may use needs no role")
	}
	if !report.Conforms() {
		t.Fatal("an unreachable operation refuses calls but breaks no delivery; it warns")
	}
}

// A binding redeemed for a person carries the person's grants cut to the
// binding, so what no role grants is never carried.
func TestABindingWiderThanEveryRoleWarns(t *testing.T) {
	t.Run("a spelled action no role grants", func(t *testing.T) {
		contract := strings.Replace(notesContract, "              - resource_kind: files.file\n                actions: [read, write]\n",
			"              - resource_kind: files.file\n                actions: [read, write, delete]\n", 1)
		report := Inspect("notes", modulePage(t, contract))
		check := checkFor(t, report, CodeBindingWiderThanRoles)
		if check.Status != StatusWarn || !strings.Contains(check.Message, "files (invoke) asks files.file:delete") {
			t.Fatalf("got %s %q", check.Status, check.Message)
		}
	})
	t.Run("a binding its calls decide, with nothing outside the unit's kinds granted", func(t *testing.T) {
		contract := strings.Replace(withoutFileGrants(notesContract), `      scope_ceiling:
          invoke:
              - resource_kind: files.file
                actions: [read, write]
`, `      calls:
          invoke:
              - /files.v1.FileService/Get
`, 1)
		report := Inspect("notes", modulePage(t, contract))
		check := checkFor(t, report, CodeBindingWiderThanRoles)
		if check.Status != StatusWarn || !strings.Contains(check.Message, "files (invoke) carries nothing for any role") {
			t.Fatalf("got %s %q", check.Status, check.Message)
		}
		if binding := roleNamed(t, report, "viewer").Bindings; len(binding) != 1 || !binding[0].ResolvedByComposition {
			t.Fatalf("a calls binding is shown as resolved by the composition: %+v", binding)
		}
	})
	t.Run("a role naming the binding carries all of it", func(t *testing.T) {
		contract := strings.Replace(withoutFileGrants(notesContract), "    - name: viewer\n", "    - name: viewer\n      bindings:\n          - binding: files\n            operation: invoke\n", 1)
		report := Inspect("notes", modulePage(t, contract))
		if check := checkFor(t, report, CodeBindingWiderThanRoles); check.Status != StatusOK {
			t.Fatalf("got %s %q", check.Status, check.Message)
		}
		viewer := roleNamed(t, report, "viewer")
		if len(viewer.Bindings) != 1 || !viewer.Bindings[0].WholeBinding || !slices.Equal(viewer.Bindings[0].Carries, []string{"files.file:read", "files.file:write"}) {
			t.Fatalf("viewer carries %+v, want the binding's whole ceiling", viewer.Bindings)
		}
	})
	t.Run("a headless binding acts for nobody and is not judged", func(t *testing.T) {
		// Core already refuses a role granting what only a headless binding
		// asks for, so the roles grant nothing at files here.
		contract := withoutFileGrants(notesContract)
		contract = strings.Replace(contract, "operations: [invoke]", "operations: [headless]", 1)
		contract = strings.Replace(contract, "          invoke:\n              - resource_kind: files.file", "          headless:\n              - resource_kind: files.file", 1)
		report := Inspect("notes", modulePage(t, contract))
		if check := checkFor(t, report, CodeBindingWiderThanRoles); check.Status != StatusOK {
			t.Fatalf("got %s %q", check.Status, check.Message)
		}
	})
}

// A module the page reaches with the person's own Work Context is declared
// as an edge dependency, and needs no binding: that is the design.
func TestAnEdgeDependencyNeedsNoBinding(t *testing.T) {
	dir := conformingSolution(t)
	contract := strings.Replace(readFile(t, filepath.Join(dir, "module.contract.codefly.yaml")),
		"owner_aware: true\n", "owner_aware: true\nedge_dependencies:\n    - module: documents\n      presence: required\n", 1)
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), contract)
	report := Inspect("unit", dir)
	check := checkFor(t, report, CodeConsumedWithoutBinding)
	if check.Status != StatusOK || !strings.Contains(check.Message, "edge_dependencies: documents") {
		t.Fatalf("got %s %q", check.Status, check.Message)
	}
	if got := report.Roles[0].AsPerson; !slices.Equal(got, []string{"documents"}) {
		t.Errorf("the role reaches %v as the person, want documents", got)
	}
}

func TestInstallationRoleIsDeclared(t *testing.T) {
	for _, test := range []struct {
		name     string
		selector string
		page     bool
		want     Status
	}{
		{"undeclared on a solution", "", false, StatusWarn},
		{"none on a solution", "installation_role: none\n", false, StatusOK},
		{"a role on a solution", "installation_role: installer\n", false, StatusOK},
		{"undeclared on a module page", "", true, StatusWarn},
		{"a role on a module page", "installation_role: viewer\n", true, StatusWarn},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract := strings.Replace(notesContract, "installation_role: none\n", test.selector, 1)
			var report *Report
			if test.page {
				report = Inspect("notes", modulePage(t, contract))
			} else {
				dir := conformingSolution(t)
				write(t, filepath.Join(dir, "module.contract.codefly.yaml"), contract)
				report = Inspect("notes", dir)
			}
			if check := checkFor(t, report, CodeInstallationRoleUndeclared); check.Status != test.want {
				t.Fatalf("got %s %q, want %s", check.Status, check.Message, test.want)
			}
		})
	}
}

// A module page was "not a solution" and judged on nothing. People reach it
// through its roles, so it is judged on them.
func TestAModulePageIsJudged(t *testing.T) {
	t.Run("with roles it conforms", func(t *testing.T) {
		report := Inspect("notes", modulePage(t, notesContract))
		if !report.IsModulePage || report.IsSolution || !report.Conforms() {
			t.Fatalf("module page=%v solution=%v status=%s: %+v", report.IsModulePage, report.IsSolution, report.Status, report.Checks)
		}
	})
	t.Run("with no role it is shown to nobody", func(t *testing.T) {
		contract := notesContract[:strings.Index(notesContract, "roles:\n")] + "bindings: []\n"
		report := Inspect("notes", modulePage(t, contract))
		if check := checkFor(t, report, CodeModulePageWithoutRoles); check.Status != StatusFail {
			t.Fatalf("got %s %q", check.Status, check.Message)
		}
	})
	t.Run("with no contract there is no role to grant", func(t *testing.T) {
		dir := modulePage(t, notesContract)
		removeFile(t, filepath.Join(dir, "module.contract.codefly.yaml"))
		if check := checkFor(t, Inspect("notes", dir), CodeContractMissing); check.Status != StatusFail {
			t.Fatalf("got %s %q", check.Status, check.Message)
		}
	})
	t.Run("a module with no page is still not a solution", func(t *testing.T) {
		dir := modulePage(t, notesContract)
		removeFile(t, filepath.Join(dir, "services", "api", FrontendFileName))
		report := Inspect("notes", dir)
		if report.IsModulePage || report.IsSolution || checkFor(t, report, CodeNotASolution).Status != StatusOK {
			t.Fatalf("a plain module was judged: %+v", report)
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

// withoutFileGrants drops every role's grant at the files module.
func withoutFileGrants(contract string) string {
	contract = strings.ReplaceAll(contract, "          - resource_kind: files.file\n            actions: [read, write]\n", "")
	return strings.ReplaceAll(contract, "          - resource_kind: files.file\n            actions: [read]\n", "")
}
