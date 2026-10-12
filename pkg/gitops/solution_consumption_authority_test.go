package gitops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
	solutionmanifest "github.com/codefly-dev/core/solution/manifest"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// consumingSolution is a solution that consumes the host's accounts with the
// viewer's own credential, a documents module it calls on a person's behalf,
// and a chat module it does not: the shape of a knowledge-base solution whose
// tools read passages for the asking person.
const consumingSolution = `schema_version: codefly.solution-manifest/v0
protocol_version: codefly.solution/v0
agent:
  publisher: codefly.dev
  kind: codefly:solution
  name: solution-generic
  version: 0.0.1
api:
  exposes:
    - id: http
      protocol: http
  consumes:
    - id: accounts
      protocol: connect
      module: saas
      service: accounts
      endpoint: connect
      as: accounts
    - id: documents
      protocol: rest
      module: documents
      service: documents
      endpoint: rest
      as: documents
      scopes:
        - resource_kind: documents.passages
          actions: [read]
        - resource_kind: documents.collections
          actions: [read]
    - id: chat
      protocol: http
      module: assistant
      service: chat
      endpoint: http
      as: assistant
lifecycle:
  package: true
  render: true
`

// writeConsumingSolutionWorkspace is writeAuthorityWorkspace with the module's
// contract replaced by a solution manifest: the only declaration a solution
// makes of what it consumes.
func writeConsumingSolutionWorkspace(t *testing.T, solution string) (*resources.Workspace, *resources.Module) {
	t.Helper()
	workspace, module := writeAuthorityWorkspace(t)
	require.NoError(t, os.Remove(filepath.Join(module.Dir(), modulecontract.FileName)))
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), solutionmanifest.FileName), []byte(solution), 0o644))
	return workspace, module
}

// THE DELIVERY HALF OF A SOLUTION'S CEILING: its declared consumption becomes
// the signed authority document's invoke unit for the module it calls on a
// person's behalf — addressed to that module, bounded by exactly the scopes it
// declared — and the consumptions that ask for no scopes become nothing.
func TestRenderDerivesAuthorityFromASolutionsDeclaredConsumption(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")
	units := []SolutionArtifactUnit{{Name: "api", Path: "services/api", Subject: "shop-api@example.iam.test"}}

	instances, undeclared, err := authorityInstancesOf(ctx, workspace, module, services, env, units, nil)
	require.NoError(t, err)
	require.Empty(t, undeclared)
	require.Len(t, instances, 1)

	destination, _ := renderShopAuthority(t, workspace, env, units, instances)
	document, _ := readDeliveredAuthority(t, destination, "staging", "acme.staging.shop-authority.yaml")
	require.Len(t, document.Principals, 1)
	require.Equal(t, "shop", document.Principals[0].Principal, "a composed solution's principal is the name it is composed under")
	require.Equal(t, []solutionhost.AuthorityBinding{
		{ID: "acme.staging.shop:documents:invoke", Revision: 1, Audience: "documents", Scope: "documents.collections:read,documents.passages:read"},
	}, document.Principals[0].Bindings, "one invoke unit, for the one consumption that asks for scopes")
	// Consumption contributes none of a module's own declarations, and says so.
	require.Empty(t, document.Queues)
	require.Empty(t, document.Namespaces)
	require.Empty(t, document.ScopeCeilings)
}

// A solution that consumes modules and asks for no scopes asks for no
// authority, and the render says why.
func TestASolutionAskingForNoScopesAsksForNoAuthority(t *testing.T) {
	ctx := context.Background()
	unscoped := strings.Replace(consumingSolution,
		"      scopes:\n        - resource_kind: documents.passages\n          actions: [read]\n        - resource_kind: documents.collections\n          actions: [read]\n", "", 1)
	require.NotEqual(t, consumingSolution, unscoped)
	workspace, module := writeConsumingSolutionWorkspace(t, unscoped)
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	instances, undeclared, err := authorityInstancesOf(ctx, workspace, module, services, env, nil, nil)

	require.NoError(t, err)
	require.Nil(t, instances)
	require.Contains(t, undeclared, "asks for no scopes on api.consumes")
}

// One request, from one declaration: a contract AND scoped consumption is
// refused rather than one silently chosen.
func TestASolutionCannotAskForAuthorityTwice(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	require.NoError(t, os.WriteFile(filepath.Join(module.Dir(), modulecontract.FileName), []byte(authorityContract), 0o644))
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	_, _, err := authorityInstancesOf(ctx, workspace, module, services, env, nil, nil)

	require.ErrorContains(t, err, "asks for authority from one declaration")
}

// The manifest an authority document is derived from is read strictly: a scope
// the manifest's own rules refuse fails the render, it is not ignored the way
// the run path's lenient read ignores what it does not need.
func TestAnInvalidConsumedScopeFailsTheRender(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeConsumingSolutionWorkspace(t, strings.Replace(consumingSolution, "actions: [read]\n        - resource_kind: documents.collections", "actions: [\"*\"]\n        - resource_kind: documents.collections", 1))
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	_, _, err := authorityInstancesOf(ctx, workspace, module, services, env, nil, nil)

	require.ErrorContains(t, err, "is not an action")
}

// Scoped consumption with no module-identity service is a request nothing would
// present: refused naming where it was asked, never dropped.
func TestScopedConsumptionWithoutAnIdentityIsRefused(t *testing.T) {
	ctx := context.Background()
	workspace, module := writeConsumingSolutionWorkspace(t, consumingSolution)
	service := filepath.Join(workspace.Dir(), "modules", "shop", "services", "api", resources.ServiceConfigurationName)
	data, err := os.ReadFile(service)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(service, []byte(strings.Replace(string(data), "module-identity: true\n", "", 1)), 0o644))
	env := selectedEnvironment(t, workspace, "staging")
	services := loadServices(t, workspace, "shop", "api")

	instances, _, err := authorityInstancesOf(ctx, workspace, module, services, env, nil, nil)

	require.Nil(t, instances)
	require.ErrorContains(t, err, "asks for scopes on api.consumes")
	require.ErrorContains(t, err, "module-identity")
}
