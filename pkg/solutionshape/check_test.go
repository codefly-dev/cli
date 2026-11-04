package solutionshape

import (
	"crypto/sha256"
	"encoding/hex"
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
scope_ceilings:
    - resource_kind: wiki.page
      actions: [read, write]
roles:
    - name: installer
      scopes:
        - resource_kind: wiki.page
          actions: [read, write]
bindings: []
`)
	// The published contract the package carries. The manifest loader checks
	// every artifact root exists on disk, and the authority derives each
	// route's method policy from this document — so the routes carry the
	// policies the contract's one role grants, and the package names the
	// document's real digest, which is what the policy reader holds it to.
	writePackagedRoutes(t, dir, conformingRoutes)
	conformingServices(t, dir)
	return dir
}

// conformingRoutes is the scaffold's published routes: one route per action its
// role grants, and one relayed module route that admits any checked caller, as
// a solution's relays do.
const conformingRoutes = `{"swagger":"2.0","basePath":"/",` +
	`"info":{"title":"wiki solution backend","version":"0.0.1"},"paths":{` +
	`"/v1/wiki/pages":{` +
	`"get":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"wiki.page","actions":["read"]}]}},` +
	`"post":{"x-codefly-authority-policy":{"requires":[{"resource_kind":"wiki.page","actions":["write"]}]}}},` +
	`"/v1/wiki/modules/documents/{path=**}":{"post":{"x-codefly-authority-policy":{"any_checked_caller":true}}}}}`

// writePackagedRoutes writes the published OpenAPI document and the package
// manifest that names it, with the digest the policy reader checks. A test
// that changes the routes calls it again.
func writePackagedRoutes(t *testing.T, dir, routes string) {
	t.Helper()
	write(t, filepath.Join(dir, "contracts", "api", "backend", "http", "openapi.json"), routes)
	sum := sha256.Sum256([]byte(routes))
	write(t, filepath.Join(dir, "module.package.codefly.yaml"), `kind: module-package
schema: codefly/module-package/v2
id: obin/wiki
version: 0.1.0
minimum-codefly-version: ">=0.1.220 <1.0.0"
artifact-roots:
    - contracts
    - services
services:
    - name: backend
      endpoints: [http]
      api-contracts:
        - endpoint: http
          kind: openapi
          package: wiki solution backend
          path: contracts/api/backend/http/openapi.json
          digest: sha256:`+hex.EncodeToString(sum[:])+`
contracts:
    composition: ">=2.0.0 <3.0.0"
