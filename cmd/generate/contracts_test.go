package generate

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/codefly-dev/core/standards"
	googleproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func resetContractsFlags(t *testing.T) {
	t.Helper()
	prevOutput, prevCheck, prevFormat := contractsOutput, contractsCheck, contractsFormat
	contractsOutput, contractsCheck, contractsFormat = "", false, "text"
	t.Cleanup(func() {
		contractsOutput, contractsCheck, contractsFormat = prevOutput, prevCheck, prevFormat
	})
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

// saveFixtureWorkspace creates a workspace with a single module named
// moduleName, whose interface exposes service/endpoint. moduleDir and
// serviceDir are returned so the caller can add module.package.codefly.yaml
// or additional service files before the command runs.
func saveFixtureWorkspace(t *testing.T, ctx context.Context, moduleName string, iface *resources.ModuleInterface, service *resources.Service) (root, moduleDir string) {
	t.Helper()
	root = t.TempDir()
	workspace := &resources.Workspace{
		Name:    "test-workspace",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: moduleName}},
	}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}

	moduleDir = filepath.Join(root, "modules", moduleName)
	module := &resources.Module{
		Kind:              resources.ModuleKind,
		Name:              moduleName,
		ServiceReferences: []*resources.ServiceReference{{Name: service.Name}},
		Interface:         iface,
	}
	module.WithDir(moduleDir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}

	service.WithDir(filepath.Join(moduleDir, "services", service.Name))
	if err := service.Save(ctx); err != nil {
		t.Fatal(err)
	}
	return root, moduleDir
}

func TestGenerateContractsRequiresInterface(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "test-workspace",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "billing"}},
	}
	if err := workspace.SaveToDirUnsafe(ctx, root); err != nil {
		t.Fatal(err)
	}
	moduleDir := filepath.Join(root, "modules", "billing")
	module := &resources.Module{Kind: resources.ModuleKind, Name: "billing"}
	module.WithDir(moduleDir)
	if err := module.Save(ctx); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	resetContractsFlags(t)

	err := ContractsCmd.RunE(ContractsCmd, []string{"billing"})
	if err == nil {
		t.Fatal("expected an error for a module without an interface")
	}
	if !strings.Contains(err.Error(), "declares no interface") {
		t.Fatalf("error = %v, want it to contain %q", err, "declares no interface")
	}
}

func TestGenerateContractsPreservesPackageManifestComments(t *testing.T) {
	content := []byte(`kind: package
schema: codefly/module-package/v1
id: codefly/billing
version: 1.2.3
services:
  - name: api
    endpoints:
      - grpc
# billing's compliance claims
claims:
  - billing:read
  - billing:write
`)
	entries := map[string][]composition.ProvidedAPIContract{
		"api": {
			{
				Endpoint: "grpc",
				Kind:     composition.APIContractKindProtobuf,
				Package:  "billing.v1",
				Path:     "contracts/api/api/grpc/contract.binpb",
				Digest:   "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			},
		},
	}

	updated, err := updatePackageManifestAPIContracts(content, entries)
	if err != nil {
		t.Fatalf("updatePackageManifestAPIContracts: %v", err)
	}

	want := `kind: package
schema: codefly/module-package/v1
id: codefly/billing
version: 1.2.3
services:
  - name: api
    endpoints:
      - grpc
    api-contracts:
      - endpoint: grpc
        kind: protobuf
        package: billing.v1
        path: contracts/api/api/grpc/contract.binpb
        digest: sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef
# billing's compliance claims
claims:
  - billing:read
  - billing:write
`
	got := string(updated)
	if got != want {
		t.Fatalf("output changed more than the inserted api-contracts block:\ngot:\n%s\nwant:\n%s", got, want)
	}

	again, err := updatePackageManifestAPIContracts(updated, entries)
	if err != nil {
		t.Fatalf("updatePackageManifestAPIContracts (second pass): %v", err)
	}
	if string(again) != got {
		t.Fatalf("re-running with the same entries is not idempotent:\nfirst:\n%s\nsecond:\n%s", got, again)
	}
}

func TestGenerateContractsSkipsNonContractEndpoints(t *testing.T) {
	ctx := context.Background()
	root, _ := saveFixtureWorkspace(t, ctx, "billing",
		&resources.ModuleInterface{
			Endpoints: []*resources.InterfaceEndpoint{
				{Service: "api", Endpoint: "web", Visibility: resources.VisibilityPublic},
			},
		},
		&resources.Service{
			Name:    "api",
			Version: "0.0.1",
			Endpoints: []*resources.Endpoint{
				{Name: "web", API: "http", Visibility: resources.VisibilityPublic},
			},
		},
	)

	t.Chdir(root)
	resetContractsFlags(t)
	contractsFormat = "json"

	out, err := captureStdout(t, func() error {
		return ContractsCmd.RunE(ContractsCmd, []string{"billing"})
	})
	if err != nil {
		t.Fatalf("RunE: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "no contract; skipped") {
		t.Fatalf("output missing skip notice:\n%s", out)
	}

	var catalog composition.APIContractCatalog
	decoder := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):]))
	if err := decoder.Decode(&catalog); err != nil {
		t.Fatalf("cannot parse catalog JSON: %v\noutput:\n%s", err, out)
	}
	if len(catalog.Endpoints) != 0 {
		t.Fatalf("catalog.Endpoints = %+v, want none", catalog.Endpoints)
	}
}

