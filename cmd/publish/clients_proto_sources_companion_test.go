//go:build proto_companion_required

package publish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/generate"
	"github.com/codefly-dev/cli/pkg/generators"
	"github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

// `generate contracts` publishes a service's proto tree verbatim beside its
// contract, buf.yaml included. Over a tree with a vendor module and excluded
// folders (module "." excludes codefly and vendor; vendor is its own module),
// publish clients names the sources as buf does: the vendored file by its
// import path, never vendor/..., and the excluded facade not at all.
func TestPublishedContractSourcesFollowBufModulesThroughPinnedCompanion(t *testing.T) {
	moduleDir := t.TempDir()
	contractDir := filepath.Join(moduleDir, "contracts", "api", "accounts", "connect")
	contract := []byte("descriptor bytes")
	for rel, content := range map[string]string{
		"contract.binpb":                          string(contract),
		"proto/buf.yaml":                          "version: v2\nmodules:\n  - path: .\n    excludes: [codefly, vendor]\n  - path: vendor\n",
		"proto/accounts/v1/accounts.proto":        "syntax = \"proto3\";\npackage accounts.v1;\nimport \"codefly/base/v0/base.proto\";\nmessage Account { codefly.base.v0.Ref ref = 1; }\n",
		"proto/codefly/api.proto":                 "syntax = \"proto3\";\npackage codefly.facade;\nimport \"accounts/v1/accounts.proto\";\nmessage Facade { accounts.v1.Account account = 1; }\n",
		"proto/vendor/codefly/base/v0/base.proto": "syntax = \"proto3\";\npackage codefly.base.v0;\nmessage Ref { string id = 1; }\n",
	} {
		path := filepath.Join(contractDir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	plan := &generators.ModuleClientPlan{Endpoint: composition.APIContractEndpoint{
		Service: "accounts", Endpoint: "connect", Kind: composition.APIContractKindProtobuf,
		Path: "contracts/api/accounts/connect/contract.binpb", Digest: composition.APIContractDigest(contract),
	}}
	catalog := &composition.APIContractCatalog{Package: "codefly/saas-starter", Version: "0.1.0"}

	entry, err := generators.BuildModuleClientEntry(t.Context(), moduleDir, "saas-starter", catalog, plan, generate.ListProtoSources)
	require.NoError(t, err)
	var names []string
	for _, source := range entry.ProtoSources {
		names = append(names, source.Path)
	}
	require.Equal(t, []string{"accounts/v1/accounts.proto", "codefly/base/v0/base.proto"}, names)
}
