package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/languages"
	resources "github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/standards"
	"gopkg.in/yaml.v3"
)

// solutionSDKFixture is a temp workspace composing module "host" by path (the
// producer, with a module.package.codefly.yaml + contracts/api catalog) and
// its own "backend" module (the solution root, with a service-entry "api"
// service), plus a solution.codefly.yaml with one bound consume entry.
type solutionSDKFixture struct {
	wsDir      string
	hostDir    string
	backendDir string
}

func newSolutionSDKFixture(t *testing.T) solutionSDKFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()

	hostDir := filepath.Join(root, "host")
	writeHostModule(t, hostDir)

	backendDir := filepath.Join(root, "backend")
	writeBackendModule(t, backendDir, "")

	workspace := &resources.Workspace{
		Name:   "backend",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "host", PathOverride: &hostDir},
			{Name: "backend", PathOverride: &backendDir},
		},
	}
	wsDir := filepath.Join(root, "ws")
	if err := workspace.SaveToDirUnsafe(ctx, wsDir); err != nil {
		t.Fatal(err)
	}

	writeSolutionManifest(t, wsDir, "", nil)

	return solutionSDKFixture{wsDir: wsDir, hostDir: hostDir, backendDir: backendDir}
}

// auditContractBytes is a stable, synthetic (not buf-parseable) descriptor
// set: enough for the resolution/validation paths under test, none of which
// reach proto.GenerateClient.
var auditContractBytes = []byte("synthetic-descriptor-set-accounts-connect")

func auditDigest() string {
	return composition.APIContractDigest(auditContractBytes)
}

// writeHostModule writes the composed "host" module: module.codefly.yaml (so
// workspace.LoadModuleFromReference can load it), module.package.codefly.yaml,
// and contracts/api/{catalog.codefly.json, accounts/connect/contract.binpb}.
func writeHostModule(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()

	contractDir := filepath.Join(dir, "contracts", "api", "accounts", "connect")
	if err := os.MkdirAll(contractDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contractDir, "contract.binpb"), auditContractBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	catalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/host",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "accounts",
			Endpoint: "connect",
			API:      standards.CONNECT,
			Kind:     composition.APIContractKindProtobuf,
			Package:  "test.accounts.v1",
			Path:     "contracts/api/accounts/connect/contract.binpb",
			Digest:   auditDigest(),
			Services: []composition.APIContractService{{
				Name:       "AuditService",
				FullName:   "test.accounts.v1.AuditService",
				Procedures: []string{"/test.accounts.v1.AuditService/QueryAuditLog"},
			}},
		}},
	}
	data, err := catalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contracts", "api", "catalog.codefly.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	packageManifest := `kind: module-package
schema: codefly/module-package/v2
id: codefly/host
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - contracts
contracts:
  composition: ">=2.0"
services: []
`
	if err := os.WriteFile(filepath.Join(dir, "module.package.codefly.yaml"), []byte(packageManifest), 0o644); err != nil {
		t.Fatal(err)
	}

	module := &resources.Module{Kind: resources.ModuleKind, Name: "host"}
	module.WithDir(dir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}
}

