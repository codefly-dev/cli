package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
)

func strptr(s string) *string { return &s }

// The solution verb delegates to runServiceCommand, which folds --naming-scope
// into env.NamingScope (and thus every port hash) — but only if the flag is
// actually registered on SolutionCmd, so a solution can boot on a disjoint port
// set in parallel with another running stack. Both port-isolation flags must
// also describe the same mechanism identically to their ServiceCmd twins, or
// `--help` documents one flag two different ways.
func TestSolutionCommandExposesPortIsolationFlags(t *testing.T) {
	for _, name := range []string{"naming-scope", "temporary-ports"} {
		solutionFlag := SolutionCmd.Flags().Lookup(name)
		if solutionFlag == nil {
			t.Fatalf("run solution has no --%s flag", name)
		}
		serviceFlag := ServiceCmd.Flags().Lookup(name)
		if serviceFlag == nil {
			t.Fatalf("run service has no --%s flag", name)
		}
		if solutionFlag.Usage != serviceFlag.Usage {
			t.Fatalf("--%s help diverges: solution=%q service=%q", name, solutionFlag.Usage, serviceFlag.Usage)
		}
	}
}

// A solution composes the saas host, which declares its own service-entry
// (frontend). The solution root must be the workspace's own `path: .` module,
// not the composed dependency — otherwise resolveSolutionEntry sees two
// service-entries and reports an ambiguous root.
func TestSolutionRootRef(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace *resources.Workspace
		want      string // "" means nil expected
	}{
		{
			name: "self identified by path: .",
			workspace: &resources.Workspace{
				Name: "lastlogin-go",
				Modules: []*resources.ModuleReference{
					{Name: "lastlogin-go", PathOverride: strptr(".")},
					{Name: "saas-starter", PathOverride: strptr("../../../module-saas-starter/module")},
				},
			},
			want: "lastlogin-go",
		},
		{
			name: "path: . wins even when listed after the dependency",
			workspace: &resources.Workspace{
				Name: "wiki",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
					{Name: "documents"},
					{Name: "wiki", PathOverride: strptr(".")},
				},
			},
			want: "wiki",
		},
		{
			name: "falls back to name == workspace when no explicit path: .",
			workspace: &resources.Workspace{
				Name: "lastlogin-python",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
					{Name: "lastlogin-python"},
				},
			},
			want: "lastlogin-python",
		},
		{
			name: "no self module present",
			workspace: &resources.Workspace{
				Name: "orphan",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
				},
			},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := solutionRootRef(tc.workspace)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("expected nil root, got %q", got.Name)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected root %q, got nil", tc.want)
			}
			if got.Name != tc.want {
				t.Fatalf("expected root %q, got %q", tc.want, got.Name)
			}
		})
	}
}

// A solution declaring api.consumes must boot its backend with
// CODEFLY__API_CONSUMES populated: the solution runtime reads it to register
// each consumed module's upstream with the gateway, so an absent variable
// silently leaves every consumed route unrouted.
func TestSolutionEntryConsumes(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	consumed, value, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
	if err != nil {
		t.Fatalf("solutionEntryConsumes: %v", err)
	}
	want := manifest.ConsumedAPI{
		ID: "documents", Module: "documents", Service: "api",
		Endpoint: "connect", Protocol: "connect", As: "documents",
	}
	if len(consumed) != 1 || consumed[0] != want {
		t.Fatalf("consumed APIs mismatch: got %+v, want [%+v]", consumed, want)
	}
	decoded, err := manifest.ParseConsumedAPIs(value)
	if err != nil {
		t.Fatalf("cannot decode %s value %q: %v", manifest.APIConsumesEnvironmentVariable, value, err)
	}
	if len(decoded) != 1 || decoded[0] != want {
		t.Fatalf("env value does not round-trip: got %+v", decoded)
	}
}

