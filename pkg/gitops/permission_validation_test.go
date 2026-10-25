package gitops

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/stretchr/testify/require"
)

func TestPreflightWarnsLocallyButRefusesRenderBeforeEffects(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	source := policyPackage(t, false)
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), "module.package.codefly.yaml")))
	require.NoError(t, os.CopyFS(module.Dir(), os.DirFS(source)))
	// The fixture publishes an explicitly unmarked Plain method.
	err := ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "/api.ApiService/Plain")
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, false))
	require.ErrorContains(t, CheckCompositionPermissions(t.Context(), workspace, env, true), "no permission policy")
	// Registry, agents and deployment output must never be reached.
	_, err = RenderModule(t.Context(), workspace, module, env, "", nil)
	require.ErrorContains(t, err, "/api.ApiService/Plain")
	_, err = os.Stat(filepath.Join(workspace.Dir(), "deployments", "modules", module.Name))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestDuplicatePublishedPolicyCannotHideConflictingDeclaration(t *testing.T) {
	dir := policyPackage(t, false)
	path := filepath.Join(dir, "module.package.codefly.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	// Publishing an identical contract twice is permitted.
	start := strings.Index(string(data), "      - endpoint:")
	end := strings.Index(string(data), "\ncontracts:")
	entry := string(data)[start:end]
	entry = strings.Replace(entry, "endpoint: grpc", "endpoint: connect", 1)
	duplicate := strings.Replace(string(data), "\ncontracts:", "\n"+entry+"\ncontracts:", 1)
	duplicate = strings.Replace(duplicate, "endpoints: [grpc]", "endpoints: [grpc, connect]", 1)
	require.NoError(t, os.WriteFile(path, []byte(duplicate), 0644))
	_, _, err = MethodPolicies(dir)
	require.NoError(t, err)
	originalPath := filepath.Join(dir, "contracts/api/api/grpc/contract.binpb")
	encoded, err := os.ReadFile(originalPath)
	require.NoError(t, err)
	var set descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(encoded, &set))
	proto.SetExtension(set.File[0].Service[0].Method[0].Options, hopv1.E_Policy, &hopv1.Policy{AnyCheckedCaller: true})
	altered, err := proto.Marshal(&set)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "contracts/api/other.binpb"), altered, 0644))
	oldDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(encoded))
	newDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(altered))
	changedEntry := strings.Replace(entry, "contracts/api/api/grpc/contract.binpb", "contracts/api/other.binpb", 1)
	changedEntry = strings.Replace(changedEntry, oldDigest, newDigest, 1)
	require.NoError(t, os.WriteFile(path, []byte(strings.Replace(duplicate, entry, changedEntry, 1)), 0644))
	_, _, err = MethodPolicies(dir)
	require.ErrorContains(t, err, "conflicting policies for /api.ApiService/WhoAmI")

}

func TestPreflightUsesSelectedClosureRatherThanEveryPinnedModule(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, resources.WorkspaceConfigurationName), []byte("name: selection\nlayout: modules\nmodules:\n  - name: good\n  - name: bad\n"), 0644))
	for _, name := range []string{"good", "bad"} {
		dir := filepath.Join(root, "modules", name)
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, resources.ModuleConfigurationName), []byte("kind: module\nname: "+name+"\n"), 0644))
	}
	require.NoError(t, os.CopyFS(filepath.Join(root, "modules", "bad"), os.DirFS(policyPackage(t, false))))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "local")
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "good"))
	require.ErrorContains(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "bad"), "no permission policy")
}