// writeBackendModule writes the solution's own module: module.codefly.yaml
// (service-entry "api") and services/api/service.codefly.yaml. extraServiceYAML
// is appended verbatim to the service manifest (a comment, an existing
// service-dependencies block) so a test can seed hand-authored content.
func writeBackendModule(t *testing.T, dir, extraServiceYAML string) {
	t.Helper()
	ctx := context.Background()

	module := &resources.Module{
		Kind:              resources.ModuleKind,
		Name:              "backend",
		ServiceEntry:      "api",
		ServiceReferences: []*resources.ServiceReference{{Name: "api"}},
	}
	module.WithDir(dir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}

	serviceDir := filepath.Join(dir, "services", "api")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "kind: service\nname: api\nversion: 0.0.1\nagent:\n  kind: codefly:service\n  name: go\n  publisher: codefly.dev\n  version: 0.0.1\n"
	content += extraServiceYAML
	if err := os.WriteFile(filepath.Join(serviceDir, resources.ServiceConfigurationName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSolutionManifest writes solution.codefly.yaml with one bound consume
// entry (module host / service accounts / endpoint connect, services
// [AuditService]). versionConstraint and services override the entry when
// set.
func writeSolutionManifest(t *testing.T, wsDir, versionConstraint string, services []string) {
	t.Helper()
	if services == nil {
		services = []string{"AuditService"}
	}
	m := &manifest.Manifest{
		SchemaVersion:   manifest.SchemaVersionV0,
		ProtocolVersion: manifest.ProtocolVersionV0,
		Agent: resources.Agent{
			Kind:      resources.SolutionAgent,
			Publisher: "test",
			Name:      "test-solution",
			Version:   "0.1.0",
		},
		API: manifest.API{
			Consumes: []manifest.APIDeclaration{{
				ID:       "accounts",
				Protocol: standards.CONNECT,
				Module:   "host",
				Service:  "accounts",
				Endpoint: "connect",
				Version:  versionConstraint,
				Services: services,
			}},
		},
		Lifecycle: manifest.Lifecycle{Create: true},
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, manifest.FileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadFixtureManifest(t *testing.T, wsDir string) *manifest.Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(wsDir, manifest.FileName))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func loadFixtureWorkspace(t *testing.T, wsDir string) *resources.Workspace {
	t.Helper()
	ws, err := resources.LoadWorkspaceFromDir(context.Background(), wsDir)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func resetSolutionSDKFlags(t *testing.T) {
	t.Helper()
	prevLangs, prevCheck, prevApply, prevSkip := solutionSDKLanguages, solutionSDKCheck, solutionSDKApply, skipGenerateForTests
	solutionSDKLanguages, solutionSDKCheck, solutionSDKApply = nil, false, false
	skipGenerateForTests = true
	t.Cleanup(func() {
		solutionSDKLanguages, solutionSDKCheck, solutionSDKApply, skipGenerateForTests = prevLangs, prevCheck, prevApply, prevSkip
	})
}

func TestSyncSolutionSDKResolvesFromComposedPackage(t *testing.T) {
	fixture := newSolutionSDKFixture(t)
	ctx := context.Background()

	ws := loadFixtureWorkspace(t, fixture.wsDir)
	m := loadFixtureManifest(t, fixture.wsDir)

	entries, err := resolveConsumedContracts(ctx, ws, m)
	if err != nil {
		t.Fatalf("resolveConsumedContracts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.PackageID != "codefly/host" {
		t.Fatalf("entry.PackageID = %q, want %q", entry.PackageID, "codefly/host")
	}
	if entry.Endpoint.Digest != auditDigest() {
		t.Fatalf("entry.Endpoint.Digest = %q, want %q", entry.Endpoint.Digest, auditDigest())
	}

	t.Chdir(fixture.wsDir)
	resetSolutionSDKFlags(t)
	solutionSDKCheck = true
	err = runSyncSolutionSDK(ctx)
	if err == nil {
		t.Fatal("expected --check to fail: library is absent")
	}
	if !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("error = %v, want it to mention the library is out of date", err)
	}
}

func TestSyncSolutionSDKRejectsMissingContracts(t *testing.T) {
	fixture := newSolutionSDKFixture(t)
	if err := os.RemoveAll(filepath.Join(fixture.hostDir, "contracts", "api")); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	ws := loadFixtureWorkspace(t, fixture.wsDir)
	m := loadFixtureManifest(t, fixture.wsDir)

	_, err := resolveConsumedContracts(ctx, ws, m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "ships no API contracts") {
		t.Fatalf("error = %v, want it to contain %q", err, "ships no API contracts")
	}
}

func TestSyncSolutionSDKRejectsUnsatisfiedVersion(t *testing.T) {
	fixture := newSolutionSDKFixture(t)
	writeSolutionManifest(t, fixture.wsDir, ">=0.2.0", nil)

	ctx := context.Background()
	ws := loadFixtureWorkspace(t, fixture.wsDir)
	m := loadFixtureManifest(t, fixture.wsDir)

	_, err := resolveConsumedContracts(ctx, ws, m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "does not satisfy") {
		t.Fatalf("error = %v, want it to contain %q", err, "does not satisfy")
	}
}

func TestSyncSolutionSDKRejectsUnknownService(t *testing.T) {
	fixture := newSolutionSDKFixture(t)
	writeSolutionManifest(t, fixture.wsDir, "", []string{"NopeService"})

	ctx := context.Background()
	ws := loadFixtureWorkspace(t, fixture.wsDir)
	m := loadFixtureManifest(t, fixture.wsDir)

	_, err := resolveConsumedContracts(ctx, ws, m)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "NopeService") {
		t.Fatalf("error = %v, want it to mention %q", err, "NopeService")
	}
}

func TestSyncSolutionSDKApplyDependencies(t *testing.T) {
	root := t.TempDir()
	hostDir := filepath.Join(root, "host")
	writeHostModule(t, hostDir)
	backendDir := filepath.Join(root, "backend")
	writeBackendModule(t, backendDir, "# hand-written note: do not remove\n")

	workspace := &resources.Workspace{
		Name:   "backend",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "host", PathOverride: &hostDir},
			{Name: "backend", PathOverride: &backendDir},
		},
	}
	wsDir := filepath.Join(root, "ws")
	ctx := context.Background()
	if err := workspace.SaveToDirUnsafe(ctx, wsDir); err != nil {
		t.Fatal(err)
	}
	writeSolutionManifest(t, wsDir, "", nil)

	t.Chdir(wsDir)
	resetSolutionSDKFlags(t)
	solutionSDKApply = true
	if err := runSyncSolutionSDK(ctx); err != nil {
		t.Fatalf("runSyncSolutionSDK: %v", err)
	}

	servicePath := filepath.Join(backendDir, "services", "api", resources.ServiceConfigurationName)
	data, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "# hand-written note: do not remove") {
		t.Fatalf("hand-written comment was dropped:\n%s", content)
	}

	service, err := resources.LoadServiceFromDir(ctx, filepath.Join(backendDir, "services", "api"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, dep := range service.ServiceDependencies {
		if dep.Name == "accounts" && dep.Module == "host" {
			found = true
		}
	}
	if !found {
		t.Fatalf("service-dependencies missing accounts/host: %+v", service.ServiceDependencies)
	}
}

func TestSyncSolutionSDKDiamond(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	hostDir := filepath.Join(root, "host")
	writeHostModule(t, hostDir)

	// A second producer module whose catalog carries the SAME proto package
	// ("test.accounts.v1") at a DIFFERENT digest — the diamond.
	otherDir := filepath.Join(root, "other")
	if err := os.MkdirAll(filepath.Join(otherDir, "contracts", "api", "billing", "connect"), 0o755); err != nil {
		t.Fatal(err)
	}
	otherContractBytes := []byte("synthetic-descriptor-set-billing-connect")
	if err := os.WriteFile(filepath.Join(otherDir, "contracts", "api", "billing", "connect", "contract.binpb"), otherContractBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	otherCatalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/other",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "billing",
			Endpoint: "connect",
			API:      standards.CONNECT,
			Kind:     composition.APIContractKindProtobuf,
			Package:  "test.accounts.v1",
			Path:     "contracts/api/billing/connect/contract.binpb",
			Digest:   composition.APIContractDigest(otherContractBytes),
			Services: []composition.APIContractService{{
				Name:       "AuditService",
				FullName:   "test.accounts.v1.AuditService",
				Procedures: []string{"/test.accounts.v1.AuditService/QueryAuditLog"},
			}},
		}},
	}
	data, err := otherCatalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "contracts", "api", "catalog.codefly.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	packageManifest := `kind: module-package
schema: codefly/module-package/v2
id: codefly/other
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - contracts
contracts:
  composition: ">=2.0"
services: []
`
	if err := os.WriteFile(filepath.Join(otherDir, "module.package.codefly.yaml"), []byte(packageManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	otherModule := &resources.Module{Kind: resources.ModuleKind, Name: "other"}
	otherModule.WithDir(otherDir)
	if err := otherModule.Save(ctx); err != nil {
		t.Fatal(err)
	}

	backendDir := filepath.Join(root, "backend")
	writeBackendModule(t, backendDir, "")

	workspace := &resources.Workspace{
		Name:   "backend",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "host", PathOverride: &hostDir},
			{Name: "other", PathOverride: &otherDir},
			{Name: "backend", PathOverride: &backendDir},
		},
	}
	wsDir := filepath.Join(root, "ws")
	if err := workspace.SaveToDirUnsafe(ctx, wsDir); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{
		SchemaVersion:   manifest.SchemaVersionV0,
		ProtocolVersion: manifest.ProtocolVersionV0,
		Agent: resources.Agent{
			Kind:      resources.SolutionAgent,
			Publisher: "test",
			Name:      "test-solution",
			Version:   "0.1.0",
		},
		API: manifest.API{
			Consumes: []manifest.APIDeclaration{
				{ID: "accounts", Protocol: standards.CONNECT, Module: "host", Service: "accounts", Endpoint: "connect", Services: []string{"AuditService"}},
				{ID: "billing", Protocol: standards.CONNECT, Module: "other", Service: "billing", Endpoint: "connect", Services: []string{"AuditService"}},
			},
		},
		Lifecycle: manifest.Lifecycle{Create: true},
	}
	mdata, err := yaml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, manifest.FileName), mdata, 0o644); err != nil {
		t.Fatal(err)
	}

	ws := loadFixtureWorkspace(t, wsDir)
	loaded := loadFixtureManifest(t, wsDir)
	entries, err := resolveConsumedContracts(ctx, ws, loaded)
	if err != nil {
		t.Fatalf("resolveConsumedContracts: %v", err)
	}

	// The diamond check runs before any codegen, so this never touches Docker.
	err = generators.GenerateClientLibrary(ctx, &generators.ClientLibraryRequest{
		Name:      "test-solution-sdk",
		Output:    t.TempDir(),
		Languages: []languages.Language{languages.GO},
		Entries:   entries,
	})
	if err == nil || !strings.Contains(err.Error(), "two versions") {
		t.Fatalf("error = %v, want it to contain %q", err, "two versions")
	}
}

// buildRealDescriptorSet compiles a two-service proto package with buf into a
// real FileDescriptorSet, so TestSyncSolutionSDKEndToEnd exercises the actual
// facade generator rather than a synthetic digest-only fixture. Requires
// `buf` on PATH in addition to the CODEFLY_GENERATE_QUALIFY Docker gate,
// since the codegen itself (coreproto.GenerateClient) still runs inside the
// proto companion image; only compiling the fixture's descriptor set needs a
// local buf.
func buildRealDescriptorSet(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("buf"); err != nil {
		t.Skip("buf is not on PATH; skipping the real end-to-end fixture")
	}
	dir := t.TempDir()
	buf := `version: v2
modules:
  - path: .
`
	proto := `syntax = "proto3";
package test.accounts.v1;

message QueryAuditLogRequest { string query = 1; }
message QueryAuditLogResponse { repeated string entries = 1; }
message ExportDataRequest { string filter = 1; }
message ExportDataResponse { bytes data = 1; }

service AuditService {
  rpc QueryAuditLog(QueryAuditLogRequest) returns (QueryAuditLogResponse);
}

service ExportService {
  rpc ExportData(ExportDataRequest) returns (ExportDataResponse);
}
`
	if err := os.WriteFile(filepath.Join(dir, "buf.yaml"), []byte(buf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "descriptor.binpb")
	cmd := exec.Command("buf", "build", "-o", out)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("buf build: %v\n%s", err, output)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSyncSolutionSDKEndToEnd(t *testing.T) {
	if os.Getenv("CODEFLY_GENERATE_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GENERATE_QUALIFY=1 to run the disposable Docker qualification")
	}
	descriptorSet := buildRealDescriptorSet(t)
	digest := composition.APIContractDigest(descriptorSet)

	root := t.TempDir()
	hostDir := filepath.Join(root, "host")
	contractDir := filepath.Join(hostDir, "contracts", "api", "accounts", "connect")
	if err := os.MkdirAll(contractDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contractDir, "contract.binpb"), descriptorSet, 0o644); err != nil {
		t.Fatal(err)
	}
	catalog := &composition.APIContractCatalog{
		Schema:  composition.APIContractCatalogSchema,
		Package: "codefly/host",
		Version: "0.1.0",
		Endpoints: []composition.APIContractEndpoint{{
			Service:  "accounts",
			Endpoint: "connect",
			API:      standards.CONNECT,
			Kind:     composition.APIContractKindProtobuf,
			Package:  "test.accounts.v1",
			Path:     "contracts/api/accounts/connect/contract.binpb",
			Digest:   digest,
			Services: []composition.APIContractService{
				{Name: "AuditService", FullName: "test.accounts.v1.AuditService", Procedures: []string{"/test.accounts.v1.AuditService/QueryAuditLog"}},
				{Name: "ExportService", FullName: "test.accounts.v1.ExportService", Procedures: []string{"/test.accounts.v1.ExportService/ExportData"}},
			},
		}},
	}
	catalogData, err := catalog.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDir, "contracts", "api", "catalog.codefly.json"), catalogData, 0o644); err != nil {
		t.Fatal(err)
	}
	packageManifest := `kind: module-package
schema: codefly/module-package/v2
id: codefly/host
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - contracts
contracts:
  composition: ">=2.0"
services: []
`
	if err := os.WriteFile(filepath.Join(hostDir, "module.package.codefly.yaml"), []byte(packageManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	hostModule := &resources.Module{Kind: resources.ModuleKind, Name: "host"}
	hostModule.WithDir(hostDir)
	if err := hostModule.Save(ctx); err != nil {
		t.Fatal(err)
	}

	backendDir := filepath.Join(root, "backend")
	writeBackendModule(t, backendDir, "")

	workspace := &resources.Workspace{
		Name:   "backend",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "host", PathOverride: &hostDir},
			{Name: "backend", PathOverride: &backendDir},
		},
	}
	wsDir := filepath.Join(root, "ws")
	if err := workspace.SaveToDirUnsafe(ctx, wsDir); err != nil {
		t.Fatal(err)
	}
	writeSolutionManifest(t, wsDir, "", []string{"AuditService"})

	t.Chdir(wsDir)
	resetSolutionSDKFlags(t)
	skipGenerateForTests = false
	solutionSDKLanguages = []string{"python"}
	if err := runSyncSolutionSDK(ctx); err != nil {
		t.Fatalf("runSyncSolutionSDK: %v", err)
	}

	outputDir := filepath.Join(wsDir, "libraries", "backend-sdk")
	manifestData, err := os.ReadFile(filepath.Join(outputDir, resources.LibraryConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sources []struct {
			ContractDigest string `yaml:"contract-digest"`
		} `yaml:"sources"`
	}
	if err := yaml.Unmarshal(manifestData, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sources) != 1 {
		t.Fatalf("sources = %+v, want exactly one element", doc.Sources)
	}

	facadePath := filepath.Join(outputDir, "python", "backend_sdk", "accounts.py")
	data, err := os.ReadFile(facadePath)
	if err != nil {
		t.Fatalf("read facade file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "def query_audit_log") {
		t.Fatalf("%s missing def query_audit_log:\n%s", facadePath, content)
	}
	if strings.Contains(content, "ExportService") {
		t.Fatalf("%s should not mention ExportService (filtered by --services):\n%s", facadePath, content)
	}
}
