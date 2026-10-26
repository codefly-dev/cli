package gitops

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// selfAudienceContract is the shop admitting work into itself: occurrences is
// addressed to the shop ({self: true}) and asks within the shop's own
// ceiling; occurrence-reads is the same, optional, left out by its binding
// key alone; beside them, the mandatory model binding and an optional binding
// addressed to a list of owners the composition writes.
const selfAudienceContract = `schema: codefly/module-contract/v1
principal: shop
namespaces: [shop]
queues: []
scope_ceilings:
  - resource_kind: shop.orders
    actions: [cancel, read, start]
bindings:
  - id: model
    operations: [invoke]
    audience: {from: assistant/model-audience}
    resource_kind: {from: assistant/model-resource-kind}
    scope_ceiling:
      invoke: [invoke]
  - id: occurrences
    operations: [invoke]
    audience: {self: true}
    scope_ceiling:
      invoke:
        - {resource_kind: shop.orders, actions: [cancel, start]}
  - id: occurrence-reads
    optional: true
    operations: [invoke]
    audience: {self: true}
    binding_key: {from: assistant/occurrence-reads-binding}
    scope_ceiling:
      invoke:
        - {resource_kind: shop.orders, actions: [read]}
  - id: owner-review
    optional: true
    operations: [invoke]
    audiences: {from: assistant/owner-audiences}
    resource_kinds: {from: assistant/owner-resource-kinds}
    scope_ceiling:
      invoke: [read, review]
destinations: []
`

var shopOccurrences = solutionhost.AuthorityBinding{ID: "occurrences", Audience: "shop", Operations: map[string][]string{
	"invoke": {"shop.orders:cancel", "shop.orders:start"},
}}

// selfAudienceWorkspace composes the shop with selfAudienceContract and the
// given assistant configuration beside the model's.
func selfAudienceWorkspace(t *testing.T, assistant string) (*resources.Workspace, *resources.Module) {
	t.Helper()
	ctx := context.Background()
	workspace, module := writeAuthorityWorkspace(t)
	writeShopSlots(t, workspace.Dir(), module.Dir(), selfAudienceContract, assistant)
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspace.Dir())
	require.NoError(t, err)
	module, err = workspace.LoadModuleFromName(ctx, "shop")
	require.NoError(t, err)
	return workspace, module
}

// TestASelfBindingIsAddressedToTheModulesPrincipal: the render resolves a
// binding addressed to the module itself to the principal it writes on the
// manifest — the module's own name — from no configuration value, and the
// delivered manifest carries the same. The optional self binding whose key
// nobody set is omitted, and the owner list is delivered once per owner.
func TestASelfBindingIsAddressedToTheModulesPrincipal(t *testing.T) {
	workspace, module := selfAudienceWorkspace(t, "OWNER_AUDIENCES=robin\nOWNER_RESOURCE_KINDS=robin=robin.answers\n")
	env := selectedEnvironment(t, workspace, "staging")
	recorder := &infoRecorder{}
	ctx := wool.New(context.Background(), &wool.Resource{Kind: "test", Unique: "test"}).WithLogger(recorder).Inject(context.Background())
	request, err := authorityRequestOf(module)
	require.NoError(t, err)
	authority, err := moduleAuthority(ctx, workspace, module, loadServices(t, workspace, "shop", "api"), env, request)
	require.NoError(t, err)
	require.Equal(t, module.Name, authority.Principal)
	want := []solutionhost.AuthorityBinding{
		modelOnly,
		shopOccurrences,
		{ID: "owner-review:robin", Audience: "robin", Operations: map[string][]string{"invoke": {"robin.answers:read", "robin.answers:review"}}},
	}
	require.Equal(t, want, authority.Bindings)
	require.Equal(t, authority.Principal, authority.Bindings[1].Audience, "the self binding is addressed to the manifest's own principal")
	require.Contains(t, recorder.joined(), "module shop: optional binding occurrence-reads is omitted from the delivered manifest")
	_, manifest := renderShopManifest(t, workspace, module, env)
	require.Equal(t, "shop", manifest.Authority.Principal)
	require.Equal(t, want, manifest.Authority.Bindings, "the delivered manifest carries the self binding at the module's principal")
}

// TestAnOptionalSelfBindingFilledIsDeliveredToTheModule: filling the optional
// self binding's one composed slot delivers it, addressed to the module; an
// empty owner list still omits the list binding beside it.
func TestAnOptionalSelfBindingFilledIsDeliveredToTheModule(t *testing.T) {
	workspace, module := selfAudienceWorkspace(t, "OCCURRENCE_READS_BINDING=reads\nOWNER_AUDIENCES=\nOWNER_RESOURCE_KINDS=\n")
	env := selectedEnvironment(t, workspace, "staging")
	authority, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.NoError(t, err)
	require.Equal(t, []solutionhost.AuthorityBinding{
		modelOnly,
		shopOccurrences,
		{ID: "occurrence-reads", Audience: "shop", Operations: map[string][]string{"invoke": {"shop.orders:read"}}},
	}, authority.Bindings)
}

// TestASelfBindingFollowsTheComposedNameNotAConfiguredOne: the self audience
// is the module's own name, so a contract claiming another module's principal
// is refused before anything resolves — never delivered with a self binding
// addressed to that other module.
func TestASelfBindingFollowsTheComposedNameNotAConfiguredOne(t *testing.T) {
	workspace, module := selfAudienceWorkspace(t, "")
	writeShopSlots(t, workspace.Dir(), module.Dir(), strings.Replace(selfAudienceContract, "principal: shop", "principal: robin", 1), "")
	env := selectedEnvironment(t, workspace, "staging")
	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorContains(t, err, `module shop publishes a contract whose principal is "robin"`)
}
