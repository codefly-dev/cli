package solutionrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
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

// A solution composes the saas host, which declares its own service-entry
// (frontend). The solution root must be the workspace's own `path: .` module,
// not the composed dependency — otherwise entry resolution sees two
// service-entries and reports an ambiguous root.
func TestRootRef(t *testing.T) {
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
			got := RootRef(tc.workspace)
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
func TestEntryConsumes(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)

	consumed, value, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
	if err != nil {
		t.Fatalf("entryConsumes: %v", err)
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

// The manifest is the module's own: it is read from the module's directory and
// only for that module's service-entry. Any other service — a sibling of the
// entry, or the entry of a module whose directory ships no manifest — must come
// back empty, or one solution's consumes would bind to another backend.
func TestEntryConsumesOnlyTargetsTheModulesOwnEntry(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithConsumes)
	elsewhere := t.TempDir()

	for _, tc := range []struct {
		name    string
		module  *resources.Module
		service *resources.Service
	}{
		{
			name:    "a non-entry service of the solution module",
			module:  wikiModuleAt(dir),
			service: wikiService("worker"),
		},
		{
			name: "a same-named entry of a module shipping no manifest",
			module: func() *resources.Module {
				module := &resources.Module{Name: "documents", ServiceEntry: "backend"}
				module.WithDir(elsewhere)
				return module
			}(),
			service: wikiService("backend"),
		},
		{
			name:    "a module with no directory to ship a manifest in",
			module:  &resources.Module{Name: "wiki", ServiceEntry: "backend"},
			service: wikiService("backend"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumed, value, err := entryConsumes(tc.module, tc.service)
			if err != nil {
				t.Fatalf("entryConsumes: %v", err)
			}
			if len(consumed) != 0 || value != "" {
				t.Fatalf("expected no injection, got %+v / %q", consumed, value)
			}
		})
	}
}

// A solution composed by source and version is never the workspace's own
// module: its manifest sits in its cache checkout, not at the workspace root.
// The manifest a run reads is the module's, wherever the module is — gating it
// on the workspace root left a composed solution with no projection at all.
func TestEntryConsumesIsLocatedByTheModuleNotTheWorkspace(t *testing.T) {
	checkout := t.TempDir()
	writeSolutionManifest(t, checkout, solutionManifestWithConsumes)
	workspaceDir := t.TempDir()
	writeSolutionManifest(t, workspaceDir, solutionManifestWithoutConsumes)

	composed := &resources.Module{Name: "lastlogin", ServiceEntry: "backend"}
	composed.WithDir(checkout)
	consumed, value, err := entryConsumes(composed, wikiService("backend"))
	if err != nil {
		t.Fatalf("entryConsumes: %v", err)
	}
	if len(consumed) != 1 || consumed[0].Module != "documents" || value == "" {
		t.Fatalf("the composed module's own manifest was not read: got %+v / %q", consumed, value)
	}
}

// Solutions that federate nothing must run exactly as before.
func TestEntryConsumesNoOps(t *testing.T) {
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
			consumed, value, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
			if err != nil {
				t.Fatalf("entryConsumes: %v", err)
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
func TestEntryConsumesToleratesUnrelatedSchemaDrift(t *testing.T) {
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
			consumed, value, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
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
func TestEntryConsumesRejectsPartialBinding(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, "      service: api\n", "", 1))

	if _, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend")); err == nil {
		t.Fatal("expected an error for a partially bound api.consumes entry")
	}
}

// One facade prefix is one registration secret, so two entries claiming the same
// prefix would hand the services of two different modules one credential — either
// could then mint the other's service-principal work context. manifest.Validate
// catches this for sync and package; the run's lenient decode skips it, so the
// run path is the one that must not let the typo through.
func TestEntryConsumesRejectsADuplicatedFacadePrefix(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, strings.Replace(solutionManifestWithConsumes, `lifecycle:`, `    - id: archives
      protocol: connect
      module: archives
      service: api
      endpoint: connect
      as: documents
lifecycle:`, 1))

	_, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
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
func TestEntryConsumesRejectsAnUnroutableFacadePrefix(t *testing.T) {
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

			_, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend"))
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
func TestEntryConsumesRejectsUnparseableManifest(t *testing.T) {
	dir := t.TempDir()
	writeSolutionManifest(t, dir, "api:\n\tconsumes: [oops\n")

	if _, _, err := entryConsumes(wikiModuleAt(dir), wikiService("backend")); err == nil {
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

// wikiModuleAt is the wiki solution module as a run loads it: with the directory
// its solution manifest sits in.
func wikiModuleAt(dir string) *resources.Module {
	module := &resources.Module{Name: "wiki", ServiceEntry: "backend"}
	module.WithDir(dir)
	return module
}

// wikiModuleIn is the wiki module of a loaded workspace where it is the `path: .`
// module, so its directory is the workspace's.
func wikiModuleIn(workspace *resources.Workspace) *resources.Module {
	return wikiModuleAt(workspace.Dir())
}

// entryConsumes is the api.consumes half of what DerivedRunInputs reads from the
// entry manifest: the projection and its CODEFLY__API_CONSUMES value.
func entryConsumes(module *resources.Module, service *resources.Service) ([]manifest.ConsumedAPI, string, error) {
	solutionManifest, err := entryManifest(module, service)
	if err != nil || solutionManifest == nil {
		return nil, "", err
	}
	consumed := solutionManifest.ConsumedAPIs()
	if len(consumed) == 0 {
		return nil, "", nil
	}
	return consumed, solutionManifest.ConsumedAPIsEnvValue(), nil
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
func TestDerivedRunInputsProvisionsBothHalves(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	module := wikiModuleIn(workspace)

	derived, err := DerivedRunInputs(ctx, workspace, module, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	overrides := derived.Overrides

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
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	digests := parsePairs(t, declared[moduleRegistrationSecretsKey])
	want := sha256.Sum256([]byte(secrets["documents"]))
	if digests["documents"] != hex.EncodeToString(want[:]) {
		t.Errorf("registrar digest for documents = %q, does not match the backend's secret", digests["documents"])
	}
	identityDigests := parsePairs(t, declared[moduleIdentitySecretsKey])
	wantIdentity := sha256.Sum256([]byte(derived.Overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable]))
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
func TestDerivedRunInputsProvisionsTheConsumedModulesOwnSecret(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	module := wikiModuleIn(workspace)

	derived, err := DerivedRunInputs(ctx, workspace, module, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	backend := derived.Overrides["wiki/backend"]
	secrets := parsePairs(t, backend[moduleRegistrationSecretsEnvironmentVariable])
	identity := derived.Overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable]
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
		values := derived.Overrides[unique]
		if values["CODEFLY__MODULE_IDENTITY_PREFIX"] != "documents" {
			t.Errorf("%s received no canonical identity prefix", unique)
		}
		if values["CODEFLY__MODULE_IDENTITY_SECRET"] != identity {
			t.Errorf("%s canonical identity differs from the provisioned identity", unique)
		}
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
func TestDerivedRunInputsKeepsTheBackendHalfWhenAModuleIsUnresolvable(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "documents" })

	derived, err := DerivedRunInputs(ctx, workspace,
		wikiModuleIn(workspace), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	for service, values := range derived.Overrides {
		if service == "wiki/backend" {
			continue
		}
		t.Errorf("service %s received %v for a module the workspace cannot load", service, values)
	}
	if derived.Overrides["wiki/backend"][moduleRegistrationSecretsEnvironmentVariable] == "" {
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
		if injection.overrides[unique]["CODEFLY__MODULE_IDENTITY_SECRET"] != "" || injection.overrides[unique]["CODEFLY__MODULE_IDENTITY_PREFIX"] != "" {
			t.Errorf("%s received a module identity carrier", unique)
		}
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

// Identity follows the declared federation alias, not a module's directory name.
// Separate module services must never receive another prefix's identity.
func TestConsumedModuleSecretOverridesBindsIdentityToDeclaredPrefix(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	consumed := []manifest.ConsumedAPI{
		{ID: "documents", Module: "documents", Service: "api", Endpoint: "connect", As: "knowledge"},
		{ID: "consumer", Module: "wiki", Service: "backend", Endpoint: "connect", As: "consumer"},
	}
	provisioned := provisionModuleRegistrationSecrets(consumed)
	injection := consumedModuleSecretOverrides(ctx, workspace, consumed, provisioned, []string{"host/accounts"})
	for unique, prefix := range map[string]string{
		"documents/api": "knowledge", "documents/worker": "knowledge", "wiki/backend": "consumer",
	} {
		values := injection.overrides[unique]
		if values["CODEFLY__MODULE_IDENTITY_PREFIX"] != prefix {
			t.Errorf("%s did not receive its declared identity prefix %s", unique, prefix)
		}
		identity := provisioned.byPrefix[prefix].identity
		if values["CODEFLY__MODULE_IDENTITY_SECRET"] != identity || values[moduleRegistrationSecretEnvironmentVariable] != identity {
			t.Errorf("%s identity carriers do not match its own provisioned identity", unique)
		}
		for otherPrefix, other := range provisioned.byPrefix {
			for key, value := range values {
				if value == other.registration || (otherPrefix != prefix && value == other.identity) {
					t.Errorf("%s received another principal's credential under %s", unique, key)
				}
			}
		}
	}
}

// A solution that federates nothing, in a workspace with no registrar to admit
// it, must run exactly as before: no projection, no secret, no override on any
// service and no configuration declared.
func TestDerivedRunInputsNoOpsWithoutConsumesOrRegistrar(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeSolutionManifest(t, dir, solutionManifestWithoutConsumes)

	derived, err := DerivedRunInputs(ctx, wikiWorkspace(dir), wikiModuleAt(dir), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	if derived.Overrides != nil || derived.WorkspaceConfigurations != nil {
		t.Fatalf("expected no derived inputs, got %+v", derived)
	}
}

// A composition whose host declares no federation group cannot authorize a
// mint. Handing the backend a secret regardless buys nothing but a heartbeat
// spent on an exchange that always fails; withholding it lets the runtime skip
// the module with the accurate "no registration secret provisioned" line.
func TestDerivedRunInputsWithholdsSecretsWithoutARegistrar(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	// Drop the only module whose accounts service declares the group.
	workspace.Modules = slices.DeleteFunc(workspace.Modules,
		func(ref *resources.ModuleReference) bool { return ref.Name == "host" })

	derived, err := DerivedRunInputs(ctx, workspace,
		wikiModuleIn(workspace), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	if got := derived.Overrides["wiki/backend"][moduleRegistrationSecretsEnvironmentVariable]; got != "" {
		t.Errorf("provisioned a secret %q with no registrar to authorize it", got)
	}
	if got := derived.Overrides["wiki/backend"][solutionRegistrationSecretEnvironmentVariable]; got != "" {
		t.Errorf("provisioned the solution's own secret %q with no registrar to admit it", got)
	}
	// Withholding is symmetric: a secret no registrar can authorize is useless to
	// the module that would present it too.
	if got := derived.Overrides["documents/api"][moduleRegistrationSecretEnvironmentVariable]; got != "" {
		t.Errorf("provisioned documents/api a secret %q with no registrar to authorize it", got)
	}
	for unique, values := range derived.Overrides {
		for _, key := range []string{"CODEFLY__MODULE_IDENTITY_PREFIX", "CODEFLY__MODULE_IDENTITY_SECRET"} {
			if _, exists := values[key]; exists {
				t.Errorf("%s received %s with no registrar", unique, key)
			}
		}
	}
	if derived.WorkspaceConfigurations != nil {
		t.Errorf("declared federation values with no registrar: %+v", derived.WorkspaceConfigurations)
	}
	// The api.consumes projection is unrelated to federation credentials and
	// must still reach the backend.
	if derived.Overrides["wiki/backend"][manifest.APIConsumesEnvironmentVariable] == "" {
		t.Error("withholding the secret also dropped the api.consumes projection")
	}
	// Withholding silently is the failure this reporting exists to prevent: the
	// composition looks wired and federates nothing.
	var warned bool
	for _, note := range derived.Notes {
		if note.Warning && strings.Contains(note.Message, federationConfigurationGroup) {
			warned = true
			if !strings.Contains(note.Message, "solution wiki") {
				t.Errorf("the warning %q does not say the solution itself cannot register", note.Message)
			}
		}
	}
	if !warned {
		t.Errorf("withholding the secrets was not reported as a warning: %+v", derived.Notes)
	}
}

// The derivation reports what it supplied instead of printing it. pkg/control
// calls it from a process whose stdout carries JSON-RPC — the MCP server serves
// on it — so a narration line written from here corrupts that stream. The notes
// must come back with the inputs, and nothing may reach stdout.
func TestDerivedRunInputsReportsNotesWithoutPrinting(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")

	var derived RunInputs
	var derivedErr error
	stdout := captureStdout(t, func() {
		derived, derivedErr = DerivedRunInputs(ctx, workspace,
			wikiModuleIn(workspace), wikiService("backend"), "wiki/backend")
	})
	if derivedErr != nil {
		t.Fatalf("DerivedRunInputs: %v", derivedErr)
	}
	if stdout != "" {
		t.Errorf("the derivation wrote %q to stdout; narration must be returned, not printed", stdout)
	}
	if len(derived.Notes) == 0 {
		t.Fatal("the derivation returned no notes: an operator cannot see that the CLI supplied the values")
	}
	if !strings.Contains(derived.Notes[0].Message, manifest.APIConsumesEnvironmentVariable) {
		t.Errorf("first note %q does not report the projection it injected", derived.Notes[0].Message)
	}
	var reportedSecrets bool
	for _, note := range derived.Notes {
		if strings.Contains(note.Message, "registration secrets") {
			reportedSecrets = true
		}
		if note.Warning {
			t.Errorf("a fully wired federation reported a warning: %q", note.Message)
		}
	}
	if !reportedSecrets {
		t.Error("the notes never mention the provisioned registration secrets")
	}
}

// captureStdout swaps os.Stdout for the duration of fn. cli.Info resolves
// os.Stdout at call time, so this observes exactly what a print from inside the
// derivation would put on the protocol stream.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	collected := make(chan string, 1)
	go func() {
		var buffer bytes.Buffer
		_, _ = io.Copy(&buffer, reader)
		collected <- buffer.String()
	}()
	fn()
	os.Stdout = original
	_ = writer.Close()
	written := <-collected
	_ = reader.Close()
	return written
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

// The solution's own credential is the third thing a run provisions, and the
// one every solution needs whether or not it consumes anything: the host admits
// a solution's gateway upstream and frontend remote only against the digest
// declared for its id in SOLUTION_REGISTRATION_SECRETS. The entry gets the
// plaintext, the registrar the `id:sha256hex` digest under the solution key —
// never under a module key, since a solution credential carries strictly more
// authority than a module one.
func TestDerivedRunInputsProvisionsTheSolutionsOwnRegistrationSecret(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")

	derived, err := DerivedRunInputs(ctx, workspace, wikiModuleIn(workspace), wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	secret := derived.Overrides["wiki/backend"][solutionRegistrationSecretEnvironmentVariable]
	if secret == "" {
		t.Fatalf("the entry received no %s", solutionRegistrationSecretEnvironmentVariable)
	}
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	digests := parsePairs(t, declared[solutionRegistrationSecretsKey])
	want := sha256.Sum256([]byte(secret))
	if len(digests) != 1 || digests["wiki"] != hex.EncodeToString(want[:]) {
		t.Errorf("%s = %q, want the module name paired with the sha256 of the entry's secret",
			solutionRegistrationSecretsKey, declared[solutionRegistrationSecretsKey])
	}
	for key, value := range declared {
		if strings.Contains(value, secret) {
			t.Errorf("registrar received the plaintext solution secret under %s; it must hold only digests", key)
		}
		if key != solutionRegistrationSecretsKey && strings.Contains(value, "wiki:") {
			t.Errorf("the solution digest was declared under %s; a solution credential is not a module credential", key)
		}
	}
	// The module halves are untouched by the third.
	if declared[moduleRegistrationSecretsKey] == "" || declared[moduleIdentitySecretsKey] == "" {
		t.Errorf("provisioning the solution secret dropped the module digests: %+v", declared)
	}
	for unique, values := range derived.Overrides {
		if unique == "wiki/backend" {
			continue
		}
		if _, leaked := values[solutionRegistrationSecretEnvironmentVariable]; leaked {
			t.Errorf("%s received the solution's registration secret; only the entry presents it", unique)
		}
	}
	var reported bool
	for _, note := range derived.Notes {
		if strings.Contains(note.Message, solutionRegistrationSecretEnvironmentVariable) && strings.Contains(note.Message, "solution wiki") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the notes never say the solution secret was provisioned: %+v", derived.Notes)
	}
}

// A solution that consumes nothing still registers with the host, so it still
// needs its own secret — and only that: no module key is declared for a run
// that federates no prefix.
func TestDerivedRunInputsProvisionsTheSolutionSecretWithoutConsumes(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-federation")
	checkout := t.TempDir()
	writeSolutionManifest(t, checkout, solutionManifestWithoutConsumes)
	module := wikiModuleAt(checkout)

	derived, err := DerivedRunInputs(ctx, workspace, module, wikiService("backend"), "wiki/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	backend := derived.Overrides["wiki/backend"]
	if backend[solutionRegistrationSecretEnvironmentVariable] == "" {
		t.Fatalf("the entry received no %s: %+v", solutionRegistrationSecretEnvironmentVariable, derived.Overrides)
	}
	if _, projected := backend[manifest.APIConsumesEnvironmentVariable]; projected {
		t.Errorf("a manifest with no api.consumes projected %s", manifest.APIConsumesEnvironmentVariable)
	}
	if _, provisioned := backend[moduleRegistrationSecretsEnvironmentVariable]; provisioned {
		t.Errorf("a solution federating nothing received %s", moduleRegistrationSecretsEnvironmentVariable)
	}
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	if declared[solutionRegistrationSecretsKey] == "" {
		t.Errorf("no solution digest declared: %+v", declared)
	}
	for _, key := range []string{moduleRegistrationSecretsKey, moduleIdentitySecretsKey} {
		if _, present := declared[key]; present {
			t.Errorf("%s declared for a run that federates no prefix", key)
		}
	}
}

// The whole derivation for a solution composed by source and version: its module
// is not the workspace's own and its manifest sits in its own checkout, and it
// must still get the projection, the module secrets for what it consumes, and
// its own registration secret under its module name.
func TestDerivedRunInputsForAComposedSolution(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-composed")
	if self := RootRef(workspace); self != nil {
		t.Fatalf("the fixture must have no self-root module for this to prove anything, got %q", self.Name)
	}
	module := loadTestModule(t, workspace, "consumer")

	derived, err := DerivedRunInputs(ctx, workspace, module, wikiService("backend"), "consumer/backend")
	if err != nil {
		t.Fatalf("DerivedRunInputs: %v", err)
	}
	backend := derived.Overrides["consumer/backend"]
	if backend[manifest.APIConsumesEnvironmentVariable] == "" {
		t.Errorf("the composed entry received no %s", manifest.APIConsumesEnvironmentVariable)
	}
	if secrets := parsePairs(t, backend[moduleRegistrationSecretsEnvironmentVariable]); secrets["accounts"] == "" {
		t.Errorf("the composed entry received no registration secret for the accounts prefix: %q", backend[moduleRegistrationSecretsEnvironmentVariable])
	}
	secret := backend[solutionRegistrationSecretEnvironmentVariable]
	if secret == "" {
		t.Fatalf("the composed entry received no %s", solutionRegistrationSecretEnvironmentVariable)
	}
	declared := derived.WorkspaceConfigurations[federationConfigurationGroup]
	want := sha256.Sum256([]byte(secret))
	if got := parsePairs(t, declared[solutionRegistrationSecretsKey])["consumer"]; got != hex.EncodeToString(want[:]) {
		t.Errorf("solution digest for consumer = %q, want the sha256 of its secret", got)
	}
}

// The registrar refuses a whole declaration when one identity is malformed, and
// refuses to boot: a module name it cannot accept must therefore mint nothing,
// out loud, rather than take the host down with a message blaming a credential.
func TestProvisionSolutionRegistrationSecretRefusesAnIdentityTheRegistrarRejects(t *testing.T) {
	for _, id := range []string{"Wiki", "wiki_go", "wiki.go", "-wiki", strings.Repeat("w", registrationIdentityMaxLength+1)} {
		secret, notes := provisionSolutionRegistrationSecret(id, "wiki/backend", []string{"host/accounts"})
		if secret != "" {
			t.Errorf("minted a secret for %q, which the registrar cannot declare", id)
		}
		if len(notes) != 1 || !notes[0].Warning || !strings.Contains(notes[0].Message, id) {
			t.Errorf("refusing %q was not reported as a warning naming it: %+v", id, notes)
		}
	}
	if secret, _ := provisionSolutionRegistrationSecret("wiki-go", "wiki/backend", []string{"host/accounts"}); secret == "" {
		t.Error("a well-formed identity minted nothing")
	}
}

// A module holding the registrar admits solutions; it does not register as one.
// Injecting the plaintext beside the digest it is checked against would dissolve
// the separation the digest carrier exists to create.
func TestProvisionSolutionRegistrationSecretSkipsTheRegistrarsOwnModule(t *testing.T) {
	secret, notes := provisionSolutionRegistrationSecret("host", "host/frontend", []string{"host/accounts"})
	if secret != "" {
		t.Errorf("minted a solution secret for the module that holds the digests")
	}
	if len(notes) != 1 || notes[0].Warning {
		t.Errorf("skipping the registrar's module is a statement, not a warning: %+v", notes)
	}
}

// Two solutions named as co-roots of one run each declare a digest to the same
// host. Merged key by key the second would replace the first, and only the
// last-named solution could register; the declarations must join instead — and
// an identity declared by both is kept once, since the registrar refuses a
// duplicate and boots nothing when it sees one.
func TestMergeJoinsDeclarationsAcrossRoots(t *testing.T) {
	first := RunInputs{
		Overrides:               map[string]map[string]string{"wiki/backend": {"A": "1"}},
		WorkspaceConfigurations: map[string]map[string]string{federationConfigurationGroup: {solutionRegistrationSecretsKey: "wiki:aa", moduleRegistrationSecretsKey: "documents:11"}},
		Notes:                   []Note{{Message: "first"}},
	}
	second := RunInputs{
		Overrides:               map[string]map[string]string{"notes/backend": {"B": "2"}, "wiki/backend": {"A": "3"}},
		WorkspaceConfigurations: map[string]map[string]string{federationConfigurationGroup: {solutionRegistrationSecretsKey: "notes:bb", moduleRegistrationSecretsKey: "documents:22,archives:33"}},
		Notes:                   []Note{{Message: "second"}},
	}
	merged := Merge(first, second)
	declared := merged.WorkspaceConfigurations[federationConfigurationGroup]
	if declared[solutionRegistrationSecretsKey] != "wiki:aa,notes:bb" {
		t.Errorf("%s = %q, want both solutions declared", solutionRegistrationSecretsKey, declared[solutionRegistrationSecretsKey])
	}
	if declared[moduleRegistrationSecretsKey] != "documents:11,archives:33" {
		t.Errorf("%s = %q, want the first declaration of documents kept and archives joined", moduleRegistrationSecretsKey, declared[moduleRegistrationSecretsKey])
	}
	if merged.Overrides["wiki/backend"]["A"] != "3" || merged.Overrides["notes/backend"]["B"] != "2" {
		t.Errorf("overrides did not layer key by key: %+v", merged.Overrides)
	}
	if len(merged.Notes) != 2 || merged.Notes[0].Message != "first" || merged.Notes[1].Message != "second" {
		t.Errorf("notes lost their order: %+v", merged.Notes)
	}
	if empty := Merge(RunInputs{}, RunInputs{}); empty.Overrides != nil || empty.WorkspaceConfigurations != nil {
		t.Errorf("merging nothing produced inputs: %+v", empty)
	}
}

// Among the modules declaring a service-entry, the root is the one nothing else
// in that set depends on. The host is depended on by the app, through a module
// with no entry of its own, and by the consumer, through api.consumes alone —
// so it is never a root, whichever of the two is composed beside it.
func TestSolutionRootsExcludesAnEntryAnotherEntryDependsOn(t *testing.T) {
	ctx := context.Background()
	workspace := loadTestWorkspace(t, "testdata/solution-composed")
	host := loadTestModule(t, workspace, "host")
	app := loadTestModule(t, workspace, "app")
	consumer := loadTestModule(t, workspace, "consumer")

	for _, tc := range []struct {
		name    string
		entries []*resources.Module
		want    []string
	}{
		{name: "transitively through a module with no entry", entries: []*resources.Module{host, app}, want: []string{"app"}},
		{name: "through api.consumes alone", entries: []*resources.Module{consumer, host}, want: []string{"consumer"}},
		{name: "two solutions beside one host", entries: []*resources.Module{host, app, consumer}, want: []string{"app", "consumer"}},
		{name: "a lone entry", entries: []*resources.Module{host}, want: []string{"host"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, root := range SolutionRoots(ctx, workspace, tc.entries) {
				got = append(got, root.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SolutionRoots = %v, want %v", got, tc.want)
			}
		})
	}
}

func loadTestModule(t *testing.T, workspace *resources.Workspace, name string) *resources.Module {
	t.Helper()
	for _, ref := range workspace.Modules {
		if ref.Name != name {
			continue
		}
		module, err := workspace.LoadModuleFromReference(context.Background(), ref)
		if err != nil {
			t.Fatalf("cannot load module %s: %v", name, err)
		}
		return module
	}
	t.Fatalf("workspace %s references no module %s", workspace.Name, name)
	return nil
}
