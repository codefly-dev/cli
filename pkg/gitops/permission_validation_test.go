package gitops

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/stretchr/testify/require"
)

// unmarkedPolicyWorkspace is sourcePolicyWorkspace with Plain published, and
// written in source, with no policy at all: the shape of every method of a
// package that predates the hop vocabulary.
func unmarkedPolicyWorkspace(t *testing.T) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace, module, source := sourcePolicyWorkspace(t)
	markPlain(t, module.Dir(), nil)
	snapshot := filepath.Join(module.Dir(), "contracts/api/api/grpc/proto")
	for _, dir := range []string{source, snapshot} {
		path := filepath.Join(dir, "api.proto")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		unmarked := strings.Replace(string(data), "rpc Plain(Empty) returns (Empty) { option (authority.hop.v1.policy) = {any_checked_caller: true}; }", "rpc Plain(Empty) returns (Empty);", 1)
		require.NotEqual(t, string(data), unmarked)
		require.NoError(t, os.WriteFile(path, []byte(unmarked), 0o644))
	}
	return workspace, module
}

// markPlain publishes policy on the package's Plain method (nil: no mark),
// keeping the package manifest's digest the contract's.
func markPlain(t *testing.T, dir string, policy *hopv1.Policy) {
	t.Helper()
	contract := filepath.Join(dir, "contracts/api/api/grpc/contract.binpb")
	encoded, err := os.ReadFile(contract)
	require.NoError(t, err)
	var set descriptorpb.FileDescriptorSet
	require.NoError(t, proto.Unmarshal(encoded, &set))
	plain := set.File[0].Service[0].Method[2]
	require.Equal(t, "Plain", plain.GetName())
	plain.Options = nil
	if policy != nil {
		plain.Options = &descriptorpb.MethodOptions{}
		proto.SetExtension(plain.Options, hopv1.E_Policy, policy)
	}
	marked, err := proto.Marshal(&set)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(contract, marked, 0o644))
	manifestPath := filepath.Join(dir, "module.package.codefly.yaml")
	manifest, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	manifest = []byte(strings.ReplaceAll(string(manifest), fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), fmt.Sprintf("sha256:%x", sha256.Sum256(marked))))
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0o644))
}

// composeCompositionHost adds the environment's host module to the workspace:
// platform, whose accounts service (the host.delivery service) declares that
// it implements codefly.dev/composition.
func composeCompositionHost(t *testing.T, workspace *resources.Workspace) *resources.Workspace {
	t.Helper()
	config := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	composed := strings.Replace(string(data), "  - name: billing\n", "  - name: billing\n  - name: platform\n", 1)
	require.NotEqual(t, string(data), composed)
	require.NoError(t, os.WriteFile(config, []byte(composed), 0o644))
	files := map[string]string{
		filepath.Join("modules", "platform", resources.ModuleConfigurationName):                          "kind: module\nname: platform\nservices:\n  - name: accounts\ninterface:\n  endpoints:\n    - service: accounts\n      endpoint: http\n      implements: [" + CompositionInterface + "@1.0.0]\n",
		filepath.Join("modules", "platform", "services", "accounts", resources.ServiceConfigurationName): hostedServiceYAML("accounts", "platform"),
	}
	for rel, content := range files {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	reloaded, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

// A host that does not consume method policies — the saas host every released
// composition delivers to — never receives them, so an unmarked method passes
// the preflight of render, run cluster and CI deploy exactly as MethodPolicies
// lists it: declaring nothing, for the hop to refuse per call. A contradictory
// policy is malformed wherever it is read, and still refuses.
func TestAnUnmarkedMethodPassesTowardAHostThatDoesNotConsumePolicies(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "shop"))

	markPlain(t, module.Dir(), &hopv1.Policy{AnyCheckedCaller: true, Requires: []*hopv1.Requirement{{ResourceKind: "console.identity", Actions: []string{"read"}}}})
	err := CheckCompositionPermissions(t.Context(), workspace, env, true, "shop")
	require.ErrorContains(t, err, "/api.ApiService/Plain declares both requirements and any_checked_caller")
	markPlain(t, module.Dir(), &hopv1.Policy{Requires: []*hopv1.Requirement{{ResourceKind: "Console", Actions: []string{"read"}}}})
	require.ErrorContains(t, ValidateModulePermissions(t.Context(), workspace, module, env), "invalid permission")
}

// A host implementing codefly.dev/composition receives every method's policy
// with the manifest, so an unmarked method is refused by name, with the reason,
// before the registry, an agent or a deployment file is reached; a local run
// adopts with a warning instead.
func TestACompositionHostRefusesAnUnmarkedMethodByName(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	workspace = composeCompositionHost(t, workspace)
	module, err := workspace.LoadModuleFromName(t.Context(), module.Name)
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	err = ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.ErrorContains(t, err, "delivered to host platform/accounts, which implements "+CompositionInterface)
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, false, "shop"))
	require.ErrorContains(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "shop"), "/api.ApiService/Plain declares no permission policy")
	_, err = RenderModule(t.Context(), workspace, module, env, "", nil)
	require.ErrorContains(t, err, "/api.ApiService/Plain")
	_, err = os.Stat(filepath.Join(workspace.Dir(), "deployments", "modules", module.Name))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// A module may hold every method it publishes to a policy wherever it is
// delivered: its contract's require_method_policies refuses an unmarked
// method toward a host that would have passed it.
func TestAModuleContractRequiresAPolicyOnEveryMethodWhereverDelivered(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract+"require_method_policies: true\n"), 0o644))
	err := ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.ErrorContains(t, err, "sets require_method_policies")
}

