package gitops

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	authorityv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/authority/v1"
	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// deliverInto writes module's paired document into environment's delivered
// overlay through the render's own writer, so the reader is held to exactly
// what a render commits.
func deliverInto(t *testing.T, root, environment string, manifest *solutionhost.ModuleManifest, declaration *compositionv1.AuthorityManifestV1) string {
	t.Helper()
	signer := nativeSigner(t)
	raw, err := proto.Marshal(declaration)
	require.NoError(t, err)
	document, err := PreparedPairedDocument(signedCarrierOf(t, manifest, signer), raw, signer)
	require.NoError(t, err)
	overlay := filepath.Join(root, "deployments", "environments", environment, "modules", manifest.Alias, "module-manifests", "overlays", environment)
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
	catalog := preparedPairManifest("catalog", 3)
	catalog.Authority = &solutionhost.Authority{Principal: "catalog", InstallationRole: "none", ResourceKinds: []string{"catalog.entry"},
		Bindings: []solutionhost.AuthorityBinding{{ID: "types", Audience: "shelf", Operations: map[string][]string{"headless": {"store.types:define", "store.types:read"}}}}}
	overlay := deliverInto(t, root, "blue", catalog, &compositionv1.AuthorityManifestV1{InstallationRole: "none", OwnerAware: true,
		Methods: []*compositionv1.MethodPolicyV1{{Method: "/catalog.v1.Catalog/List", Requires: []*authorityv1.WorkScopeV1{{ResourceKind: "catalog.entry", Actions: []string{"read"}}}}}})
	writeRenderedEnvelope(t, overlay, `{"principals":{"catalog":{"operation_audiences":{"types":{"audience":"shelf","headless_scopes":[{"resource_kind":"store.types","actions":["define"]}]}}}}}`)

	delivered, err := ReadDeliveredAuthority(root, "blue")
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
	delivered, err = ReadDeliveredAuthority(root, "blue")
	require.NoError(t, err)
	require.Contains(t, delivered.EnvelopeUnread, "principals[catalog].operation_audiences[types]")

	// No envelope at all is said, and an environment with nothing rendered is an error, not an empty composition.
	require.NoError(t, os.Remove(filepath.Join(overlay, "module-envelope.yaml")))
	delivered, err = ReadDeliveredAuthority(root, "blue")
	require.NoError(t, err)
	require.Contains(t, delivered.EnvelopeUnread, "carries no module-envelope.yaml")
	_, err = ReadDeliveredAuthority(root, "green")
	require.ErrorContains(t, err, `no paired delivery document is rendered for environment "green"`)
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
