package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/stretchr/testify/require"
)

// authorityCore is the authority library's module. Its packages that decide
// (decide, authority, delivered, roleplan, and any added later) are the
// host's: it applies them to every delivery, and a product proves its roles
// against them in its own test (roleplan).
const authorityCore = "github.com/codefly-dev/authority/libraries/authority-core/go/"

// linkedAuthorityCore is all the CLI may link of it, tests included: declare,
// which projects a contract into the authority's document and decides
// nothing, and domain, whose role-name constants the scaffold writes.
var linkedAuthorityCore = []string{authorityCore + "declare", authorityCore + "domain"}

// The CLI carries no authorization policy, in its code or its tests: of the
// authority library it links only declare and domain, and the core it reads
// contracts with decides nothing about roles. A failure here is a policy rule
// creeping back into the CLI or core, which would bind every future rule
// change to a CLI release again: put the rule in authority-core's decide,
// where the host applies it, and prove it there.
func TestTheCLICarriesNoAuthorizationPolicy(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-test", "github.com/codefly-dev/cli/...").Output()
	require.NoError(t, err, "go list the CLI's dependencies, tests included")
	var linked []string
	for _, path := range strings.Fields(string(out)) {
		// A test variant reads "pkg [pkg.test]"; Fields splits it, so keep
		// the package paths alone.
		if strings.HasPrefix(path, authorityCore) && !slices.Contains(linked, path) {
			linked = append(linked, path)
		}
	}
	require.Contains(t, linked, authorityCore+"declare", "the CLI renders through declare; a listing without it lists something else")
	for _, path := range linked {
		require.Contains(t, linkedAuthorityCore, path, "the CLI links %s: only declare and domain of the authority library are the CLI's; a package that decides would make a policy change need a CLI release", path)
	}

	// Core reads roles as declared: every contract core's reader refused
	// before v0.19.32 for an authorization rule (its conformance fixtures,
	// kept here as core v0.19.31 shipped them) reads now. The host refuses
	// each where a new delivery is admitted (authority-core decide).
	fixtures, err := filepath.Glob(filepath.Join("testdata", "core-policy-fixtures", "*.yaml"))
	require.NoError(t, err)
	require.Len(t, fixtures, 16, "the sixteen contracts core refused for an authorization rule")
	for _, path := range fixtures {
		document, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = modulecontract.Parse(document)
		require.NoError(t, err, "core's contract reader decides a policy again: %s", filepath.Base(path))
	}
}
