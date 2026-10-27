package gitops

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/genproto/googleapis/api/annotations"
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
	// Manifests go to the endpoint that implements the interface.
	composed = strings.Replace(composed, "      delivery: platform/accounts/rest\n", "      delivery: platform/accounts/http\n", 1)
	require.Contains(t, composed, "      delivery: platform/accounts/http\n")
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

// composePlainHost adds the environment's host module to the workspace with a
// delivery endpoint that implements no Codefly interface.
func composePlainHost(t *testing.T, workspace *resources.Workspace) *resources.Workspace {
	t.Helper()
	config := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	composed := strings.Replace(string(data), "  - name: billing\n", "  - name: billing\n  - name: platform\n", 1)
	require.NotEqual(t, string(data), composed)
	require.NoError(t, os.WriteFile(config, []byte(composed), 0o644))
	files := map[string]string{
		filepath.Join("modules", "platform", resources.ModuleConfigurationName):                          "kind: module\nname: platform\nservices:\n  - name: accounts\n",
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

// A host that does not consume method policies never receives them, so an
// unmarked method passes the preflight of render, run cluster and CI deploy
// exactly as MethodPolicies lists it: declaring nothing, for the hop to refuse
// per call. A contradictory policy is malformed wherever it is read, and still
// refuses.
func TestAnUnmarkedMethodPassesTowardAHostThatDoesNotConsumePolicies(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	workspace = composePlainHost(t, workspace)
	module, err := workspace.LoadModuleFromName(t.Context(), module.Name)
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	require.NoError(t, CheckCompositionPermissions(t.Context(), workspace, env, true, "shop"))

	markPlain(t, module.Dir(), &hopv1.Policy{AnyCheckedCaller: true, Requires: []*hopv1.Requirement{{ResourceKind: "console.identity", Actions: []string{"read"}}}})
	err = CheckCompositionPermissions(t.Context(), workspace, env, true, "shop")
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

// The policies go where the manifests go: the endpoint host.delivery names. A
// host service implementing codefly.dev/composition on another endpoint still
// receives manifests on its delivery endpoint, which consumes no policy, so an
// unmarked method passes (saas: composition on connect, delivery on rest).
func TestCompositionOnAnotherEndpointThanDeliveryConsumesNoPolicy(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	workspace = composeCompositionHost(t, workspace)
	config := filepath.Join(workspace.Dir(), "modules", "platform", resources.ModuleConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config, []byte(strings.Replace(string(data), "endpoint: http", "endpoint: grpc", 1)), 0o644))
	workspace, err = resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(t.Context(), module.Name)
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
}

// A module may hold every method it publishes to a policy wherever it is
// delivered: its contract's require_method_policies refuses an unmarked
// method toward a host that would have passed it.
func TestAModuleContractRequiresAPolicyOnEveryMethodWhereverDelivered(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	workspace = composePlainHost(t, workspace)
	module, err := workspace.LoadModuleFromName(t.Context(), module.Name)
	require.NoError(t, err)
	env := selectedEnvironment(t, workspace, "staging")
	require.NoError(t, ValidateModulePermissions(t.Context(), workspace, module, env))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract+"require_method_policies: true\n"), 0o644))
	err = ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.ErrorContains(t, err, "sets require_method_policies")
}

// A host that cannot be read never drops the requirement: one the workspace
// does not compose is held to receive every policy, by name, and one whose
// module does not load is an error naming why, never a silent "no".
func TestAnUnreadableHostNeverDropsThePolicyRequirement(t *testing.T) {
	workspace, module := unmarkedPolicyWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	err := ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.ErrorContains(t, err, "host platform/accounts is not composed in this workspace")

	workspace = composePlainHost(t, workspace)
	module, err = workspace.LoadModuleFromName(t.Context(), module.Name)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(workspace.Dir(), "modules", "platform", resources.ModuleConfigurationName), []byte("kind: module\nname: platform\nservices: {not: a list}\n"), 0o644))
	err = ValidateModulePermissions(t.Context(), workspace, module, env)
	require.ErrorContains(t, err, "cannot tell whether host platform/accounts implements "+CompositionInterface)
	_, err = HostImplements(t.Context(), workspace, "platform", "accounts", "rest", CompositionInterface)
	require.ErrorContains(t, err, "its module does not load")
}