// The manifest at the workspace root describes the workspace's own module.
// Injecting it into any other service binds one solution's consumes to another
// backend, so every non-entry target must come back empty.
func TestSolutionEntryConsumesOnlyTargetsTheSelfRootEntry(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	for _, tc := range []struct {
		name      string
		workspace *resources.Workspace
		module    *resources.Module
		service   *resources.Service
	}{
		{
			name:      "a non-entry service of the solution root",
			workspace: wikiWorkspace(dir),
			module:    wikiModule(),
			service:   wikiService("worker"),
		},
		{
			name:      "a same-named service in a composed module",
			workspace: wikiWorkspace(dir),
			module:    &resources.Module{Name: "documents", ServiceEntry: "backend"},
			service:   wikiService("backend"),
		},
		{
			name: "no self-root module, so the entry came from a composed module",
			workspace: workspaceAt(dir, &resources.Workspace{
				Name: "orphan",
				Modules: []*resources.ModuleReference{
					{Name: "saas-starter", PathOverride: strptr("../saas/module")},
				},
			}),
			module:  &resources.Module{Name: "saas-starter", ServiceEntry: "backend"},
			service: wikiService("backend"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumed, value, err := solutionEntryConsumes(tc.workspace, tc.module, tc.service)
			if err != nil {
				t.Fatalf("solutionEntryConsumes: %v", err)
			}
			if len(consumed) != 0 || value != "" {
				t.Fatalf("expected no injection, got %+v / %q", consumed, value)
			}
		})
	}
}

// Solutions that federate nothing must run exactly as before.
func TestSolutionEntryConsumesNoOps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string // "" means write no manifest file
	}{
		{name: "no solution manifest"},
		{name: "no api.consumes", manifest: solutionManifestWithoutConsumes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.manifest != "" {
				writeSolutionManifest(t, dir, tc.manifest)
			}
			consumed, value, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
			if err != nil {
				t.Fatalf("solutionEntryConsumes: %v", err)
			}
			if len(consumed) != 0 || value != "" {
				t.Fatalf("expected no injection, got %+v / %q", consumed, value)
			}
		})
	}
}

// Running a solution must not require the whole manifest schema: a field from a
// newer core, or a rule unrelated to federation, cannot be allowed to make the
// solution unrunnable. Only the api.consumes projection is load-bearing here.
func TestSolutionEntryConsumesToleratesUnrelatedSchemaDrift(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest string
	}{
		{"field from a newer core", solutionManifestWithConsumes + "\nfuture_field:\n  nested: true\n"},
		{"unknown key inside a consumes entry", strings.Replace(solutionManifestWithConsumes, "      as: documents", "      as: documents\n      timeout: 30s", 1)},
		{"schema version this CLI predates", strings.Replace(solutionManifestWithConsumes, "codefly.solution-manifest/v0", "codefly.solution-manifest/v1", 1)},
		{"agent block that would fail strict validation", strings.Replace(solutionManifestWithConsumes, "  version: 0.1.0", "  version: latest", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSolutionManifest(t, dir, tc.manifest)
			consumed, value, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
			if err != nil {
				t.Fatalf("schema drift must not block the run: %v", err)
			}
			if len(consumed) != 1 || value == "" {
				t.Fatalf("expected the consumes projection to survive, got %+v / %q", consumed, value)
			}
		})
	}
}

// A half-bound consumes entry names a module but no service or endpoint, which
// projects into a CODEFLY__ENDPOINT key built from empty segments. The lenient
// decode skips manifest.Validate, so this is checked here instead.
func TestSolutionEntryConsumesRejectsPartialBinding(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, "      service: api\n", "", 1))

	if _, _, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend")); err == nil {
		t.Fatal("expected an error for a partially bound api.consumes entry")
	}
}

// One facade prefix is one registration secret, so two entries claiming the same
// prefix would hand the services of two different modules one credential — either
// could then mint the other's service-principal work context. manifest.Validate
// catches this for sync and package; the run's lenient decode skips it, so the
// run path is the one that must not let the typo through.
func TestSolutionEntryConsumesRejectsADuplicatedFacadePrefix(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, `lifecycle:`, `    - id: archives
      protocol: connect
      module: archives
      service: api
      endpoint: connect
      as: documents
lifecycle:`, 1))

	_, _, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
	if err == nil {
		t.Fatal("expected an error for two api.consumes entries claiming one facade prefix")
	}
	if !strings.Contains(err.Error(), "documents") {
		t.Fatalf("error %q does not name the duplicated prefix", err)
	}
}

