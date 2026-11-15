package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// uncomposed resolves a contract with no composition values for class: what a
// test of a contract whose slots need none reads its declaration through.
func uncomposed(class string) ContractResolver { return solutionhost.Resolver(nil, class, nil) }

// slotted is a module whose one binding's audience is a slot the composition
// must fill.
const slotted = `schema: codefly/module-contract/v1
principal: ledger
owner_aware: true
namespaces: []
queues: []
scope_ceilings:
    - resource_kind: ledger.entry
      actions: [read]
bindings:
    - id: store
      operations: [invoke]
      audience: {from: ledger/store-audience}
      scope_ceiling:
          invoke:
              - resource_kind: store.item
                actions: [read]
destinations: []
`

func slottedModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, modulecontract.FileName), []byte(slotted), 0o600))
	return dir
}

// Every reader of the authority's document reads core's one rendering, which
// resolves the contract: a slot the composition does not fill is refused by
// name, whichever field is read, rather than the field being read off the raw
// contract.
func TestEveryDocumentReaderRefusesAnUnfilledSlotByName(t *testing.T) {
	dir := slottedModule(t)
	_, ceilingsErr := ScopeCeilings(dir, uncomposed(""))
	_, rolesErr := Roles(dir, uncomposed(""))
	_, ownerErr := OwnerAware(dir, uncomposed(""))
	_, sharedErr := SharedKinds(dir, uncomposed(""))
	_, _, installationErr := InstallationDeclaration(dir, uncomposed(""))
	for name, err := range map[string]error{"ScopeCeilings": ceilingsErr, "Roles": rolesErr, "OwnerAware": ownerErr, "SharedKinds": sharedErr, "InstallationDeclaration": installationErr} {
		require.ErrorIs(t, err, modulecontract.ErrUnresolvedSlot, name)
		require.ErrorContains(t, err, "ledger/store-audience", name)
	}
}

// One module's declaration resolves its contract once, however many fields
// its readers read; outside a declaration every read resolves afresh.
func TestOneDeclarationResolvesTheContractOnce(t *testing.T) {
	dir := slottedModule(t)
	resolutions := 0
	counting := func(contract *modulecontract.Contract) (*modulecontract.Resolved, error) {
		resolutions++
		return contract.Resolve(modulecontract.MapValues{Public: map[string]map[string]string{"ledger": {"store-audience": "store"}}})
	}
	sources := AuthoritySources{
		Ceilings: func(ctx context.Context, _ string) ([]*authorityv1.WorkScopeV1, error) {
			return ScopeCeilings(dir, onceResolver(ctx, counting))
		},
		Roles: func(ctx context.Context, _ string) ([]*compositionv1.RoleV1, error) {
			return Roles(dir, onceResolver(ctx, counting))
		},
		OwnerAware: func(ctx context.Context, _ string) (bool, error) { return OwnerAware(dir, onceResolver(ctx, counting)) },
		SharedKinds: func(ctx context.Context, _ string) ([]string, error) {
			return SharedKinds(dir, onceResolver(ctx, counting))
		},
		Installation: func(ctx context.Context, _ string) (string, []*compositionv1.EdgeDependencyV1, error) {
			return InstallationDeclaration(dir, onceResolver(ctx, counting))
		},
	}
	declaration, err := sources.Declaration(context.Background(), "ledger")
	require.NoError(t, err)
	require.NotNil(t, declaration)
	require.Equal(t, 1, resolutions, "five readers of one module's declaration resolve its contract once")

	_, err = OwnerAware(dir, onceResolver(context.Background(), counting))
	require.NoError(t, err)
	_, err = OwnerAware(dir, onceResolver(context.Background(), counting))
	require.NoError(t, err)
	require.Equal(t, 3, resolutions, "outside a declaration each read resolves afresh")
}
