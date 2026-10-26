package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	hopv1 "github.com/codefly-dev/authority/libraries/hop/go/gen/authority/hop/v1"
)

// audienceListContract is the shop's contract with its mandatory model binding
// and two optional bindings addressed to a LIST of owners the composition
// writes: owner names the owner protocol's calls, owner-review a bare ceiling
// qualified per owner.
const audienceListContract = `schema: codefly/module-contract/v1
principal: shop
namespaces: [shop]
queues: []
scope_ceilings: []
bindings:
  - id: model
    operations: [invoke]
    audience: {from: assistant/model-audience}
    resource_kind: {from: assistant/model-resource-kind}
    scope_ceiling:
      invoke: [invoke]
  - id: owner
    optional: true
    operations: [invoke, headless]
    audiences: {from: assistant/owner-audiences}
    calls:
      invoke:
        - /targets.v1.TargetService/Reach
      headless:
        - /targets.v1.TargetService/ListKinds
  - id: owner-review
    optional: true
    operations: [invoke]
    audiences: {from: assistant/owner-audiences}
    resource_kinds: {from: assistant/owner-resource-kinds}
    scope_ceiling:
      invoke: [read, review]
destinations: []
`

const bothOwners = "OWNER_AUDIENCES=robin,documents\nOWNER_RESOURCE_KINDS=robin=robin.answers,documents=documents.versions\n"

// targetPolicy is one owner's declaration for one owner-protocol method:
// what it requires.
type targetPolicy struct{ requires []*hopv1.Requirement }

// writeOwner composes one owner module publishing the owner protocol
// (targets.v1.TargetService) with its own policies, in its own vocabulary.
func writeOwner(t *testing.T, root, name string, policies map[string]targetPolicy) {
	t.Helper()
	service := &descriptorpb.ServiceDescriptorProto{Name: proto.String("TargetService")}
	for _, method := range []string{"Reach", "ListKinds"} {
		policy, served := policies[method]
		if !served {
			continue
		}
		m := &descriptorpb.MethodDescriptorProto{Name: proto.String(method), InputType: proto.String(".targets.v1.Empty"), OutputType: proto.String(".targets.v1.Empty"), Options: &descriptorpb.MethodOptions{}}
		proto.SetExtension(m.Options, hopv1.E_Policy, &hopv1.Policy{Requires: policy.requires})
		service.Method = append(service.Method, m)
	}
	set, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("targets/v1/api.proto"), Package: proto.String("targets.v1"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Empty")}}, Service: []*descriptorpb.ServiceDescriptorProto{service},
	}}})
	require.NoError(t, err)
	writeCallee(t, filepath.Join(root, "modules", name), name, "grpc", "protobuf", "targets.v1", "contract.binpb", set)
}

func requirement(kind string, actions ...string) []*hopv1.Requirement {
	return []*hopv1.Requirement{{ResourceKind: kind, Actions: actions}}
}