// A prefix the registrar cannot parse is caught on the manifest, not at the
// registrar's startup. Both shapes below reach the registrar as a malformed
// `prefix:sha256hex` declaration — the separator splits the entry in the wrong
// place, the uppercase label fails the identity pattern — and it refuses to start
// on either, taking the composition with it and blaming a credential.
func TestSolutionEntryConsumesRejectsAnUnroutableFacadePrefix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
	}{
		{name: "carries the entry separator", prefix: "doc:v1"},
		{name: "carries the projection separator", prefix: "doc,v1"},
		{name: "not lowercase", prefix: "Documents"},
		{name: "underscore is not a routing label", prefix: "doc_store"},
		{name: "cannot end on a dash", prefix: "documents-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes,
				"as: documents", "as: "+tc.prefix, 1))

			_, _, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend"))
			if err == nil {
				t.Fatalf("expected an error for the facade prefix %q", tc.prefix)
			}
			if !strings.Contains(err.Error(), tc.prefix) {
				t.Fatalf("error %q does not name the offending prefix", err)
			}
		})
	}
}

// YAML that does not parse at all is a genuine boundary failure: booting a
// backend that silently federates nothing is the bug this injection exists to
// prevent.
func TestSolutionEntryConsumesRejectsUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, "api:\n\tconsumes: [oops\n")

	if _, _, err := solutionEntryConsumes(wikiWorkspace(dir), wikiModule(), wikiService("backend")); err == nil {
		t.Fatal("expected an error for an unparseable solution manifest")
	}
}

func wikiWorkspace(dir string) *resources.Workspace {
	return workspaceAt(dir, &resources.Workspace{
		Name:    "wiki",
		Modules: []*resources.ModuleReference{{Name: "wiki", PathOverride: strptr(".")}},
	})
}

func workspaceAt(dir string, workspace *resources.Workspace) *resources.Workspace {
	workspace.WithDir(dir)
	return workspace
}

func wikiModule() *resources.Module {
	return &resources.Module{Name: "wiki", ServiceEntry: "backend"}
}

func wikiService(name string) *resources.Service {
	return &resources.Service{Name: name}
}

func writeSolutionManifest(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(content), 0o600); err != nil {
		t.Fatalf("cannot write %s: %v", manifest.FileName, err)
	}
}

const solutionManifestWithoutConsumes = `
schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: wiki
  version: 0.1.0
api:
  exposes:
    - id: gateway
      protocol: http
lifecycle:
  create: true
`

const solutionManifestWithConsumes = `
schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  kind: codefly:solution
  publisher: codefly.dev
  name: wiki
  version: 0.1.0
api:
  exposes:
    - id: gateway
      protocol: http
  consumes:
    - id: documents
      protocol: connect
      module: documents
      service: api
      endpoint: connect
      as: documents
lifecycle:
  create: true
`

// The two halves of the exchange must fit each other: accounts authorizes a
// module by comparing sha256 of the secret it presented against the digest
// declared for that prefix, so a provisioning that does not pair digest to
// plaintext, prefix for prefix, refuses every mint.
func TestProvisionModuleRegistrationSecretsPairsDigestToPlaintext(t *testing.T) {
	provisioned := provisionModuleRegistrationSecrets([]manifest.ConsumedAPI{
		{ID: "documents", As: "documents"},
		{ID: "billing", As: "billing"},
	})
	if got := provisioned.prefixes; !reflect.DeepEqual(got, []string{"documents", "billing"}) {
		t.Fatalf("provisioned prefixes = %v", got)
	}

	secrets := parsePairs(t, provisioned.registrationSecrets())
	digests := parsePairs(t, provisioned.registrationDigests())
	identityDigests := parsePairs(t, provisioned.identityDigests())
	if len(secrets) != 2 || len(digests) != 2 || len(identityDigests) != 2 {
		t.Fatalf("expected 2 entries per half, got %d secrets / %d registration digests / %d identity digests",
			len(secrets), len(digests), len(identityDigests))
	}
	for prefix, secret := range secrets {
		want := sha256.Sum256([]byte(secret))
		if got := digests[prefix]; got != hex.EncodeToString(want[:]) {
			t.Errorf("registration digest for %q = %q, want sha256 of the provisioned secret", prefix, got)
		}
		identity := provisioned.byPrefix[prefix].identity
		wantIdentity := sha256.Sum256([]byte(identity))
		if got := identityDigests[prefix]; got != hex.EncodeToString(wantIdentity[:]) {
			t.Errorf("identity digest for %q = %q, want sha256 of the module's own secret", prefix, got)
		}
		// The registrar splits an entry on its first ":" and the whole projection
		// on ",", so a secret carrying either would corrupt the declaration.
		if strings.ContainsAny(secret, ":,") || strings.ContainsAny(identity, ":,") {
			t.Errorf("a secret for %q contains a separator: %q / %q", prefix, secret, identity)
		}
	}
}

