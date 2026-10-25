package gitops

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
	clicomposition "github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/cli/pkg/generators"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Start with valid published policies and the source snapshot exported beside
// them. Only the source changes in the regression, so digest checks still pass.
func sourcePolicyWorkspace(t *testing.T) (*resources.Workspace, *resources.Module, string) {
	t.Helper()
	workspace, module := writeAuthorityWorkspace(t)
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), modulecontract.FileName)))
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), "module.package.codefly.yaml")))
	require.NoError(t, os.CopyFS(module.Dir(), os.DirFS(policyPackage(t, false))))
	contract := filepath.Join(module.Dir(), "contracts/api/api/grpc/contract.binpb")
	encoded, err := os.ReadFile(contract)
	require.NoError(t, err)
	var set descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(encoded, &set))
	set.File[0].Service[0].Method[2].Options = &descriptorpb.MethodOptions{}
	proto.SetExtension(set.File[0].Service[0].Method[2].Options, hopv1.E_Policy, &hopv1.Policy{AnyCheckedCaller: true})
	valid, err := proto.Marshal(&set)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(contract, valid, 0644))
	manifestPath := filepath.Join(module.Dir(), "module.package.codefly.yaml")
	manifest, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	manifest = []byte(strings.ReplaceAll(string(manifest), fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), fmt.Sprintf("sha256:%x", sha256.Sum256(valid))))
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0644))
	source := filepath.Join(module.Dir(), "services/api/proto")
	snapshot := filepath.Join(filepath.Dir(contract), "proto")
	for _, dir := range []string{source, snapshot} {
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "api.proto"), []byte(`syntax = "proto3";
package api;
import "authority/hop/v1/policy.proto";
message Empty {}
service ApiService {
 rpc WhoAmI(Empty) returns (Empty) { option (authority.hop.v1.policy) = {requires: [{resource_kind: "console.identity", actions: ["read"]}]}; }
 rpc Version(Empty) returns (Empty) { option (authority.hop.v1.policy) = {any_checked_caller: true}; }
 rpc Plain(Empty) returns (Empty) { option (authority.hop.v1.policy) = {any_checked_caller: true}; }
}
`), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "buf.yaml"), []byte("version: v2\nmodules:\n  - path: .\n"), 0644))
	}
	return workspace, module, source
}

func TestPreflightDetectsUnexportedPermissionSourceBeforeRender(t *testing.T) {
	workspace, module, source := sourcePolicyWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	path := filepath.Join(source, "api.proto")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(strings.ReplaceAll(string(original), `actions: ["read"]`, `actions: ["write"]`)), 0644))
	// A changed requirement remains valid policy. It is the stale export, not a
	// policy syntax error or bad digest, which must stop a source-backed deploy.
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, false, "shop"))
	err = CheckCompositionPermissions(t.Context(), workspace, env, true, "shop")
	require.ErrorContains(t, err, "api.proto")
	require.ErrorContains(t, err, "codefly generate contracts shop")
	_, err = RenderModule(t.Context(), workspace, module, env, "", nil)
	require.ErrorContains(t, err, "api.proto")
	_, err = os.Stat(filepath.Join(workspace.Dir(), "deployments/modules/shop"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(path, original, 0644))
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
}

func TestPermissionSourceDetectsTreeChangesAndMissingInputs(t *testing.T) {
	for _, tc := range []struct {
		name, path, content string
		remove              bool
	}{
		{name: "new proto", path: "added.proto", content: "syntax = \"proto3\";"},
		{name: "deleted proto", path: "api.proto", remove: true},
		{name: "invalid source", path: "api.proto", content: "not a proto"},
		{name: "buf configuration", path: "buf.yaml", content: "version: v1\n"},
		{name: "new lock", path: "buf.lock", content: "version: v2\n"},
		{name: "missing source", path: ".", remove: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, module, source := sourcePolicyWorkspace(t)
			if tc.remove {
				require.NoError(t, os.RemoveAll(filepath.Join(source, tc.path)))
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(source, tc.path), []byte(tc.content), 0644))
			}
			err := ValidateModulePermissions(t.Context(), workspace, module, selectedEnvironment(t, workspace, "staging"))
			require.ErrorContains(t, err, "source/export freshness")
			if tc.path != "." {
				require.ErrorContains(t, err, tc.path)
			}
		})
	}
	t.Run("missing snapshot", func(t *testing.T) {
		workspace, module, _ := sourcePolicyWorkspace(t)
		require.NoError(t, os.RemoveAll(filepath.Join(module.Dir(), "contracts/api/api/grpc/proto")))
		require.ErrorContains(t, ValidateModulePermissions(t.Context(), workspace, module, selectedEnvironment(t, workspace, "staging")), "required proto tree")
	})
}

