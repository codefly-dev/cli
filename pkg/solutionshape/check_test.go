package solutionshape

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// conformingSolution writes the shape `codefly add solution` generates, so a
// test that breaks one rule changes exactly that rule.
func conformingSolution(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "module.codefly.yaml"), `kind: module
name: wiki
service-entry: backend
services:
    - name: backend
    - name: web
interface:
    endpoints:
        - service: backend
          endpoint: http
          visibility: public
        - service: web
          endpoint: http
          visibility: internal
`)
	write(t, filepath.Join(dir, "solution.codefly.yaml"), `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
api:
    exposes:
        - id: http
          protocol: http
    consumes:
        - id: documents
          protocol: connect
          module: documents
          service: documents
          endpoint: grpc
          as: documents
lifecycle:
    package: true
    render: true
`)
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), `schema: codefly/module-contract/v1
principal: wiki
owner_aware: true
namespaces: []
queues: []
scope_ceilings: []
bindings: []
`)
	write(t, filepath.Join(dir, "module.package.codefly.yaml"), `kind: module-package
schema: codefly/module-package/v2
id: obin/wiki
version: 0.1.0
minimum-codefly-version: ">=0.1.220 <1.0.0"
artifact-roots:
    - services
services:
    - name: backend
      endpoints: [http]
contracts:
    composition: ">=2.0.0 <3.0.0"
`)
	write(t, filepath.Join(dir, "services", "backend", "service.codefly.yaml"), `kind: service
name: backend
version: 0.0.0
agent:
    publisher: codefly.dev
    kind: codefly:service
    name: go
    version: 0.0.70
module-identity: true
service-dependencies:
    - interface: codefly.dev/authority@>=0.2.0 <0.4.0
    - name: hop
      module: authority
      kind: external
      endpoints:
        - name: grpc
endpoints:
    - name: http
      visibility: public
      exposure: none
      api: http
`)
	write(t, filepath.Join(dir, "services", "backend", FrontendFileName), `title: Wiki
order: 60
exposed_module: ./Page
host_contract: 1
`)
	write(t, filepath.Join(dir, "services", "web", "service.codefly.yaml"), `name: web
version: 0.0.0
agent:
    publisher: codefly.dev
    kind: codefly:service
    name: nextjs
    version: 0.0.167
endpoints:
    - name: http
      visibility: public
      exposure: none
`)
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func checkFor(t *testing.T, report *Report, code string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Code == code {
			return check
		}
	}
	t.Fatalf("no check %q in report: %+v", code, report.Checks)
	return Check{}
}

// The shape the CLI generates must be the shape the CLI accepts. If these ever
// disagree, one of them is wrong and a scaffolded solution is born failing.
func TestTheGeneratedShapeConforms(t *testing.T) {
	report := Inspect("wiki", conformingSolution(t))
	if !report.IsSolution {
		t.Fatal("a module shipping a solution manifest was not read as a solution")
	}
	if !report.Conforms() {
		for _, check := range report.Checks {
			if check.Status != StatusOK {
				t.Errorf("%s: %s — %s", check.Status, check.Code, check.Message)
			}
		}
		t.Fatal("the shape the scaffold generates does not pass the check")
	}
}

// A plain module is not a broken solution. This command is run across a whole
// tree, and reporting every library as a failing solution would make it useless.
func TestAModuleWithNoManifestIsNotASolution(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "module.codefly.yaml"), "kind: module\nname: plain\nservices: []\n")
	report := Inspect("plain", dir)
	if report.IsSolution {
		t.Fatal("a module with no solution manifest was read as a solution")
	}
	if !report.Conforms() {
		t.Fatal("a plain module was reported as a failing solution")
	}
}

// The rule that fails silently in production, which is why this checker exists.
func TestOwnerAwareMissingFails(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), `schema: codefly/module-contract/v1
principal: wiki
namespaces: []
queues: []
scope_ceilings: []
bindings: []
`)
	report := Inspect("wiki", dir)
	check := checkFor(t, report, CodeOwnerAwareMissing)
	if check.Status != StatusFail {
		t.Fatalf("owner_aware missing was %s, want fail", check.Status)
	}
	if report.Conforms() {
		t.Fatal("a solution that can act for nobody was reported as conforming")
	}
}

// The mesh rule: an edge to a module is a path past the authority's check.
func TestADirectModuleEdgeFails(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "services", "backend", "service.codefly.yaml"), `kind: service
name: backend
version: 0.0.0
agent:
    publisher: codefly.dev
    kind: codefly:service
    name: go
    version: 0.0.70
module-identity: true
service-dependencies:
    - interface: codefly.dev/authority@>=0.2.0 <0.4.0
    - name: documents
      module: documents
      endpoints:
        - name: grpc
endpoints:
    - name: http
      visibility: public
      exposure: none
      api: http
`)
	report := Inspect("wiki", dir)
	check := checkFor(t, report, CodeModuleEdgeDeclared)
	if check.Status != StatusFail {
		t.Fatalf("a direct module edge was %s, want fail", check.Status)
	}
	// The authority's own door is not an offence.
	if got := check.Message; got == "" {
		t.Fatal("the failure does not name the edge")
	}
}

