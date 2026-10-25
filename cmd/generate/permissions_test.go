package generate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	"github.com/stretchr/testify/require"
)

func samplePermissions() []*compositionv1.MethodPolicyV1 {
	return []*compositionv1.MethodPolicyV1{
		{Method: "/api/Write", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "ledger.entry", Actions: []string{"write", "read"}}}},
		{Method: "/api/Version", AnyCheckedCaller: true},
	}
}

func TestGeneratedPermissionsCompileAndPreserveMethodRequirements(t *testing.T) {
	methods := samplePermissions()
	files, err := permissionFiles(methods)
	require.NoError(t, err)
	methods[0], methods[1] = methods[1], methods[0]
	again, err := permissionFiles(methods)
	require.NoError(t, err)
	require.Equal(t, files, again)
	dir := t.TempDir()
	for name, data := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0644))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module permissions_test\n\ngo 1.26.0\n"), 0644))
	test := `package permissions
import "testing"
func TestConsumer(t *testing.T) {
 if LedgerEntryRead.Kind()!="ledger.entry" || LedgerEntryRead.Action()!="read" {t.Fatal("wrong permission")}
 if len(Methods()["/api/Write"].Requires)!=2 || !Methods()["/api/Version"].AnyCheckedCaller {t.Fatal("policy lost")}
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "consumer_test.go"), []byte(test), 0644))
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "consumer_test.go"), []byte(strings.Replace(test, "LedgerEntryRead.Kind()", "UndeclaredPermission.Kind()", 1)), 0644))
	cmd = exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err = cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(out), "undefined: UndeclaredPermission")
	require.True(t, bytes.Contains(files["permissions_gen.ts"], []byte("LedgerEntryRead: \"ledger.entry:read\"")))
}

func TestPermissionGenerationRefusesCollisionsAndMissingPolicies(t *testing.T) {
	_, reservedErr := permissionFiles([]*compositionv1.MethodPolicyV1{{Method: "/api/Reserved", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "method", Actions: []string{"policy"}}}}})
	require.ErrorContains(t, reservedErr, "generated identifier MethodPolicy")
	methods := samplePermissions()
	methods = append(methods, &compositionv1.MethodPolicyV1{Method: "/api/Other", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "ledger_entry", Actions: []string{"read"}}}})
	_, err := permissionFiles(methods)
	require.ErrorContains(t, err, "collide")
	_, err = permissionFiles([]*compositionv1.MethodPolicyV1{{Method: "/api/Missing"}})
	require.ErrorContains(t, err, "no permission policy")
}

// permissionsModule writes a module "ledger" with two Go services, api
// (source-dir defaulted) and admin (source-dir go/cmd/admin, so its go.mod is
// at go/), each publishing one protobuf contract with its own marked methods,
// a store service with no Go module and no contract, and a module contract
// declaring one foreign method. It returns the loaded module.
func permissionsModule(t *testing.T) *resources.Module {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	write("module.codefly.yaml", "kind: module\nname: ledger\nservices:\n  - name: api\n  - name: admin\n  - name: store\n")
	agent := "agent:\n  kind: codefly:service\n  name: go-grpc\n  version: 0.0.1\n  publisher: codefly.dev\n"
	write("services/api/service.codefly.yaml", "name: api\nversion: 0.0.0\n"+agent)
	write("services/admin/service.codefly.yaml", "name: admin\nversion: 0.0.0\n"+agent+"spec:\n  source-dir: go/cmd/admin\n")
	write("services/store/service.codefly.yaml", "name: store\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  name: postgres\n  version: 0.0.1\n  publisher: codefly.dev\nspec:\n  database-name: ledger\n")
	write("services/api/code/go.mod", "module example.com/ledger/api\n\ngo 1.26.0\n")
	write("services/admin/go/go.mod", "module example.com/ledger/admin\n\ngo 1.26.0\n")
	write("module.contract.codefly.yaml", `schema: codefly/module-contract/v1
principal: ledger
namespaces: []
queues: []
scope_ceilings:
  - resource_kind: ledger.entry
    actions: [read, write, approve]
bindings: []
destinations: []
foreign_methods:
  - method: /otlp.TraceService/Export
    requires:
      - resource_kind: ledger.entry
        actions: [write]
`)
	contract := func(service, method, action string) string {
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(method), InputType: proto.String(".ledger.Empty"), OutputType: proto.String(".ledger.Empty"), Options: &descriptorpb.MethodOptions{}}
		proto.SetExtension(m.Options, hopv1.E_Policy, &hopv1.Policy{Requires: []*hopv1.Requirement{{ResourceKind: "ledger.entry", Actions: []string{action}}}})
		set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
			Name: proto.String(service + ".proto"), Package: proto.String("ledger"), Syntax: proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}},
			Service:     []*descriptorpb.ServiceDescriptorProto{{Name: proto.String(permissionIdentifier(service) + "Service"), Method: []*descriptorpb.MethodDescriptorProto{m}}},
		}}})
		require.NoError(t, err)
		write("contracts/api/"+service+"/grpc/contract.binpb", string(set))
		sum := sha256.Sum256(set)
		return `  - name: ` + service + `
    endpoints: [grpc]
    api-contracts:
      - endpoint: grpc
        kind: protobuf
        package: ledger
        path: contracts/api/` + service + `/grpc/contract.binpb
        digest: sha256:` + hex.EncodeToString(sum[:]) + "\n"
	}
	write("module.package.codefly.yaml", `schema: codefly/module-package/v2