// composeSaasHost composes the host as platform compositions name it locally:
// host.component is saas, and saas's own accounts service is host.delivery,
// implementing codefly.dev/composition. saas publishes that many
// unmarked methods of its own (the shape of its 447), which are the host's,
// not methods behind the hop.
func composeSaasHost(t *testing.T, workspace *resources.Workspace, methods int) *resources.Workspace {
	t.Helper()
	config := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	composed := strings.Replace(string(data), "  - name: billing\n", "  - name: billing\n  - name: saas\n", 1)
	composed = strings.Replace(composed, "      component: platform-host\n", "      component: saas\n", 1)
	composed = strings.Replace(composed, "      delivery: platform/accounts/rest\n", "      delivery: saas/accounts/connect\n", 1)
	require.Equal(t, 3, len(strings.Split(composed, "saas"))-1, "the host is saas by component and by delivery")
	require.NoError(t, os.WriteFile(config, []byte(composed), 0o644))

	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String("AccountsService")}
	for i := range methods {
		service.Method = append(service.Method, &descriptorpb.MethodDescriptorProto{Name: proto.String(fmt.Sprintf("Method%03d", i)), InputType: proto.String(".accounts.Empty"), OutputType: proto.String(".accounts.Empty")})
	}
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("accounts.proto"), Package: proto.String("accounts"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}}, Service: []*descriptorpb.ServiceDescriptorProto{service},
	}}})
	require.NoError(t, err)
	source := "syntax = \"proto3\";\npackage accounts;\n"
	files := map[string]string{
		filepath.Join("modules", "saas", resources.ModuleConfigurationName):                          "kind: module\nname: saas\nservices:\n  - name: accounts\ninterface:\n  endpoints:\n    - service: accounts\n      endpoint: grpc\n      implements: [" + CompositionInterface + "@1.0.0]\n",
		filepath.Join("modules", "saas", "services", "accounts", resources.ServiceConfigurationName): hostedServiceYAML("accounts", "shop"),
		filepath.Join("modules", "saas", "services", "accounts", "proto", "accounts.proto"):          source,
		filepath.Join("modules", "saas", "contracts", "accounts", "grpc", "proto", "accounts.proto"): source,
		filepath.Join("modules", "saas", "contracts", "accounts", "grpc", "contract.binpb"):          string(set),
		filepath.Join("modules", "saas", "module.package.codefly.yaml"): fmt.Sprintf(`schema: codefly/module-package/v2
kind: module-package
id: platform/saas
version: 0.1.0
minimum-codefly-version: ">=0.3.32 <1.0.0"
artifact-roots:
  - contracts
  - services
services:
  - name: accounts
    endpoints: [grpc]
    api-contracts:
      - endpoint: grpc
        kind: protobuf
        package: accounts
        path: contracts/accounts/grpc/contract.binpb
        digest: sha256:%x
contracts:
  composition: ">=2.0.0 <3.0.0"
`, sha256.Sum256(set)),
	}
	for rel, content := range files {
		full := filepath.Join(workspace.Dir(), rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	reloaded, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

// The host's own module is not behind the hop: when host.component names saas
// and saas's accounts service implements codefly.dev/composition, saas's own
// unmarked methods are the host's and pass, while every module the hop fronts
// is still held to a policy on every method and refused by name.
func TestTheHostsOwnModuleIsNotHeldToPoliciesItReceives(t *testing.T) {
	const saasMethods = 447
	workspace, module := unmarkedPolicyWorkspace(t)
	workspace = composeSaasHost(t, workspace, saasMethods)
	env := selectedEnvironment(t, workspace, "staging")
	require.Equal(t, "saas", env.Host.Component)

	saas, err := workspace.LoadModuleFromName(t.Context(), "saas")
	require.NoError(t, err)
	methods, ok, err := MethodPolicies(saas.Dir())
	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, methods, saasMethods)
	require.Error(t, ValidateMethodPolicies(methods, true), "every saas method is unmarked")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, saas, env))

	module, err = workspace.LoadModuleFromName(t.Context(), module.Name)
	require.NoError(t, err)
	err = ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.ErrorContains(t, err, "delivered to host saas/accounts, which implements "+CompositionInterface)

	err = CheckCompositionPermissions(t.Context(), workspace, env, true, "shop", "saas")
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.NotContains(t, err.Error(), "/accounts.AccountsService/")
	require.NotContains(t, err.Error(), "module saas")

	// The host module's own contract may still hold it to a policy everywhere.
	require.NoError(t, os.WriteFile(filepath.Join(saas.Dir(), modulecontract.FileName), []byte("schema: codefly/module-contract/v1\nprincipal: saas\nnamespaces: []\nqueues: []\nscope_ceilings: []\nbindings: []\ndestinations: []\nrequire_method_policies: true\n"), 0o644))
	saas, err = workspace.LoadModuleFromName(t.Context(), "saas")
	require.NoError(t, err)
	err = ValidateModulePermissions(t.Context(), workspace, saas, env)
	require.ErrorContains(t, err, "/accounts.AccountsService/Method000 declares no permission policy")
	require.ErrorContains(t, err, "sets require_method_policies")
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
	markPlain(t, filepath.Join(root, "modules", "bad"), &hopv1.Policy{AnyCheckedCaller: true, Requires: []*hopv1.Requirement{{ResourceKind: "console.identity", Actions: []string{"read"}}}})
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "local")
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "good"))
	require.ErrorContains(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "bad"), "declares both requirements and any_checked_caller")
}