// Every secret one run mints is a distinct value: the two a prefix carries, and
// the ones every other prefix carries. Reusing any of them would let the holder
// present a credential it was not issued, which is the impersonation this split
// removes.
func TestProvisionModuleRegistrationSecretsMintsIndependentRegistrationAndIdentitySecrets(t *testing.T) {
	provisioned := provisionModuleRegistrationSecrets([]manifest.ConsumedAPI{
		{ID: "documents", As: "documents"},
		{ID: "billing", As: "billing"},
	})
	seen := map[string]string{}
	for _, prefix := range provisioned.prefixes {
		secrets := provisioned.byPrefix[prefix]
		for kind, secret := range map[string]string{
			"registration": secrets.registration,
			"identity":     secrets.identity,
		} {
			if secret == "" {
				t.Fatalf("no %s secret provisioned for %q", kind, prefix)
			}
			if owner, repeated := seen[secret]; repeated {
				t.Errorf("the %s secret for %q is also %s", kind, prefix, owner)
			}
			seen[secret] = kind + " of " + prefix
		}
	}
}

// A secret is rotated per run: two runs of the same solution must not share one,
// so a secret read out of one run's process environment cannot register in the
// next.
func TestProvisionModuleRegistrationSecretsRotatePerRun(t *testing.T) {
	consumed := []manifest.ConsumedAPI{{ID: "documents", As: "documents"}}
	first := provisionModuleRegistrationSecrets(consumed)
	second := provisionModuleRegistrationSecrets(consumed)
	if first.byPrefix["documents"].registration == second.byPrefix["documents"].registration {
		t.Errorf("two runs provisioned the same registration secret: %q", first.byPrefix["documents"].registration)
	}
	if first.byPrefix["documents"].identity == second.byPrefix["documents"].identity {
		t.Errorf("two runs provisioned the same identity secret: %q", first.byPrefix["documents"].identity)
	}
}

func TestProvisionModuleRegistrationSecretsSkipsUnfederatableEntries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consumed []manifest.ConsumedAPI
		want     []string
	}{
		{
			// The runtime refuses to invent a prefix for an entry with no facade
			// entry-point, so it never registers and needs no credential.
			name:     "no facade entry-point",
			consumed: []manifest.ConsumedAPI{{ID: "documents"}},
		},
		{
			// The registrar rejects a projection declaring one prefix twice, which
			// would leave the whole composition unable to federate anything.
			name: "repeated facade",
			consumed: []manifest.ConsumedAPI{
				{ID: "documents.read", As: "documents"},
				{ID: "documents.write", As: "documents"},
			},
			want: []string{"documents"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provisioned := provisionModuleRegistrationSecrets(tc.consumed)
			if len(tc.want) == 0 {
				if provisioned != nil {
					t.Fatalf("provisioned %v for entries that can never register", provisioned.prefixes)
				}
				return
			}
			if !reflect.DeepEqual(provisioned.prefixes, tc.want) {
				t.Fatalf("provisioned prefixes = %v, want %v", provisioned.prefixes, tc.want)
			}
			if len(parsePairs(t, provisioned.registrationDigests())) != len(tc.want) {
				t.Fatalf("registration digests %q do not match prefixes %v", provisioned.registrationDigests(), tc.want)
			}
			if len(parsePairs(t, provisioned.identityDigests())) != len(tc.want) {
				t.Fatalf("identity digests %q do not match prefixes %v", provisioned.identityDigests(), tc.want)
			}
		})
	}
}

