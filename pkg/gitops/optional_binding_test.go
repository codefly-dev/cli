package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// optionalBindingContract is the shop's contract with a mandatory model
// binding and an optional ratings binding whose audience — a module this
// workspace does not compose — is asked for by the calls it makes.
const optionalBindingContract = `schema: codefly/module-contract/v1
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
  - id: ratings
    optional: true
    operations: [invoke]
    audience: {from: assistant/ratings-audience}
    binding_key: {from: assistant/ratings-binding}
    calls:
      invoke:
        - /ratings.v1.RatingsService/Rate
destinations: []
`

// writtenCeiling is the same optional binding asking for a written ceiling,
// for the composition that fills it: it is delivered without the ratings
// module's declarations to read.
var writtenCeiling = strings.Replace(optionalBindingContract, `    calls:
      invoke:
        - /ratings.v1.RatingsService/Rate
`, `    scope_ceiling:
      invoke:
        - resource_kind: ratings.ratings
          actions: [write]
`, 1)

var modelOnly = solutionhost.AuthorityBinding{ID: "model", Audience: "model-gateway", Operations: map[string][]string{
	"invoke": {"modelservice.profiles:invoke"},
}}

// infoRecorder keeps every info line logged through the context.
type infoRecorder struct {
	mu   sync.Mutex
	logs []string
}

func (r *infoRecorder) Process(msg *wool.Log) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if msg.Level == wool.INFO {
		r.logs = append(r.logs, msg.Message)
	}
}

func (r *infoRecorder) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.logs, "\n")
}

// writeShopSlots replaces the composition's assistant group and the shop's
// contract.
func writeShopSlots(t *testing.T, workspaceDir, moduleDir, contract, assistant string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(moduleDir, modulecontract.FileName), []byte(contract), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "configurations", "staging", "assistant.env"),
		[]byte("MODEL_AUDIENCE=model-gateway\nMODEL_RESOURCE_KIND=modelservice.profiles\n"+assistant), 0o644))
}

// TestAnOptionalBindingNoSlotOfWhichIsFilledIsOmittedAndLogged: a composition
// that fills none of the optional binding's slots — absent, or supplied empty
// as a module's declared configuration supplies a key nobody set — renders
// without it, delivered manifest included, and the render logs the omission
// with its reason. The audience is not composed, and its calls are never read.
func TestAnOptionalBindingNoSlotOfWhichIsFilledIsOmittedAndLogged(t *testing.T) {
	for name, assistant := range map[string]string{
		"absent":         "",
		"supplied empty": "RATINGS_AUDIENCE=\nRATINGS_BINDING=\n",
	} {
		t.Run(name, func(t *testing.T) {
			workspace, module := writeAuthorityWorkspace(t)
			env := selectedEnvironment(t, workspace, "staging")
			writeShopSlots(t, workspace.Dir(), module.Dir(), optionalBindingContract, assistant)

			recorder := &infoRecorder{}
			ctx := wool.New(context.Background(), &wool.Resource{Kind: "test", Unique: "test"}).
				WithLogger(recorder).Inject(context.Background())
			request, err := authorityRequestOf(module)
			require.NoError(t, err)
			authority, err := moduleAuthority(ctx, workspace, module, loadServices(t, workspace, "shop", "api"), env, request)
			require.NoError(t, err)
			require.Equal(t, []solutionhost.AuthorityBinding{modelOnly}, authority.Bindings)
			logged := recorder.joined()
			require.Contains(t, logged, "module shop: optional binding ratings is omitted from the delivered manifest")
			require.Contains(t, logged, "audience ← assistant/ratings-audience, binding_key ← assistant/ratings-binding")

			_, manifest := renderShopManifest(t, workspace, module, env)
			require.Equal(t, []solutionhost.AuthorityBinding{modelOnly}, manifest.Authority.Bindings)
		})
	}
}

// TestAnOptionalBindingWhoseSlotsAreFilledIsDelivered: filled, it is a binding
// like any other.
func TestAnOptionalBindingWhoseSlotsAreFilledIsDelivered(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	writeShopSlots(t, workspace.Dir(), module.Dir(), writtenCeiling, "RATINGS_AUDIENCE=ratings\nRATINGS_BINDING=ratings\n")
	authority, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.NoError(t, err)
	require.Equal(t, []solutionhost.AuthorityBinding{modelOnly, {ID: "ratings", Audience: "ratings", Operations: map[string][]string{
		"invoke": {"ratings.ratings:write"},
	}}}, authority.Bindings)
}

// TestAnOptionalBindingFilledInPartIsRefusedByName: a composition that fills
// the audience and misspells the binding key meant to fill it; the binding is
// not dropped, the slot is refused by name.
func TestAnOptionalBindingFilledInPartIsRefusedByName(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	writeShopSlots(t, workspace.Dir(), module.Dir(), writtenCeiling, "RATINGS_AUDIENCE=ratings\nRATINGS_BINDNG=ratings\n")
	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorIs(t, err, modulecontract.ErrUnresolvedSlot)
	require.Contains(t, err.Error(), "optional binding ratings (filled in part, so resolved whole) binding_key ← assistant/ratings-binding")
}

// TestAMandatoryBindingUnfilledIsStillRefused: without optional, the same
// composition is refused as it always was.
func TestAMandatoryBindingUnfilledIsStillRefused(t *testing.T) {
	workspace, module := writeAuthorityWorkspace(t)
	env := selectedEnvironment(t, workspace, "staging")
	writeShopSlots(t, workspace.Dir(), module.Dir(), strings.Replace(writtenCeiling, "    optional: true\n", "", 1), "")
	_, err := shopAuthority(t, workspace, module, loadServices(t, workspace, "shop", "api"), env)
	require.ErrorIs(t, err, modulecontract.ErrUnresolvedSlot)
	require.Contains(t, err.Error(), "binding ratings audience ← assistant/ratings-audience")
}