// composeSaasHost composes saas as the host, the way platform compositions
// name it: saas's own accounts service is host.delivery, implementing
// codefly.dev/composition, and host.component is whatever the environment
// calls the host's instance (saas locally, platform-obin on a cell). saas
// publishes that many unmarked methods of its own (the shape of its 447),
// which are the host's, not methods behind the hop.
func composeSaasHost(t *testing.T, workspace *resources.Workspace, component string, methods int) *resources.Workspace {
	t.Helper()
	config := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(config)
	require.NoError(t, err)
	composed := strings.Replace(string(data), "  - name: billing\n", "  - name: billing\n  - name: saas\n", 1)
	composed = strings.Replace(composed, "      component: platform-host\n", "      component: "+component+"\n", 1)
	composed = strings.Replace(composed, "      delivery: platform/accounts/rest\n", "      delivery: saas/accounts/grpc\n", 1)
	require.Contains(t, composed, "      component: "+component+"\n")
	require.Contains(t, composed, "      delivery: saas/accounts/grpc\n")
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

// The host's own module is not behind the hop: the module of the
// host.delivery service (saas, from saas/accounts/grpc) implements
// codefly.dev/composition, so its own unmarked methods are the host's and
// pass, while every module the hop fronts is still held to a policy on every
// method and refused by name. host.component is the host's instance name, not
// a module: saas locally, platform-obin on a cell, and naming the fronted
// module there sets nothing aside.
func TestTheHostsOwnModuleIsNotHeldToPoliciesItReceives(t *testing.T) {
	const saasMethods = 447
	for _, component := range []string{"saas", "platform-obin", "shop"} {
		t.Run("component "+component, func(t *testing.T) {
			workspace, module := unmarkedPolicyWorkspace(t)
			workspace = composeSaasHost(t, workspace, component, saasMethods)
			env := selectedEnvironment(t, workspace, "staging")
			require.Equal(t, component, env.Host.Component)

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
		})
	}
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

// addHostStandIn adds the local authority's shape to shop: a service "local"
// whose exported grpc endpoint implements implemented — the interface the
// host's delivery service implements, for a stand-in — and publishes
// LocalService, two methods declaring nothing. route, when set, binds one of
// them to that HTTP route; identity declares module-identity on it.
func addHostStandIn(t *testing.T, workspace *resources.Workspace, implemented, route string, identity bool) *resources.Workspace {
	t.Helper()
	dir := filepath.Join(workspace.Dir(), "modules", "shop")
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String("LocalService")}
	for _, name := range []string{"Check", "Current"} {
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(".host.Empty"), OutputType: proto.String(".host.Empty")}
		if name == "Current" && route != "" {
			m.Options = &descriptorpb.MethodOptions{}
			proto.SetExtension(m.Options, annotations.E_Http, &annotations.HttpRule{Pattern: &annotations.HttpRule_Get{Get: route}})
		}
		service.Method = append(service.Method, m)
	}
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("local.proto"), Package: proto.String("host"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}}, Service: []*descriptorpb.ServiceDescriptorProto{service},
	}}})
	require.NoError(t, err)
	local := hostedServiceYAML("local")
	if identity {
		local += "module-identity: true\n"
	}
	source := "syntax = \"proto3\";\npackage host;\n"
	files := map[string]string{
		resources.ModuleConfigurationName: "kind: module\nname: shop\nservices:\n  - name: api\n  - name: local\ninterface:\n  endpoints:\n" +
			"    - service: api\n      endpoint: grpc\n    - service: api\n      endpoint: http\n      visibility: public\n" +
			"    - service: local\n      endpoint: grpc\n      implements: [" + implemented + "]\n",
		filepath.Join("services", "local", resources.ServiceConfigurationName):     local,
		filepath.Join("services", "local", "proto", "local.proto"):                 source,
		filepath.Join("contracts", "api", "local", "grpc", "proto", "local.proto"): source,
		filepath.Join("contracts", "api", "local", "grpc", "contract.binpb"):       string(set),
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
	manifestPath := filepath.Join(dir, "module.package.codefly.yaml")
	manifest, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	if !strings.Contains(string(manifest), "  - name: local\n") {
		published := strings.Replace(string(manifest), "\ncontracts:\n", fmt.Sprintf(`
  - name: local
    endpoints: [grpc]
    api-contracts:
      - endpoint: grpc
        kind: protobuf
        package: host
        path: contracts/api/local/grpc/contract.binpb
        digest: sha256:%x
contracts:
`, sha256.Sum256(set)), 1)
		require.NotEqual(t, string(manifest), published)
		manifest = []byte(published)
	} else {
		manifest = regexp.MustCompile(`(path: contracts/api/local/grpc/contract.binpb\n\s+digest: )sha256:[0-9a-f]+`).ReplaceAll(manifest, []byte(fmt.Sprintf("${1}sha256:%x", sha256.Sum256(set))))
	}
	require.NoError(t, os.WriteFile(manifestPath, manifest, 0o644))
	reloaded, err := resources.LoadWorkspaceFromDir(t.Context(), workspace.Dir())
	require.NoError(t, err)
	return reloaded
}