// The registrar is found by the configuration group it depends on, not by a
// service name, so no host's naming is baked into the CLI. Loaded from a real
// workspace on disk: the dependency lives in service.codefly.yaml and only the
// loader knows how to get it from there.
func TestFederationRegistrars(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")

	got := federationRegistrars(ctx, workspace)
	if !reflect.DeepEqual(got, []string{"host/accounts"}) {
		t.Fatalf("federationRegistrars = %v, want [host/accounts] — the only service declaring %q",
			got, federationConfigurationGroup)
	}
}

// A workspace with no registrar must still run: federation stays dead, but the
// solution serves its own routes. Composed modules that do not resolve locally
// are likewise skipped rather than failing the run.
func TestFederationRegistrarsToleratesAWorkspaceWithout(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = append(workspace.Modules,
		&resources.ModuleReference{Name: "missing", PathOverride: strptr("modules/missing")})

	got := federationRegistrars(ctx, workspace)
	if !reflect.DeepEqual(got, []string{"host/accounts"}) {
		t.Fatalf("federationRegistrars = %v, want [host/accounts] despite an unresolvable module", got)
	}
}

// The whole injection, end to end on a real workspace: the consuming backend
// gets the projection plus its plaintext secrets, and the registrar gets the
// matching digests — each keyed by the module-qualified unique so it lands on
// exactly one service.
func TestSolutionDerivedOverridesProvisionsBothHalves(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	module := &resources.Module{Name: "wiki", ServiceEntry: "backend"}

	derived, err := solutionDerivedRunInputs(ctx, workspace, module, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("solutionDerivedRunInputs: %v", err)
	}
	overrides := derived.overrides

	backend := overrides["wiki/backend"]
	if backend[manifest.APIConsumesEnvironmentVariable] == "" {
		t.Errorf("backend did not receive %s", manifest.APIConsumesEnvironmentVariable)
	}
	secrets := parsePairs(t, backend[moduleRegistrationSecretsEnvironmentVariable])
	if len(secrets) != 1 || secrets["documents"] == "" {
		t.Fatalf("backend %s = %q, want one documents entry",
			moduleRegistrationSecretsEnvironmentVariable, backend[moduleRegistrationSecretsEnvironmentVariable])
	}

	// The digests ride the federation workspace configuration group — the carrier
	// the registrar reads by contract — not a raw process variable it would only
	// pick up through an incidental os.Getenv fallback.
	declared := derived.workspaceConfigurations[federationConfigurationGroup]
	digests := parsePairs(t, declared[moduleRegistrationSecretsKey])
	want := sha256.Sum256([]byte(secrets["documents"]))
	if digests["documents"] != hex.EncodeToString(want[:]) {
		t.Errorf("registrar digest for documents = %q, does not match the backend's secret", digests["documents"])
	}
	identityDigests := parsePairs(t, declared[moduleIdentitySecretsKey])
	wantIdentity := sha256.Sum256([]byte(derived.overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable]))
	if identityDigests["documents"] != hex.EncodeToString(wantIdentity[:]) {
		t.Errorf("registrar identity digest for documents = %q, does not match the module's own secret", identityDigests["documents"])
	}
	// The raw secrets belong only to the ends that present them.
	for _, key := range []string{moduleRegistrationSecretsKey, moduleIdentitySecretsKey} {
		if strings.Contains(declared[key], secrets["documents"]) {
			t.Errorf("registrar received the plaintext secret under %s; it must hold only digests", key)
		}
	}
	// Nothing about the registrar rides the per-service override seam any more.
	for service, values := range overrides {
		switch service {
		case "wiki/backend", "documents/api", "documents/worker":
			continue
		}
		t.Errorf("service %s received a derived process override %v; the digest belongs on the configuration group", service, values)
	}
}