func TestPermissionSourceChecksEveryEndpointSnapshot(t *testing.T) {
	workspace, module, _ := sourcePolicyWorkspace(t)
	root := module.Dir()
	require.NoError(t, os.CopyFS(filepath.Join(root, "contracts/api/api/connect"), os.DirFS(filepath.Join(root, "contracts/api/api/grpc"))))
	path := filepath.Join(root, "module.package.codefly.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	start := strings.Index(string(data), "      - endpoint:")
	end := strings.Index(string(data), "\ncontracts:")
	entry := strings.ReplaceAll(string(data)[start:end], "grpc", "connect")
	changed := strings.Replace(string(data), "\ncontracts:", "\n"+entry+"\ncontracts:", 1)
	changed = strings.Replace(changed, "endpoints: [grpc]", "endpoints: [grpc, connect]", 1)
	require.NoError(t, os.WriteFile(path, []byte(changed), 0644))
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	require.NoError(t, os.WriteFile(filepath.Join(root, "contracts/api/api/connect/proto/api.proto"), []byte("stale"), 0644))
	require.ErrorContains(t, ValidateModulePermissions(t.Context(), workspace, module, env), "endpoint connect")
}

func TestPermissionSourceAllowsMaterializedPackageWithoutSource(t *testing.T) {
	workspace, module, source := sourcePolicyWorkspace(t)
	require.NoError(t, os.RemoveAll(source))
	workspace, module = materializedPolicyWorkspace(t, workspace, module)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	// Artifact integrity still applies even when no source check is required.
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), "contracts/api/api/grpc/contract.binpb"), []byte("tamper"), 0644))
	require.ErrorContains(t, ValidateModulePermissions(t.Context(), workspace, module, env), "the package names")
}

func TestPermissionSourcePreflightRereadsWithoutWriting(t *testing.T) {
	workspace, module, source := sourcePolicyWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	preflight := PermissionPreflight(true)
	require.NoError(t, preflight(t.Context(), workspace, env))
	require.NoError(t, os.WriteFile(filepath.Join(source, "api.proto"), []byte("edited"), 0644))
	before, err := generators.ReadTree(workspace.Dir())
	require.NoError(t, err)
	require.ErrorContains(t, preflight(t.Context(), workspace, env), "api.proto")
	after, err := generators.ReadTree(workspace.Dir())
	require.NoError(t, err)
	require.Equal(t, before, after)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, validatePermissionSource(ctx, workspace, module, nil), context.Canceled)
}

func TestPermissionSourceChecksSelectedCalleeOnly(t *testing.T) {
	workspace, module, source := sourcePolicyWorkspace(t)
	current, err := os.ReadFile(filepath.Join(source, "api.proto"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "api.proto"), []byte(strings.ReplaceAll(string(current), `actions: ["read"]`, `actions: ["write"]`)), 0644))
	path := filepath.Join(workspace.Dir(), "modules/billing/services/ledger", resources.ServiceConfigurationName)
	original := []byte(devServiceYAML("ledger"))
	require.NoError(t, os.WriteFile(path, original, 0644))
	workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "billing"))
	require.NoError(t, os.WriteFile(path, append(original, []byte("\nservice-dependencies:\n  - name: api\n    module: shop\n")...), 0644))
	workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	require.ErrorContains(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "billing"), "module "+module.Name+" service api")
	billing, err := workspace.LoadModuleFromName(t.Context(), "billing")
	require.NoError(t, err)
	_, err = RenderModule(t.Context(), workspace, billing, env, "", nil)
	require.ErrorContains(t, err, "module shop service api")
	ledger, err := billing.LoadServiceFromName(t.Context(), "ledger")
	require.NoError(t, err)
	_, err = RenderService(t.Context(), workspace, billing, ledger, env, "", false, nil)
	require.ErrorContains(t, err, "module shop service api")
	_, err = os.Stat(filepath.Join(workspace.Dir(), "deployments"))
	require.ErrorIs(t, err, os.ErrNotExist)

}

func TestPermissionSourceUsesServiceOverride(t *testing.T) {
	workspace, module, source := sourcePolicyWorkspace(t)
	override := filepath.Join(t.TempDir(), "api")
	require.NoError(t, os.CopyFS(override, os.DirFS(filepath.Dir(source))))
	require.NoError(t, os.WriteFile(filepath.Join(override, "proto/api.proto"), []byte("edited override"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), resources.LocalOverlayConfigurationName), []byte("resolve:\n  shop:\n    services:\n      api:\n        path: "+override+"\n"), 0644))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(t.Context(), "shop")
	require.NoError(t, err)
	require.ErrorContains(t, ValidateModulePermissions(t.Context(), workspace, module, selectedEnvironment(t, workspace, "staging")), "api.proto")
}