kind: module-package
id: codefly/ledger
version: 0.1.0
minimum-codefly-version: ">=0.0.0"
artifact-roots:
  - contracts
  - services
services:
`+contract("api", "Read", "read")+contract("admin", "Approve", "approve")+`contracts:
  composition: ">=2.0.0 <3.0.0"
`)
	module, err := resources.LoadModuleFromDir(t.Context(), dir)
	require.NoError(t, err)
	return module
}

func TestPermissionsDefaultLandsInEachGoServiceModule(t *testing.T) {
	module := permissionsModule(t)
	require.NoError(t, generatePermissions(t.Context(), module, "", false))
	require.NoError(t, generatePermissions(t.Context(), module, "", true))
	require.NoDirExists(t, filepath.Join(module.Dir(), "pkg"))
	require.NoDirExists(t, filepath.Join(module.Dir(), "services", "store", "code"))

	api := filepath.Join(module.Dir(), "services", "api", "code", "pkg", "gen", "permissions")
	admin := filepath.Join(module.Dir(), "services", "admin", "go", "pkg", "gen", "permissions")
	read := func(dir, name string) string {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(data)
	}
	// Each service's own methods, and the foreign method the module contract
	// names no service for in both.
	require.Contains(t, read(api, "permissions_gen.go"), `"/ledger.ApiService/Read"`)
	require.NotContains(t, read(api, "permissions_gen.go"), "AdminService")
	require.Contains(t, read(admin, "permissions_gen.go"), `"/ledger.AdminService/Approve"`)
	require.NotContains(t, read(admin, "permissions_gen.go"), "ApiService")
	for _, dir := range []string{api, admin} {
		require.Contains(t, read(dir, "permissions_gen.go"), `"/otlp.TraceService/Export"`)
		require.Contains(t, read(dir, "permissions_gen.ts"), `"/otlp.TraceService/Export"`)
	}

	// The api service imports its table as a package of its own go.mod.
	code := filepath.Join(module.Dir(), "services", "api", "code")
	consumer := `package consumer
import (
	"testing"
	"example.com/ledger/api/pkg/gen/permissions"
)
func TestConsumer(t *testing.T) {
	if permissions.Methods()["/ledger.ApiService/Read"].Requires[0] != permissions.LedgerEntryRead {
		t.Fatal("policy lost")
	}
}
`
	require.NoError(t, os.MkdirAll(filepath.Join(code, "consumer"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(code, "consumer", "consumer_test.go"), []byte(consumer), 0o644))
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = code
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestPermissionsCheckDetectsDriftWithoutWriting(t *testing.T) {
	module := permissionsModule(t)
	require.NoError(t, generatePermissions(t.Context(), module, "", false))
	require.NoError(t, generatePermissions(t.Context(), module, "", true))
	path := filepath.Join(module.Dir(), "services", "admin", "go", "pkg", "gen", "permissions", "permissions_gen.go")
	before, err := os.Stat(path)
	require.NoError(t, err)
	contract := filepath.Join(module.Dir(), "module.contract.codefly.yaml")
	raw, err := os.ReadFile(contract)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(contract, []byte(strings.Replace(string(raw), "actions: [write]\n", "actions: [read]\n", 1)), 0o644))
	require.ErrorContains(t, generatePermissions(t.Context(), module, "", true), "stale")
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime())
	require.NoError(t, generatePermissions(t.Context(), module, "", false))
	require.NoError(t, generatePermissions(t.Context(), module, "", true))
	ts := filepath.Join(module.Dir(), "services", "api", "code", "pkg", "gen", "permissions", "permissions_gen.ts")
	require.NoError(t, os.Remove(ts))
	require.ErrorContains(t, generatePermissions(t.Context(), module, "", true), ts)
}

func TestPermissionsOutputOverrideHoldsTheWholeModule(t *testing.T) {
	module := permissionsModule(t)
	output := filepath.Join(t.TempDir(), "permissions")
	require.NoError(t, generatePermissions(t.Context(), module, output, false))
	require.NoError(t, generatePermissions(t.Context(), module, output, true))
	data, err := os.ReadFile(filepath.Join(output, "permissions_gen.go"))
	require.NoError(t, err)
	for _, method := range []string{"/ledger.ApiService/Read", "/ledger.AdminService/Approve", "/otlp.TraceService/Export"} {
		require.Contains(t, string(data), strconv.Quote(method))
	}
	require.NoDirExists(t, filepath.Join(module.Dir(), "services", "api", "code", "pkg", "gen"))
	require.ErrorContains(t, generatePermissions(t.Context(), module, "", true), "missing or stale")
}

func TestPermissionsRefuseAContractServiceWithNoGoModule(t *testing.T) {
	module := permissionsModule(t)
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), "services", "admin", "go", "go.mod")))
	require.ErrorContains(t, generatePermissions(t.Context(), module, "", false), "service admin publishes method policies but has no go.mod")
}