// The tests below scaffold a real go-grpc service and run buf inside the
// proto companion, so they need Docker and network access. Gated like the
// other agent-backed qualification tests (see docs/commands.md
// "CODEFLY_GITOPS_K3D_QUALIFY").

func buildCodeflyBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codefly")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/codefly-dev/cli/cmd/codefly")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build codefly binary: %v\n%s", err, out)
	}
	return bin
}

func runCodefly(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("codefly %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// scaffoldGoGRPCFixture builds a workspace with a go-grpc "api" service in
// module "billing", exposes its "grpc" endpoint through the module interface,
// and gives the module a minimal package manifest. It returns the module
// directory.
func scaffoldGoGRPCFixture(t *testing.T) string {
	t.Helper()
	// The in-process agent instance cache is keyed only by module/service name
	// (core's services.Load), so a prior test's "billing/api" fixture — a
	// different temp workspace, since deleted — would otherwise be served here.
	services.ClearAgents()
	ctx := context.Background()
	bin := buildCodeflyBinary(t)

	root := t.TempDir()
	runCodefly(t, bin, root, "init", "workspace", "qualify-ws", "--default", "--layout", "modules")
	wsDir := filepath.Join(root, "qualify-ws")
	runCodefly(t, bin, wsDir, "add", "module", "billing", "--yes")
	moduleDir := filepath.Join(wsDir, "modules", "billing")
	runCodefly(t, bin, moduleDir, "add", "service", "api", "--agent", "go-grpc", "--default")

	service, err := resources.LoadServiceFromDir(ctx, filepath.Join(moduleDir, "services", "api"))
	if err != nil {
		t.Fatalf("LoadServiceFromDir: %v", err)
	}
	found := false
	for _, endpoint := range service.Endpoints {
		if endpoint.Name == "grpc" {
			endpoint.Visibility = resources.VisibilityPublic
			found = true
		}
	}
	if !found {
		t.Fatalf("go-grpc scaffold has no grpc endpoint: %+v", service.Endpoints)
	}
	if err := service.SaveAtDir(ctx, service.Dir()); err != nil {
		t.Fatalf("save service: %v", err)
	}

	module, err := resources.LoadModuleFromDir(ctx, moduleDir)
	if err != nil {
		t.Fatalf("LoadModuleFromDir: %v", err)
	}
	module.Interface = &resources.ModuleInterface{
		Endpoints: []*resources.InterfaceEndpoint{
			{Service: "api", Endpoint: "grpc", Visibility: resources.VisibilityPublic},
		},
	}
	if err := module.Save(ctx); err != nil {
		t.Fatalf("save module: %v", err)
	}

	manifest := `kind: module-package
schema: codefly/module-package/v2
id: codefly/billing
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - contracts/api
contracts:
  composition: ">=2.0"
services:
  - name: api
    endpoints:
      - grpc
      - rest
`
	if err := os.WriteFile(filepath.Join(moduleDir, "module.package.codefly.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write module.package.codefly.yaml: %v", err)
	}
	return moduleDir
}

func TestGenerateContractsGoGrpc(t *testing.T) {
	if os.Getenv("CODEFLY_GENERATE_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GENERATE_QUALIFY=1 to run the disposable Docker qualification")
	}
	moduleDir := scaffoldGoGRPCFixture(t)

	t.Chdir(moduleDir)
	resetContractsFlags(t)
	contractsFormat = "json"

	out, err := captureStdout(t, func() error {
		return ContractsCmd.RunE(ContractsCmd, []string{"billing"})
	})
	if err != nil {
		t.Fatalf("RunE: %v\noutput:\n%s", err, out)
	}

	contractPath := filepath.Join(moduleDir, "contracts", "api", "api", "grpc", "contract.binpb")
	data, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read %s: %v", contractPath, err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := googleproto.Unmarshal(data, &set); err != nil {
		t.Fatalf("contract.binpb does not parse as a FileDescriptorSet: %v", err)
	}
	hasService := false
	for _, file := range set.GetFile() {
		if len(file.GetService()) > 0 {
			hasService = true
		}
	}
	if !hasService {
		t.Fatal("descriptor set contains no service")
	}

	var catalog composition.APIContractCatalog
	decoder := json.NewDecoder(strings.NewReader(out[strings.Index(out, "{"):]))
	if err := decoder.Decode(&catalog); err != nil {
		t.Fatalf("cannot parse catalog JSON: %v\noutput:\n%s", err, out)
	}
	if len(catalog.Endpoints) != 1 || len(catalog.Endpoints[0].Services) == 0 {
		t.Fatalf("catalog.Endpoints = %+v, want one endpoint with services", catalog.Endpoints)
	}
	procedure := catalog.Endpoints[0].Services[0].Procedures[0]
	if !strings.HasPrefix(procedure, "/") {
		t.Fatalf("procedure %q does not start with /", procedure)
	}

	manifest, err := composition.LoadPackageManifest(moduleDir)
	if err != nil {
		t.Fatalf("LoadPackageManifest: %v", err)
	}
	var apiContracts []composition.ProvidedAPIContract
	for _, service := range manifest.Services {
		if service.Name == "api" {
			apiContracts = service.APIContracts
		}
	}
	if len(apiContracts) != 1 {
		t.Fatalf("api service api-contracts = %+v, want exactly one", apiContracts)
	}
	if want := composition.APIContractDigest(data); apiContracts[0].Digest != want {
		t.Fatalf("manifest digest = %s, want %s", apiContracts[0].Digest, want)
	}
}