`)
}

// conformingServices writes the two services the shape names.
func conformingServices(t *testing.T, dir string) {
	t.Helper()
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
    - contracts
    - services
services:
    - name: backend
      endpoints: [http]
      api-contracts:
        - endpoint: http
          kind: openapi
          package: wiki solution backend
          path: contracts/api/backend/http/openapi.json
          digest: sha256:0000000000000000000000000000000000000000000000000000000000000000
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

	// With an app where the manifest says it is, the finding goes away.
	write(t, filepath.Join(dir, "services", PageService, "code", "package.json"), `{"name":"web","private":true}`)
	if checkFor(t, Inspect("wiki", dir), CodePageShellHasNoApp).Status != StatusOK {
		t.Fatal("a shell with a real app was still reported")
	}
}

// TestAPageShellFindsItsAppWhereTheManifestSaysItIs covers the two layouts in
// use. They differ by exactly `spec.source-dir`, and a check that knew only
// one reported the other as having no app: a shell this CLI scaffolds takes
// the nextjs agent's default and gets its app under `code/`, while the
// fleet's hand-written pages sit at the service root and say `source-dir: .`.
// The first `codefly add solution` ever run reported its own freshly
// scaffolded, working shell as empty.
func TestAPageShellFindsItsAppWhereTheManifestSaysItIs(t *testing.T) {
	for _, layout := range []struct {
		name string
		// sourceDir is written into spec; empty means the key is absent, so
		// the agent's own default applies.
		sourceDir string
		appDir    string
	}{
		{"the agent's default puts the app under code/", "", "code"},
		{"source-dir . puts the app at the service root", ".", ""},
	} {
		t.Run(layout.name, func(t *testing.T) {
			dir := conformingSolution(t)
			shell := filepath.Join(dir, "services", PageService)
			if layout.sourceDir != "" {
				manifest := filepath.Join(shell, "service.codefly.yaml")
				existing, err := os.ReadFile(manifest)
				if err != nil {
					t.Fatalf("cannot read the shell's manifest: %v", err)
				}
				write(t, manifest, string(existing)+"spec:\n    source-dir: "+layout.sourceDir+"\n")
			}
			write(t, filepath.Join(shell, layout.appDir, "package.json"), `{"name":"web","private":true}`)
			if checkFor(t, Inspect("wiki", dir), CodePageShellHasNoApp).Status != StatusOK {
				t.Fatal("the shell's app was not found where the manifest says it is")
			}
		})
	}
}

// TestAContractWithNoInstallerRoleFails covers the rule the authority states
// at runtime and nothing else catches. A solution is a unit a person installs,
// so a contract offering no installer role is refused — at `runtime-run`,
// after the whole composition has booted. Seven of nine solutions in one fleet
// declared no role at all and the shape checker called them all conformant.
func TestAContractWithNoInstallerRoleFails(t *testing.T) {
	dir := conformingSolution(t)
	if checkFor(t, Inspect("wiki", dir), CodeInstallerRoleMissing).Status != StatusOK {
		t.Fatal("a contract declaring the installer role was reported")
	}

	// Strip the roles: the contract still reads, and everything else passes.
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), `schema: codefly/module-contract/v1
principal: wiki
owner_aware: true
namespaces: []
queues: []
scope_ceilings: []
bindings: []
`)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodeInstallerRoleMissing).Status != StatusFail {
		t.Fatal("a contract with no installer role was not reported")
	}
	if report.Conforms() {
		t.Fatal("a solution the authority refuses to admit was called conformant")
	}
}

// TestAPackageCarryingNoBackendContractFails. The authority derives the
// method policy of every route from the contract the package carries, so a
// package declaring none is delivered the module's ceilings and no policy —
// "no delivered method policies; declared ceilings alone do not admit a
// call". The backend boots, serves its route, declares its policy, and admits
// nothing; the only sign is a warning in the authority's log during a run.
//
// Of the nine solutions in one fleet, the two that are actually run — wiki
// and proposals — carry it and the other seven do not.
func TestAPackageCarryingNoBackendContractFails(t *testing.T) {
	dir := conformingSolution(t)
	if checkFor(t, Inspect("wiki", dir), CodeBackendContractMissing).Status != StatusOK {
		t.Fatal("a package carrying the backend's contract was reported")
	}

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
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodeBackendContractMissing).Status != StatusFail {
		t.Fatal("a package carrying no contract for the backend was not reported")
	}
	if report.Conforms() {
		t.Fatal("a solution that can admit no call to any route it serves was called conformant")
	}
}

// THE SURPLUS-PERMISSION CLASS, which is the one core cannot catch: core
// refuses a grant nothing BACKS, so every grant below is perfectly declared.
// What none of them is, is reached.
//
// The worked example is obin-ai/core-solutions' wiki, whose `installer` was
// `editor` plus a publish action: whoever installed the wiki received full
// document read and write as a side effect, and three roles rendered as three
// near-identical fifty-row tables nobody could read against each other.
func TestARoleGrantingWhatNoOperationRequiresWarns(t *testing.T) {
	for _, test := range []struct {
		name    string
		roles   string
		ceiling string
		surplus []string
	}{
		{
			name:    "an action no route spells is checked by nothing",
			ceiling: "[read, write, publish]",
			roles: `    - name: installer
      scopes:
          - resource_kind: wiki.page
            actions: [read, write, publish]`,
			// read and write are each required by a route; publish by none.
			surplus: []string{"installer grants wiki.page:publish"},
		},
		{
			name:    "every role is judged, not only the installer",
			ceiling: "[read, write, publish]",
			roles: `    - name: installer
      scopes:
          - resource_kind: wiki.page
            actions: [read]
    - name: editor
      scopes:
          - resource_kind: wiki.page
            actions: [read, publish]`,
			surplus: []string{"editor grants wiki.page:publish"},
		},
		{
			name:    "an installation-owned scope grants just as much",
			ceiling: "[read, write, publish]",
			roles: `    - name: installer
      scopes:
          - resource_kind: wiki.page
            actions: [read]
      installation_owned_scopes:
          - resource_kind: wiki.page
            actions: [publish]`,
			surplus: []string{"installer grants wiki.page:publish"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := conformingSolution(t)
			writeContract(t, dir, test.ceiling, test.roles)
			report := Inspect("wiki", dir)
			check := checkFor(t, report, CodeRoleScopeUnreached)
			if check.Status != StatusWarn {
				t.Fatalf("a surplus grant was %s, want warn", check.Status)
			}
			for _, want := range test.surplus {
				if !strings.Contains(check.Message, want) {
					t.Errorf("the warning does not name %q: %q", want, check.Message)
				}
			}
			if !report.Conforms() {
				t.Fatal("surplus authority was treated as a broken solution; it refuses no call and breaks no run")
			}
		})
	}
}

// A grant on a kind one of the contract's BINDINGS carries is never surplus:
// the module at the other end authorizes the relayed child, its policies are
// not in this directory, and the role grants the scope precisely so the child
// carries it. This is the regression test for the mistake checkInstallerRole
// records — a permissions check that "once shipped and failed wiki and
// proposals, the only two solutions known to work".
func TestABindingsKindIsNeverSurplus(t *testing.T) {
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
            actions: [read, write]
          - resource_kind: documents.collection
            actions: [read, create]
bindings:
    - id: documents
      operations: [invoke]
      audience: {from: wiki-audiences/documents-audience}
      binding_key: {from: wiki/DOCUMENTS_BINDING}
      scope_ceiling:
          invoke:
              - resource_kind: documents
                actions: [read, write]
              - resource_kind: documents.collection
                actions: [create, read]
`)
	report := Inspect("wiki", dir)
	check := checkFor(t, report, CodeRoleScopeUnreached)
	if check.Status != StatusOK {
		t.Fatalf("a binding-backed grant was reported %s: %q — the relayed child is authorized by the module at the other end, whose policies are not in this directory", check.Status, check.Message)
	}
}

