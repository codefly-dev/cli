package generate

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func TestPermissionsCheckDetectsDriftWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	contract := `schema: codefly/module-contract/v1
principal: ledger
namespaces: []
queues: []
scope_ceilings:
  - resource_kind: ledger.entry
    actions: [read, write]
bindings: []
destinations: []
foreign_methods:
  - method: /api/Read
    requires:
      - resource_kind: ledger.entry
        actions: [read]
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "module.contract.codefly.yaml"), []byte(contract), 0644))
	require.NoError(t, generatePermissions(t.Context(), dir, "", false))
	require.NoError(t, generatePermissions(t.Context(), dir, "", true))
	path := filepath.Join(dir, "pkg/gen/permissions/permissions_gen.go")
	before, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "module.contract.codefly.yaml"), []byte(strings.Replace(contract, "[read]", "[write]", 1)), 0644))
	require.ErrorContains(t, generatePermissions(t.Context(), dir, "", true), "stale")
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.ModTime(), after.ModTime())
	require.NoError(t, generatePermissions(t.Context(), dir, "", false))
	require.NoError(t, os.Remove(filepath.Join(dir, "pkg/gen/permissions/permissions_gen.ts")))
	require.ErrorContains(t, generatePermissions(t.Context(), dir, "", true), "permissions_gen.ts")
}