// service-entry is load-bearing: a run resolves the solution manifest only
// through it, so a wrong one leaves every consumed route unrouted, silently.
func TestAWrongServiceEntryFails(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "module.codefly.yaml"), `kind: module
name: wiki
service-entry: web
services:
    - name: backend
    - name: web
`)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodeServiceEntryNotTheEntry).Status != StatusFail {
		t.Fatal("a service-entry naming the shell was accepted")
	}
}

// Without the package the render declares no presence, so nobody can install
// the solution and the contract's scopes reach no host.
func TestAMissingPackageFails(t *testing.T) {
	dir := conformingSolution(t)
	if err := os.Remove(filepath.Join(dir, "module.package.codefly.yaml")); err != nil {
		t.Fatal(err)
	}
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodePackageMissing).Status != StatusFail {
		t.Fatal("a solution with no published identity was accepted")
	}
}

// Publishing the shell is wrong but not broken: a host that federates the
// remote simply does not need it. That is a warning, not a failure.
func TestPublishingTheShellWarnsRatherThanFails(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "module.package.codefly.yaml"), `kind: module-package
schema: codefly/module-package/v2
id: obin/wiki
version: 0.1.0
minimum-codefly-version: ">=0.1.220 <1.0.0"
artifact-roots:
    - services
services:
    - name: backend
      endpoints: [http]
    - name: web
      endpoints: [http]
contracts:
    composition: ">=2.0.0 <3.0.0"
`)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodePackagePublishesShell).Status != StatusWarn {
		t.Fatal("publishing the shell should warn, not fail")
	}
	if !report.Conforms() {
		t.Fatal("a warning was treated as a failure")
	}
}

// A consumed entry with no route name is unroutable, and core's own schema
// lets it default — so nothing else catches it.
func TestAConsumedAPIWithNoRouteNameFails(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "solution.codefly.yaml"), `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
api:
    exposes:
        - id: http
          protocol: http
    consumes:
        - id: documents
          protocol: connect
          module: documents
          service: documents
          endpoint: grpc
lifecycle:
    package: true
    render: true
`)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodeConsumedAPIUnnamed).Status != StatusFail {
		t.Fatal("a consumed API with no route name was accepted")
	}
}

// The backend must present the solution's identity, or the authority refuses
// the whole authority request.
func TestAMissingModuleIdentityFails(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "services", "backend", "service.codefly.yaml"), `kind: service
name: backend
version: 0.0.0
agent:
    publisher: codefly.dev
    kind: codefly:service
    name: go
    version: 0.0.70
endpoints:
    - name: http
      visibility: public
      exposure: none
      api: http
`)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodeModuleIdentityMissing).Status != StatusFail {
		t.Fatal("a backend that presents no identity was accepted")
	}
}

// A consumed module with no binding is reported, but as a warning: it is
// correct when the module is reached with the viewer's own credential, which
// is what wiki does for robin, tracing and annotations. Failing it would fail
// the reference solution.
func TestAConsumedModuleWithNoBindingWarnsRatherThanFails(t *testing.T) {
	report := Inspect("wiki", conformingSolution(t))
	check := checkFor(t, report, CodeConsumedWithoutBinding)
	if check.Status != StatusWarn {
		t.Fatalf("a consumed module with no binding was %s, want warn", check.Status)
	}
	if !strings.Contains(check.Message, "documents") {
		t.Fatalf("the warning does not name the module: %q", check.Message)
	}
	if !report.Conforms() {
		t.Fatal("a warning was treated as a failure")
	}
}

// THE MISSING-PERMISSION CLASS IS CAUGHT, and it is core that catches it: a
// role may grant another module's resource kind only when one of the
// contract's bindings spells that scope out, or it is within the module's own
// ceilings. Running the contract through core's loader is what surfaces it —
// which is the whole reason this package reads every document with the reader
// that owns it instead of parsing YAML itself.
func TestARoleGrantingAnUnbackedScopeIsReported(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), `schema: codefly/module-contract/v1
principal: wiki
owner_aware: true
namespaces: []
queues: []
scope_ceilings:
    - resource_kind: wiki.page
      actions: [read, write]
roles:
    - name: installer
      scopes:
          - resource_kind: wiki.page
            actions: [read, write]
          - resource_kind: documents
            actions: [read]
bindings: []
`)
	report := Inspect("wiki", dir)
	check := checkFor(t, report, CodeContractUnreadable)
	if check.Status != StatusFail {
		t.Fatalf("a role granting a scope no binding backs was %s, want fail", check.Status)
	}
	if !strings.Contains(check.Message, "documents:read") {
		t.Fatalf("the failure does not name the unbacked grant: %q", check.Message)
	}
}

// A shell declared with no app behind it is the state conforming the fleet
// created: the manifest was added, the page was never written. It warns rather
// than fails — the solution is correctly shaped and nothing breaks until
// someone runs the shell — but it is reported, because a declared service with
// no source is invisible to every other reading.
func TestAPageShellWithNoAppWarns(t *testing.T) {
	dir := conformingSolution(t)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodePageShellHasNoApp).Status != StatusWarn {
		t.Fatal("a shell with no app behind it was not reported")
	}
	if !report.Conforms() {
		t.Fatal("a warning was treated as a failure")
	}

	// With an app beside it, the finding goes away.
	write(t, filepath.Join(dir, "services", PageService, "package.json"), `{"name":"web","private":true}`)
	if checkFor(t, Inspect("wiki", dir), CodePageShellHasNoApp).Status != StatusOK {
		t.Fatal("a shell with a real app was still reported")
	}
}