// audienceListWorkspace composes the shop beside robin and the document store,
// each serving the owner protocol under its own policies; documentsListKinds
// says whether the document store serves ListKinds.
func audienceListWorkspace(t *testing.T, documentsListKinds bool, assistant string) (*resources.Workspace, *resources.Module) {
	t.Helper()
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	configuration := filepath.Join(workspace.Dir(), resources.WorkspaceConfigurationName)
	config, err := os.ReadFile(configuration)
	require.NoError(t, err)
	require.Contains(t, string(config), "modules:\n  - name: shop\n")
	require.NoError(t, os.WriteFile(configuration, []byte(strings.Replace(string(config), "modules:\n  - name: shop\n", "modules:\n  - name: shop\n  - name: robin\n  - name: documents\n", 1)), 0o644))
	writeOwner(t, workspace.Dir(), "robin", map[string]targetPolicy{
		"Reach":     {requires: requirement("robin.answers", "read")},
		"ListKinds": {requires: requirement("robin.answers", "list")},
	})
	documents := map[string]targetPolicy{"Reach": {requires: append(requirement("documents.versions", "read"), requirement("documents", "read")...)}}
	if documentsListKinds {
		documents["ListKinds"] = targetPolicy{requires: requirement("documents.versions", "list")}
	}
	writeOwner(t, workspace.Dir(), "documents", documents)
	writeShopSlots(t, workspace.Dir(), module.Dir(), audienceListContract, assistant)
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

// TestABindingAddressedToTwoOwnersIsDeliveredOncePerOwner: each listed owner
// gets its own binding, <binding>:<owner>, with the ceiling read from THAT
// owner's own policies for the calls named, and the bare ceiling qualified by
// that owner's kind; the delivered manifest carries the same, and the render
// says which ids each declared binding became.
func TestABindingAddressedToTwoOwnersIsDeliveredOncePerOwner(t *testing.T) {
	workspace, module := audienceListWorkspace(t, true, bothOwners)
	env := selectedEnvironment(t, workspace, "staging")
	recorder := &infoRecorder{}
	ctx := wool.New(context.Background(), &wool.Resource{Kind: "test", Unique: "test"}).WithLogger(recorder).Inject(context.Background())
	request, err := authorityRequestOf(module)
	require.NoError(t, err)
	authority, err := moduleAuthority(ctx, workspace, module, loadServices(t, workspace, "shop", "api"), env, request)
	require.NoError(t, err)
	want := []solutionhost.AuthorityBinding{
		modelOnly,
		{ID: "owner:documents", Audience: "documents", Operations: map[string][]string{
			"invoke": {"documents.versions:read", "documents:read"}, "headless": {"documents.versions:list"},
		}},
		{ID: "owner:robin", Audience: "robin", Operations: map[string][]string{
			"invoke": {"robin.answers:read"}, "headless": {"robin.answers:list"},
		}},
		{ID: "owner-review:documents", Audience: "documents", Operations: map[string][]string{
			"invoke": {"documents.versions:read", "documents.versions:review"},
		}},
		{ID: "owner-review:robin", Audience: "robin", Operations: map[string][]string{
			"invoke": {"robin.answers:read", "robin.answers:review"},
		}},
	}
	require.Equal(t, want, authority.Bindings)
	_, manifest := renderShopManifest(t, workspace, module, env)
	require.Equal(t, want, manifest.Authority.Bindings, "the delivered manifest carries one binding per listed owner")
	logged := recorder.joined()
	require.Contains(t, logged, "module shop: binding owner is delivered once per listed audience, as owner:documents, owner:robin")
	require.Contains(t, logged, "module shop: binding owner-review is delivered once per listed audience, as owner-review:documents, owner-review:robin")
}

// TestAnOwnerCallOneListedOwnerDoesNotServeIsRefusedByName: the document store
// does not serve ListKinds, so the render refuses naming the binding, the call
// and that owner, and robin, which serves it, is not blamed.
func TestAnOwnerCallOneListedOwnerDoesNotServeIsRefusedByName(t *testing.T) {
	workspace, module := audienceListWorkspace(t, false, bothOwners)
	env := selectedEnvironment(t, workspace, "staging")
	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorIs(t, err, modulecontract.ErrUnresolvedCall)
	require.Contains(t, err.Error(), "binding owner headless call /targets.v1.TargetService/ListKinds at documents: documents does not serve it")
	require.NotContains(t, err.Error(), "at robin")
}

// TestAnOptionalOwnerListLeftEmptyIsOmitted: no owner listed — absent, or
// supplied empty — renders the shop without either binding, delivered
// manifest included, each omission logged.
func TestAnOptionalOwnerListLeftEmptyIsOmitted(t *testing.T) {
	for name, assistant := range map[string]string{
		"absent":         "",
		"supplied empty": "OWNER_AUDIENCES=\nOWNER_RESOURCE_KINDS=\n",
	} {
		t.Run(name, func(t *testing.T) {
			workspace, module := audienceListWorkspace(t, true, assistant)
			env := selectedEnvironment(t, workspace, "staging")
			recorder := &infoRecorder{}
			ctx := wool.New(context.Background(), &wool.Resource{Kind: "test", Unique: "test"}).WithLogger(recorder).Inject(context.Background())
			request, err := authorityRequestOf(module)
			require.NoError(t, err)
			authority, err := moduleAuthority(ctx, workspace, module, loadServices(t, workspace, "shop", "api"), env, request)
			require.NoError(t, err)
			require.Equal(t, []solutionhost.AuthorityBinding{modelOnly}, authority.Bindings)
			logged := recorder.joined()
			require.Contains(t, logged, "module shop: optional binding owner is omitted from the delivered manifest")
			require.Contains(t, logged, "module shop: optional binding owner-review is omitted from the delivered manifest")
			require.NotContains(t, logged, "delivered once per listed audience")
		})
	}
}

// TestASingleOwnerListDeliversOneBinding: a list of one is still delivered
// under the derived id, so a composition adding a second owner later adds a
// binding and renames none.
func TestASingleOwnerListDeliversOneBinding(t *testing.T) {
	workspace, module := audienceListWorkspace(t, true, "OWNER_AUDIENCES=robin\nOWNER_RESOURCE_KINDS=robin=robin.answers\n")
	env := selectedEnvironment(t, workspace, "staging")
	authority, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.NoError(t, err)
	ids := make([]string, 0, len(authority.Bindings))
	for _, binding := range authority.Bindings {
		ids = append(ids, binding.ID)
	}
	require.Equal(t, []string{"model", "owner:robin", "owner-review:robin"}, ids)
}