// Never a vacuous pass: a question that could not be asked must not read as a
// question answered "nothing wrong". The published contract is a GENERATED
// artifact, and a tree whose contract is behind its code is exactly when that
// matters.
func TestOperationsThatCannotBeReadAreReportedRatherThanPassed(t *testing.T) {
	t.Run("a contract whose digest the package does not name", func(t *testing.T) {
		dir := conformingSolution(t)
		write(t, filepath.Join(dir, "contracts", "api", "backend", "http", "openapi.json"),
			strings.Replace(conformingRoutes, "wiki solution backend", "edited since the package named it", 1))
		report := Inspect("wiki", dir)
		if check := checkFor(t, report, CodeOperationPoliciesUnread); check.Status != StatusWarn {
			t.Fatalf("an unreadable contract was %s, want warn", check.Status)
		}
		for _, check := range report.Checks {
			if check.Code == CodeRoleScopeUnreached {
				t.Fatalf("the roles were judged against operations that could not be read: %q", check.Message)
			}
		}
	})
	t.Run("a contract carrying no operation at all", func(t *testing.T) {
		dir := conformingSolution(t)
		writePackagedRoutes(t, dir, `{"swagger":"2.0","basePath":"/","paths":{}}`)
		report := Inspect("wiki", dir)
		if check := checkFor(t, report, CodeOperationPoliciesUnread); check.Status != StatusWarn {
			t.Fatalf("an empty contract was %s, want warn", check.Status)
		}
	})
}

// The package that publishes no contract at all is already failed by
// checkBackendContract, with the remediation. Saying it a second time in
// another vocabulary helps nobody.
func TestNoPublishedContractIsNotReportedTwice(t *testing.T) {
	dir := conformingSolution(t)
	write(t, filepath.Join(dir, "module.package.codefly.yaml"), `kind: module-package
schema: codefly/module-package/v2
id: obin/wiki
version: 0.1.0
minimum-codefly-version: ">=0.1.220 <1.0.0"
artifact-roots:
    - contracts
    - services
services:
    - name: backend
      endpoints: [http]
contracts:
    composition: ">=2.0.0 <3.0.0"
`)
	report := Inspect("wiki", dir)
	if checkFor(t, report, CodeBackendContractMissing).Status != StatusFail {
		t.Fatal("a package carrying no contract was accepted")
	}
	for _, check := range report.Checks {
		if check.Code == CodeOperationPoliciesUnread || check.Code == CodeRoleScopeUnreached {
			t.Fatalf("the same missing contract was reported again as %s: %q", check.Code, check.Message)
		}
	}
}

// writeContract writes the module contract with one ceiling on wiki.page and
// the given roles, so a case in the table above changes exactly what it is
// about.
func writeContract(t *testing.T, dir, actions, roles string) {
	t.Helper()
	write(t, filepath.Join(dir, "module.contract.codefly.yaml"), `schema: codefly/module-contract/v1
principal: wiki
owner_aware: true
namespaces: []
queues: []
scope_ceilings:
    - resource_kind: wiki.page
      actions: `+actions+`
roles:
`+roles+`
bindings: []
`)
}