func TestPermissionSourceDevPathIsCheckedBeforeBuild(t *testing.T) {
	for _, packaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("packaged=%v", packaged), func(t *testing.T) {
			workspace, module, source := sourcePolicyWorkspace(t)
			override := filepath.Join(t.TempDir(), "api")
			require.NoError(t, os.CopyFS(override, os.DirFS(filepath.Dir(source))))
			if packaged {
				require.NoError(t, os.RemoveAll(source))
				workspace, module = materializedPolicyWorkspace(t, workspace, module)
			}
			renderDevFixture(t, workspace)
			require.NoError(t, os.WriteFile(filepath.Join(override, "proto/api.proto"), []byte("edited explicit source"), 0644))
			calls := stubBuild(t, []string{"registry.example.com/acme/api@" + devNewDigest}, nil)
			before, err := generators.ReadTree(workspace.Dir())
			require.NoError(t, err)
			_, err = DeployDev(t.Context(), &DevRequest{Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"), Source: DevSource{Dir: override, Origin: DevSourceFlag}})
			require.ErrorContains(t, err, "source/export freshness")
			require.Zero(t, *calls)
			after, err := generators.ReadTree(workspace.Dir())
			require.NoError(t, err)
			require.Equal(t, before, after)
			// A matching explicit source can replace stale unselected source.
			published, err := os.ReadFile(filepath.Join(module.Dir(), "contracts/api/api/grpc/proto/api.proto"))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(override, "proto/api.proto"), published, 0644))
			if !packaged {
				require.NoError(t, os.WriteFile(filepath.Join(source, "api.proto"), []byte("stale unselected workspace"), 0644))
			}
			_, err = DeployDev(t.Context(), &DevRequest{Workspace: workspace, Module: module, Service: "api", Environment: environmentNamed("staging"), Source: DevSource{Dir: override, Origin: DevSourceFlag}})
			require.NoError(t, err)
			require.Equal(t, 1, *calls)

		})
	}
}

func materializedPolicyWorkspace(t *testing.T, workspace *resources.Workspace, module *resources.Module) (*resources.Workspace, *resources.Module) {
	t.Helper()
	// A materialized package has a path in the overlay, with its origin retained
	// by the CLI's existing receipt. Copied snapshot proto is not local source.
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName), []byte("name: acme\nlayout: modules\nenvironments:\n  - name: staging\nmodules:\n  - name: shop\n    source: github.com/example/shop\n    version: 1.0.0\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), resources.LocalOverlayConfigurationName), []byte("resolve:\n  shop:\n    path: "+module.Dir()+"\n"), 0644))
	require.NoError(t, clicomposition.SaveResolutionReceipts(t.Context(), workspace.Dir(), map[string]*clicomposition.ResolutionReceipt{
		"shop": {Source: "github.com/example/shop", Requested: "1.0.0", Version: "1.0.0", Mode: clicomposition.ResolutionModeVerified, Path: module.Dir()},
	}))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(t.Context(), "shop")
	require.NoError(t, err)
	return workspace, module
}

func TestPermissionSourceChecksConfigurationOnlyProducer(t *testing.T) {
	workspace, _, source := sourcePolicyWorkspace(t)
	require.NoError(t, os.WriteFile(filepath.Join(source, "api.proto"), []byte("stale callee"), 0644))
	path := filepath.Join(workspace.Dir(), "modules/billing/services/ledger", resources.ServiceConfigurationName)
	// No runtime service dependency. This service gets its producer from values.
	require.NoError(t, os.WriteFile(path, []byte(devServiceYAML("ledger")), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "configurations/staging/target.env"), []byte("ENDPOINT=${endpoint:shop/api/grpc}\n"), 0644))
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	// Root-only values don't invent module participation.
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "billing"))
	require.NoError(t, os.WriteFile(path, []byte(devServiceYAML("ledger")+"workspace-configuration-dependencies:\n  - target\n"), 0644))
	workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	require.ErrorContains(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "billing"), "module shop service api")
	billing, err := workspace.LoadModuleFromName(t.Context(), "billing")
	require.NoError(t, err)
	_, err = RenderModule(t.Context(), workspace, billing, env, "", nil)
	require.ErrorContains(t, err, "module shop service api")
	ledger, err := billing.LoadServiceFromName(t.Context(), "ledger")
	require.NoError(t, err)
	_, err = RenderService(t.Context(), workspace, billing, ledger, env, "", false, nil)
	require.ErrorContains(t, err, "module shop service api")
	_, err = os.Stat(filepath.Join(workspace.Dir(), "deployments"))
	require.ErrorIs(t, err, os.ErrNotExist)
	encoded, err := resources.EncodeWorkspaceConfigurationOverrides([]resources.WorkspaceConfigurationOverride{{Name: "target", Key: "ENDPOINT", Value: "${endpoint:billing/ledger/grpc}"}})
	require.NoError(t, err)
	t.Setenv(resources.WorkspaceConfigurationOverridesEnvironment, encoded)
	// An invocation override removes the edge to shop. On-disk text cannot
	// determine which producer this invocation actually receives.
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "billing"))
}
