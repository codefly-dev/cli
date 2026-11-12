package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// deliverInto writes module's paired document into environment's overlay of
// the module trees at modules through the render's own writer, so the reader is
// held to exactly what a render commits.
func deliverInto(t *testing.T, modules, environment string, manifest *solutionhost.ModuleManifest, declaration *compositionv1.AuthorityManifestV1) string {
	t.Helper()
	signer := nativeSigner(t)
	raw, err := proto.Marshal(declaration)
	require.NoError(t, err)
	document, err := PreparedPairedDocument(signedCarrierOf(t, manifest, signer), raw, signer)
	require.NoError(t, err)
	overlay := filepath.Join(modules, manifest.Alias, "module-manifests", "overlays", environment)
	require.NoError(t, os.MkdirAll(overlay, 0o755))
	tree, err := newRenderRoot(overlay)
	require.NoError(t, err)
	require.NoError(t, writePairedDocumentInTree(tree, ".", "ns", manifest.Binding, document))
	return overlay
}

func writeRenderedEnvelope(t *testing.T, overlay, document string) {
	t.Helper()
	body := "apiVersion: v1\nkind: ConfigMap\ndata:\n  module-envelope.json: '" + document + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(overlay, "module-envelope.yaml"), []byte(body), 0o644))
}

func TestTheDeliveredTreeIsReadAsTheHostComposesIt(t *testing.T) {
	root := t.TempDir()
	trees := []string{workingrender.Modules(root, "blue"), filepath.Join(root, "deployments", "environments", "blue", "modules")}
	catalog := preparedPairManifest("catalog", 3)
	catalog.Authority = &solutionhost.Authority{Principal: "catalog", InstallationRole: "none", ResourceKinds: []string{"catalog.entry"},
		Bindings: []solutionhost.AuthorityBinding{{ID: "types", Audience: "shelf", Operations: map[string][]string{"headless": {"store.types:define", "store.types:read"}}}}}
	overlay := deliverInto(t, trees[1], "blue", catalog, &compositionv1.AuthorityManifestV1{InstallationRole: "none", OwnerAware: true,
		Methods: []*compositionv1.MethodPolicyV1{{Method: "/catalog.v1.Catalog/List", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "catalog.entry", Actions: []string{"read"}}}}}})
	writeRenderedEnvelope(t, overlay, `{"principals":{"catalog":{"operation_audiences":{"types":{"audience":"shelf","headless_scopes":[{"resource_kind":"store.types","actions":["define"]}]}}}}}`)

	delivered, err := ReadDeliveredAuthority(trees, "blue")
	require.NoError(t, err)
	module := delivered.Modules["catalog"]
	require.Empty(t, module.Unread)
	require.Contains(t, module.File, "catalog-paired.yaml", "every module names the file it was read from")
	require.True(t, module.Module.OwnerAware)
	require.Len(t, module.Module.Bindings["types"].HeadlessScopes[0].GetActions(), 2)
	require.Contains(t, module.Module.Methods, "/catalog.v1.Catalog/List")
	require.Empty(t, delivered.EnvelopeUnread)
	require.Equal(t, []string{"define"}, delivered.Envelope["catalog"]["types"].HeadlessScopes[0].GetActions())

	// A grant this cannot read is unread, never read as an envelope granting nothing.
	writeRenderedEnvelope(t, overlay, `{"principals":{"catalog":{"operation_audiences":{"types":{"audience":"shelf","headless_scopes":[{"resource_kind":"store.types","verbs":["define"]}]}}}}}`)
	delivered, err = ReadDeliveredAuthority(trees, "blue")
	require.NoError(t, err)
	require.Contains(t, delivered.EnvelopeUnread, "principals[catalog].operation_audiences[types]")

	// No envelope at all is said, and an environment with nothing rendered is an error, not an empty composition.
	require.NoError(t, os.Remove(filepath.Join(overlay, "module-envelope.yaml")))
	delivered, err = ReadDeliveredAuthority(trees, "blue")
	require.NoError(t, err)
	require.Contains(t, delivered.EnvelopeUnread, "carries no module-envelope.yaml")
	_, err = ReadDeliveredAuthority(trees, "green")
	require.ErrorContains(t, err, `no paired delivery document is rendered for environment "green"`)
}

// platform-obin's main kept 146 files under deployments/modules/*/…/overlays/blue/
// from the days a Blue render wrote into staging's tree. deployments/modules is
// staging's delivered tree, so for Blue it is a foreign one: what Blue delivers
// is read from Blue's working render and Blue's own delivered tree only, and
// the leftover can never outrank a fresh Blue render.
func TestAnotherEnvironmentsTreeNeverSpeaksForThisOne(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "environments:\n",
		"environments:\n  - name: blue\n    namespace: acme\n    gitops:\n      path: deployments/environments/blue\n"})
	root := workspace.Dir()
	blue, err := EnvironmentModuleTrees(workspace, selectedEnvironment(t, workspace, "blue"))
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "blue"), filepath.Join(root, "deployments", "environments", "blue", "modules")}, blue)
	staging, err := EnvironmentModuleTrees(workspace, selectedEnvironment(t, workspace, "staging"))
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "staging"), filepath.Join(root, "deployments", "modules")}, staging)

	declaration := &compositionv1.AuthorityManifestV1{InstallationRole: "none"}
	deliverInto(t, filepath.Join(root, "deployments", "modules"), "blue", preparedPairManifest("compute", 1), declaration)
	fresh := deliverInto(t, workingrender.Modules(root, "blue"), "blue", preparedPairManifest("compute", 2), declaration)
	delivered, err := ReadDeliveredAuthority(blue, "blue")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(fresh, "compute-paired.yaml"), delivered.Modules["compute"].File, "the leftover in staging's tree spoke for blue")
}

func TestAPermissionTableThisCannotReadIsAnErrorNotAnEmptyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions_gen.go")
	require.NoError(t, os.WriteFile(path, []byte("package permissions\n"), 0o644))
	_, err := PermissionTable(path)
	require.ErrorContains(t, err, "holds no method table")
	require.NoError(t, os.WriteFile(path, []byte("\t\"/a/B\": {AnyCheckedCaller: false, Requires: []Permission{Undefined}},\n"), 0o644))
	_, err = PermissionTable(path)
	require.ErrorContains(t, err, "which the file does not define")
}