func TestGenerateContractsCheckDetectsDrift(t *testing.T) {
	if os.Getenv("CODEFLY_GENERATE_QUALIFY") != "1" {
		t.Skip("set CODEFLY_GENERATE_QUALIFY=1 to run the disposable Docker qualification")
	}
	moduleDir := scaffoldGoGRPCFixture(t)

	t.Chdir(moduleDir)
	resetContractsFlags(t)
	if err := ContractsCmd.RunE(ContractsCmd, []string{"billing"}); err != nil {
		t.Fatalf("initial generate: %v", err)
	}

	resetContractsFlags(t)
	contractsCheck = true
	if err := ContractsCmd.RunE(ContractsCmd, []string{"billing"}); err != nil {
		t.Fatalf("--check on freshly generated contracts should exit 0: %v", err)
	}

	contractPath := filepath.Join(moduleDir, "contracts", "api", "api", "grpc", "contract.binpb")
	original, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), original...)
	corrupted[0] ^= 0xFF
	if err := os.WriteFile(contractPath, corrupted, 0o644); err != nil {
		t.Fatal(err)
	}

	resetContractsFlags(t)
	contractsCheck = true
	out, err := captureStdout(t, func() error {
		return ContractsCmd.RunE(ContractsCmd, []string{"billing"})
	})
	if err == nil {
		t.Fatalf("--check on drifted contracts should fail\noutput:\n%s", out)
	}
	if !strings.Contains(out, "api/grpc/contract.binpb") {
		t.Fatalf("drift output does not name the differing path:\n%s", out)
	}

	if err := os.WriteFile(contractPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	resetContractsFlags(t)
	contractsCheck = true
	if err := ContractsCmd.RunE(ContractsCmd, []string{"billing"}); err != nil {
		t.Fatalf("--check after restoring the contract should exit 0: %v", err)
	}
}

func TestEndpointCarriesContract(t *testing.T) {
	for _, api := range []string{standards.GRPC, standards.CONNECT, standards.REST} {
		if !endpointCarriesContract(api) {
			t.Errorf("endpointCarriesContract(%q) = false, want true", api)
		}
	}
	for _, api := range []string{standards.HTTP, standards.TCP} {
		if endpointCarriesContract(api) {
			t.Errorf("endpointCarriesContract(%q) = true, want false", api)
		}
	}
}

func TestServiceContractPackage(t *testing.T) {
	// protoDir holds the service's own files; imports buf pulls in live
	// elsewhere and must be ignored even when they declare services.
	protoDir := t.TempDir()
	for _, rel := range []string{"saas/acct/v1/api.proto", "saas/shared/v1/types.proto"} {
		full := filepath.Join(protoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("syntax = \"proto3\";\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	file := func(name, pkg string, withService bool) *descriptorpb.FileDescriptorProto {
		f := &descriptorpb.FileDescriptorProto{Name: googleproto.String(name), Package: googleproto.String(pkg)}
		if withService {
			f.Service = []*descriptorpb.ServiceDescriptorProto{{Name: googleproto.String("Svc")}}
		}
		return f
	}

	t.Run("single service package, ignoring message-only packages and imports", func(t *testing.T) {
		set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
			file("saas/acct/v1/api.proto", "saas.acct.v1", true),
			file("saas/shared/v1/types.proto", "saas.shared.v1", false),
			// an import: has a service but is not one of the service's own files.
			file("google/protobuf/descriptor.proto", "google.protobuf", true),
		}}
		pkg, err := serviceContractPackage(set, protoDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if pkg != "saas.acct.v1" {
			t.Fatalf("pkg = %q, want saas.acct.v1", pkg)
		}
	})

	t.Run("no own file declares a service is an error", func(t *testing.T) {
		set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
			file("saas/acct/v1/api.proto", "saas.acct.v1", false),
			file("saas/shared/v1/types.proto", "saas.shared.v1", false),
		}}
		if _, err := serviceContractPackage(set, protoDir); err == nil {
			t.Fatal("expected an error when no own file declares a service")
		}
	})

	t.Run("services across multiple own packages is an error", func(t *testing.T) {
		set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{
			file("saas/acct/v1/api.proto", "saas.acct.v1", true),
			file("saas/shared/v1/types.proto", "saas.shared.v1", true),
		}}
		if _, err := serviceContractPackage(set, protoDir); err == nil {
			t.Fatal("expected an error when services span multiple packages")
		}
	})
}