// A consumed module mints its service-principal work context with an identity
// secret of its own, so every service of that module boots with one singular
// plaintext — and with nothing else. Handing a module the whole map would give it
// its siblings' credentials; handing the backend this secret would let the
// consumer mint the provider's work context, which is what the two digests exist
// to prevent.
func TestSolutionDerivedOverridesProvisionsTheConsumedModulesOwnSecret(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	module := &resources.Module{Name: "wiki", ServiceEntry: "backend"}

	derived, err := solutionDerivedRunInputs(ctx, workspace, module, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("solutionDerivedRunInputs: %v", err)
	}
	backend := derived.overrides["wiki/backend"]
	secrets := parsePairs(t, backend[moduleRegistrationSecretsEnvironmentVariable])
	identity := derived.overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable]
	if identity == "" {
		t.Fatal("documents/api received no identity secret")
	}
	if identity == secrets["documents"] {
		t.Error("the module's identity secret is the secret the backend registers with; the consumer can mint the provider's work context")
	}
	for _, value := range backend {
		if strings.Contains(value, identity) {
			t.Errorf("the backend holds the module's identity secret in %q", value)
		}
	}
	for _, unique := range []string{"documents/api", "documents/worker"} {
		values := derived.overrides[unique]
		if got := values[moduleRegistrationSecretEnvironmentVariable]; got != identity {
			t.Errorf("%s %s = %q, want the identity secret minted for documents",
				unique, moduleRegistrationSecretEnvironmentVariable, got)
		}
		if _, leaked := values[moduleRegistrationSecretsEnvironmentVariable]; leaked {
			t.Errorf("%s received the whole %s map; a module holds one identity",
				unique, moduleRegistrationSecretsEnvironmentVariable)
		}
	}
}

// A consumed module the workspace does not carry — not composed, or pinned to an
// artifact no materialization resolved — has no service to inject into. The run
// still boots, but it must not pass in silence: the preceding line reports the
// backend half as provisioned, and a module that quietly receives nothing looks
// exactly like one whose exchange is broken.
func TestConsumedModuleSecretOverridesReportsAModuleItCannotResolve(t *testing.T) {
	ctx := context.Background()
	consumed := []manifest.ConsumedAPI{
		{ID: "documents", Module: "documents", Service: "api", Endpoint: "connect", As: "documents"},
	}
	provisioned := provisionModuleRegistrationSecrets(consumed)

	for _, tc := range []struct {
		name      string
		workspace func() *resources.Workspace
		reason    string
	}{
		{
			name: "not referenced by the workspace",
			workspace: func() *resources.Workspace {
				workspace := loadTestWorkspace(t, "testdata/solution-federation")
				workspace.Modules = slices.DeleteFunc(workspace.Modules,
					func(ref *resources.ModuleReference) bool { return ref.Name == "documents" })
				return workspace
			},
			reason: "references no such module",
		},
		{
			name: "referenced but unloadable",
			workspace: func() *resources.Workspace {
				workspace := loadTestWorkspace(t, "testdata/solution-federation")
				for _, ref := range workspace.Modules {
					if ref.Name == "documents" {
						ref.PathOverride = strptr("modules/gone")
					}
				}
				return workspace
			},
			reason: "cannot load module",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			injection := consumedModuleSecretOverrides(ctx, tc.workspace(), consumed, provisioned, []string{"host/accounts"})
			if len(injection.overrides) != 0 {
				t.Errorf("injected %v for a module with no service", injection.overrides)
			}
			if len(injection.provisioned) != 0 {
				t.Errorf("reported %v as provisioned", injection.provisioned)
			}
			if len(injection.unresolved) != 1 || !strings.Contains(injection.unresolved[0], "documents") {
				t.Fatalf("unresolved = %v, want the documents module and why it has no service", injection.unresolved)
			}
			if !strings.Contains(injection.unresolved[0], tc.reason) {
				t.Errorf("unresolved %q does not say %q; an operator cannot tell an uncomposed module from a broken checkout",
					injection.unresolved[0], tc.reason)
			}
		})
	}
}

// The backend half is independent of the module half: a consumed module the
// workspace cannot resolve must not cost the solution the secrets it presents
// itself, nor leave an override on a service that is not in the run.
func TestSolutionDerivedRunInputsKeepsTheBackendHalfWhenAModuleIsUnresolvable(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "documents" })

	derived, err := solutionDerivedRunInputs(ctx, workspace,
		&resources.Module{Name: "wiki", ServiceEntry: "backend"}, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("solutionDerivedRunInputs: %v", err)
	}
	for service, values := range derived.overrides {
		if service == "wiki/backend" {
			continue
		}
		t.Errorf("service %s received %v for a module the workspace cannot load", service, values)
	}
	if derived.overrides["wiki/backend"][moduleRegistrationSecretsEnvironmentVariable] == "" {
		t.Error("skipping the module also dropped the backend's own registration secrets")
	}
}

