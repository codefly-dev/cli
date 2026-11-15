package gitops

import (
	"context"
	"sync"

	"github.com/codefly-dev/core/solutionhost"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	modulecontract "github.com/codefly-dev/core/contracts/module"
)

// ContractResolver resolves a module contract against the composition it is
// delivered into (WorkspaceContractResolver): what the render reads to know
// which bindings the composition delivers and which class an edge's presence
// resolves for.
type ContractResolver func(*modulecontract.Contract) (*modulecontract.Resolved, error)

// oneResolution is the memo one module's declaration shares, by principal.
type oneResolution struct {
	mu sync.Mutex
	by map[string]resolution
}

type resolution struct {
	resolved *modulecontract.Resolved
	err      error
}

type oneResolutionKey struct{}

// withOneResolution scopes ctx to one module's declaration: every reader of it
// that resolves through onceResolver shares one resolution.
func withOneResolution(ctx context.Context) context.Context {
	return context.WithValue(ctx, oneResolutionKey{}, &oneResolution{by: map[string]resolution{}})
}

// onceResolver is resolve, answered once per contract principal within a
// context scoped by withOneResolution, and resolved every time outside one,
// so a long run never serves a resolution its composition has moved past.
func onceResolver(ctx context.Context, resolve ContractResolver) ContractResolver {
	memo, _ := ctx.Value(oneResolutionKey{}).(*oneResolution)
	if memo == nil {
		return resolve
	}
	return func(contract *modulecontract.Contract) (*modulecontract.Resolved, error) {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if held, ok := memo.by[contract.Principal]; ok {
			return held.resolved, held.err
		}
		resolved, err := resolve(contract)
		memo.by[contract.Principal] = resolution{resolved, err}
		return resolved, err
	}
}

// The contract's half of the authority's document, each field in the
// authority's spelling. Core renders it (solutionhost.DeclareModule), the one
// render every producer calls: the CLI reads its fields here and decides
// nothing about them. Each reads the module contract at dir, resolved by
// resolve; a module that publishes no contract declares none, and one still
// shipping the retired roles declaration is refused naming it.

// ownDocument is the module at dir's half of the authority's document, as core
// renders it: nil when it publishes no contract.
func ownDocument(dir string, resolve ContractResolver) (*solutionhost.AuthorityDocument, error) {
	declaration, _, err := solutionhost.DeclareModule(dir, "", nil, resolve)
	if err != nil || declaration == nil {
		return nil, err
	}
	return &declaration.Document, nil
}

// Roles reads the roles the module at dir declares, in the order written:
// what the authority's document carries for it (AuthorityManifestV1.roles).
// A scope naming through is carried in the role's through, per binding in the
// order first named, only when the composition delivers that binding.
func Roles(dir string, resolve ContractResolver) ([]*compositionv1.RoleV1, error) {
	document, err := ownDocument(dir, resolve)
	if err != nil || document == nil {
		return nil, err
	}
	var out []*compositionv1.RoleV1
	for _, role := range document.Roles {
		var through []*compositionv1.RoleThroughV1
		for _, t := range role.Through {
			through = append(through, &compositionv1.RoleThroughV1{Binding: t.Binding, Scopes: wireScopes(t.Scopes)})
		}
		out = append(out, &compositionv1.RoleV1{Name: role.Name, Scopes: wireScopes(role.Scopes),
			InstallationOwnedScopes: wireScopes(role.InstallationOwnedScopes), Through: through})
	}
	return out, nil
}

// ScopeCeilings reads the module's own resource kinds with the actions
// declared on each (its contract's scope_ceilings), a solution's as much as a
// module's.
func ScopeCeilings(dir string, resolve ContractResolver) ([]*authorityv1.WorkScopeV1, error) {
	document, err := ownDocument(dir, resolve)
	if err != nil || document == nil {
		return nil, err
	}
	out := make([]*authorityv1.WorkScopeV1, 0, len(document.ScopeCeilings))
	return append(out, wireScopes(document.ScopeCeilings)...), nil
}

// SharedKinds reads the kinds the module declares shared (a scope ceiling's
// shared: true), in the order written: kinds no installation owns, which the
// authority issues unselected through a solution.
func SharedKinds(dir string, resolve ContractResolver) ([]string, error) {
	document, err := ownDocument(dir, resolve)
	if err != nil || document == nil {
		return nil, err
	}
	return document.SharedKinds, nil
}

// InstallationDeclaration reads the module's installation declaration: the
// agent's standing role selector, carried verbatim, and the modules it reaches
// with the person's Work Context, each edge's presence resolved for the class
// resolve resolves for, because the Core signed manifest carries that presence
// and the paired agreement rule compares the two carriers byte for byte. nil
// and [] are equivalent zero edges, and a repeated proto field cannot carry
// the difference, so none is nil.
func InstallationDeclaration(dir string, resolve ContractResolver) (string, []*compositionv1.EdgeDependencyV1, error) {
	document, err := ownDocument(dir, resolve)
	if err != nil || document == nil {
		return "", nil, err
	}
	var edges []*compositionv1.EdgeDependencyV1
	for _, edge := range document.EdgeDependencies {
		edges = append(edges, &compositionv1.EdgeDependencyV1{Module: edge.Module, Presence: edge.Presence})
	}
	return document.InstallationRole, edges, nil
}

// OwnerAware reads the module's explicit declaration that it checks resource
// owners. An omitted declaration means false, never an inference from the
// presence of roles or installation-owned scopes.
func OwnerAware(dir string, resolve ContractResolver) (bool, error) {
	document, err := ownDocument(dir, resolve)
	if err != nil || document == nil {
		return false, err
	}
	return document.OwnerAware, nil
}

// wireScopes spells core's declared scopes as the authority's interface does:
// nil for none.
func wireScopes(scopes []solutionhost.DeclaredScope) []*authorityv1.WorkScopeV1 {
	if len(scopes) == 0 {
		return nil
	}
	out := make([]*authorityv1.WorkScopeV1, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, &authorityv1.WorkScopeV1{ResourceKind: scope.ResourceKind, Actions: append([]string(nil), scope.Actions...)})
	}
	return out
}