// A service that stands in for the host — it implements the interface the
// host's delivery service implements, and does not run as the module, as the
// local authority does beside its hop — is the host's code: a call to it is
// never one the host decides for the module. So the table the module's
// manifest carries to that host, which the preflight holds and a caller's
// binding resolves against, leaves it out: its unmarked methods are not
// refused, and a route it binds differently from the module's own service is
// no conflict there, though the whole package still declares it twice. Every
// service that runs as the module stays held by name — the module's own api,
// and the stand-in itself once it declares module-identity — and a service
// implementing an interface the host does not implement stands in for nothing.
func TestAServiceStandingInForTheHostIsNotInTheTableItsManifestCarries(t *testing.T) {
	const composition = CompositionInterface + "@1.0.0"
	ctx := t.Context()
	workspace, _, _ := sourcePolicyWorkspace(t)
	workspace = composeCompositionHost(t, workspace)
	workspace = addHostStandIn(t, workspace, composition, "/v1/console/whoami", false)
	env := selectedEnvironment(t, workspace, "staging")
	module, err := workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)

	_, _, err = MethodPolicies(module.Dir())
	require.ErrorContains(t, err, "conflicting policies for GET /v1/console/whoami")
	methods, ok, err := HostMethodPolicies(ctx, workspace, env, module)
	require.NoError(t, err)
	require.True(t, ok)
	var names []string
	for _, m := range methods {
		names = append(names, m.GetMethod())
	}
	require.Contains(t, names, "/api.ApiService/WhoAmI")
	require.Contains(t, names, "GET /v1/console/whoami")
	require.NotContains(t, names, "/host.LocalService/Check")
	require.NoError(t, ValidateModulePermissions(ctx, workspace, module, env))
	require.NoError(t, CheckCompositionPermissions(ctx, workspace, env, true, "shop"))

	// The module's own service still declares a policy on every method.
	markPlain(t, module.Dir(), nil)
	err = CheckCompositionPermissions(ctx, workspace, env, true, "shop")
	require.ErrorContains(t, err, "/api.ApiService/Plain declares no permission policy")
	require.NotContains(t, err.Error(), "LocalService")
	markPlain(t, module.Dir(), &hopv1.Policy{AnyCheckedCaller: true})

	// A stand-in that runs as the module is what the host fronts.
	workspace = addHostStandIn(t, workspace, composition, "", true)
	module, err = workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	err = ValidateModulePermissions(ctx, workspace, module, env)
	require.ErrorContains(t, err, "/host.LocalService/Check declares no permission policy")
	require.ErrorContains(t, err, "/host.LocalService/Current declares no permission policy")

	// An interface the host does not implement makes no stand-in.
	workspace = addHostStandIn(t, workspace, "codefly.dev/session@0.2.0", "", false)
	module, err = workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	require.ErrorContains(t, ValidateModulePermissions(ctx, workspace, module, env), "/host.LocalService/Check declares no permission policy")
}