// A consumed module that itself holds the federation digests is the authority the
// exchange runs against, not a module that authenticates to it. Injecting the
// plaintext there would hand the registrar the preimage of the digest it compares
// against — the separation the digest carrier exists to create.
func TestConsumedModuleSecretOverridesExcludesTheRegistrarsOwnModule(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	consumed := []manifest.ConsumedAPI{
		{ID: "accounts", Module: "host", Service: "accounts", Endpoint: "connect", As: "accounts"},
		{ID: "documents", Module: "documents", Service: "api", Endpoint: "connect", As: "documents"},
	}
	provisioned := provisionModuleRegistrationSecrets(consumed)

	injection := consumedModuleSecretOverrides(ctx, workspace, consumed, provisioned, federationRegistrars(ctx, workspace))

	for _, unique := range []string{"host/accounts", "host/gateway"} {
		if got := injection.overrides[unique][moduleRegistrationSecretEnvironmentVariable]; got != "" {
			t.Errorf("%s received the plaintext %q whose digest its own module holds", unique, got)
		}
	}
	if !reflect.DeepEqual(injection.registrars, []string{"host"}) {
		t.Errorf("registrars = %v, want [host] reported as deliberately excluded", injection.registrars)
	}
	// The module that does authenticate is unaffected.
	if injection.overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable] != provisioned.byPrefix["documents"].identity {
		t.Error("excluding the registrar's module also dropped the consumed module's own secret")
	}
}

// A solution that federates nothing must run exactly as before: no secrets
// provisioned, and no override on any host service.
func TestSolutionDerivedOverridesNoOpsWithoutConsumes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithoutConsumes)

	derived, err := solutionDerivedRunInputs(ctx, wikiWorkspace(dir), wikiModule(), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("solutionDerivedRunInputs: %v", err)
	}
	if derived.overrides != nil || derived.workspaceConfigurations != nil {
		t.Fatalf("expected no derived inputs, got %+v", derived)
	}
}

// A composition whose host declares no federation group cannot authorize a
// mint. Handing the backend a secret regardless buys nothing but a heartbeat
// spent on an exchange that always fails; withholding it lets the runtime skip
// the module with the accurate "no registration secret provisioned" line.
func TestSolutionDerivedRunInputsWithholdsSecretsWithoutARegistrar(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	// Drop the only module whose accounts service declares the group.
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "host" })

	derived, err := solutionDerivedRunInputs(ctx, workspace,
		&resources.Module{Name: "wiki", ServiceEntry: "backend"}, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("solutionDerivedRunInputs: %v", err)
	}
	if got := derived.overrides["wiki/backend"][moduleRegistrationSecretsEnvironmentVariable]; got != "" {
		t.Errorf("provisioned a secret %q with no registrar to authorize it", got)
	}
	// Withholding is symmetric: a secret no registrar can authorize is useless to
	// the module that would present it too.
	if got := derived.overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable]; got != "" {
		t.Errorf("provisioned documents/api a secret %q with no registrar to authorize it", got)
	}
	if derived.workspaceConfigurations != nil {
		t.Errorf("declared federation values with no registrar: %+v", derived.workspaceConfigurations)
	}
	// The api.consumes projection is unrelated to federation credentials and
	// must still reach the backend.
	if derived.overrides["wiki/backend"][manifest.APIConsumesEnvironmentVariable] == "" {
		t.Error("withholding the secret also dropped the api.consumes projection")
	}
}

func loadTestWorkspace(t *testing.T, dir string) *resources.Workspace {
	t.Helper()
	absolute, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := resources.LoadWorkspaceFromDir(context.Background(), absolute)
	if err != nil {
		t.Fatalf("cannot load workspace %s: %v", dir, err)
	}
	return workspace
}

// parsePairs decodes either half of the exchange — both are comma-separated
// `prefix:value` entries.
func parsePairs(t *testing.T, raw string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		if entry == "" {
			continue
		}
		prefix, value, ok := strings.Cut(entry, ":")
		if !ok {
			t.Fatalf("entry %q is not prefix:value", entry)
		}
		out[prefix] = value
	}
	return out
}
