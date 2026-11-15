package gitops

import (
	"os/exec"
	"strings"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/stretchr/testify/require"
)

// policyPackages are the authority library's packages that decide: the host
// applies them to every delivery, and a product proves its roles against them
// in its own test (roleplan). The CLI links none of them, so a change to a role
// or permission rule never needs a CLI release; it renders through declare,
// which projects a contract and decides nothing, and names roles through
// domain's constants alone.
var policyPackages = []string{
	"github.com/codefly-dev/authority/libraries/authority-core/go/decide",
	"github.com/codefly-dev/authority/libraries/authority-core/go/authority",
	"github.com/codefly-dev/authority/libraries/authority-core/go/delivered",
	"github.com/codefly-dev/authority/libraries/authority-core/go/roleplan",
}

// The CLI carries no authorization policy: none of its packages links one that
// decides, and the core it reads contracts with decides nothing about roles.
// A failure here is a policy rule creeping back into the CLI or core, which
// would bind every future rule change to a CLI release again: put the rule in
// authority-core's decide, where the host applies it.
func TestTheCLICarriesNoAuthorizationPolicy(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "github.com/codefly-dev/cli/...").Output()
	require.NoError(t, err, "go list the CLI's dependencies")
	linked := map[string]bool{}
	for _, path := range strings.Fields(string(out)) {
		linked[path] = true
	}
	require.True(t, linked["github.com/codefly-dev/authority/libraries/authority-core/go/declare"], "the CLI renders through declare; a listing without it lists something else")
	for _, path := range policyPackages {
		require.False(t, linked[path], "the CLI links %s, which decides authorization: a policy change would need a CLI release", path)
	}

	// Core reads roles as declared: a contract declaring a role other than
	// admin and user, granting nothing core could hold it to, reads. Core once
	// refused it (role-admin-or-user, role-within-request); the host does now.
	_, err = modulecontract.Parse([]byte(`schema: codefly/module-contract/v1
principal: wiki
owner_aware: true
namespaces: []
queues: []
scope_ceilings:
    - resource_kind: wiki.page
      actions: [read]
roles:
    - name: viewer
      scopes:
          - resource_kind: elsewhere.thing
            actions: [delete]
bindings: []
`))
	require.NoError(t, err, "core's contract reader decides a role policy again")
}
