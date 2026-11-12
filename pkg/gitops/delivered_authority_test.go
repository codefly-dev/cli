package gitops

import (
	"context"
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
	blue, err := EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "blue"))
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "blue"), filepath.Join(root, "deployments", "environments", "blue", "modules")}, blue)
	staging, err := EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
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

// The condition this replaced could only decide anything when the workspace
// declared a top-level `gitops:` default to compare an environment's own
// repo-url against: with none, the checkout's tree was appended for every
// environment, and appended last, which is the position that wins. That is
// not a hypothetical workspace — environments commonly deliver into the very
// repository they are declared in, each on its own branch, and declare no
// default at all. TestAnotherEnvironmentsTreeNeverSpeaksForThisOne cannot
// reach it: its fixture declares no repo-url, so it never evaluates the
// branch. This one declares one.
func TestAnEnvironmentsCheckoutTreeIsReadOnlyOnItsDeliveryBranch(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "environments:\n",
		"environments:\n  - name: blue\n    namespace: acme\n    gitops:\n      repo-url: https://github.com/acme/example.git\n      path: deployments/environments/blue\n      branch: blue-delivery\n"})
	root := workspace.Dir()
	runGit(t, root, "init", "-q", "--initial-branch=main")
	blue := selectedEnvironment(t, workspace, "blue")

	trees, err := EnvironmentModuleTrees(context.Background(), workspace, blue)
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "blue")}, trees,
		"a checkout on main holds no Blue delivery, so nothing under Blue's gitops path speaks for Blue")

	// The whole point of excluding it: what the checkout committed must not
	// outrank what was just rendered.
	declaration := &compositionv1.AuthorityManifestV1{InstallationRole: "none"}
	deliverInto(t, filepath.Join(root, "deployments", "environments", "blue", "modules"), "blue", preparedPairManifest("compute", 1), declaration)
	fresh := deliverInto(t, workingrender.Modules(root, "blue"), "blue", preparedPairManifest("compute", 2), declaration)
	delivered, err := ReadDeliveredAuthority(trees, "blue")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(fresh, "compute-paired.yaml"), delivered.Modules["compute"].File,
		"the tree main committed spoke for Blue ahead of a fresh Blue render")

	// On Blue's own delivery branch the same tree is Blue's delivered tree,
	// and is read.
	runGit(t, root, "checkout", "-q", "-b", "blue-delivery")
	trees, err = EnvironmentModuleTrees(context.Background(), workspace, blue)
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "blue"),
		filepath.Join(root, "deployments", "environments", "blue", "modules")}, trees)

	// An environment naming no delivery repository, in a workspace declaring
	// no default, delivers nowhere else: its tree on disk is read as before,
	// and no branch is consulted for it.
	staging, err := EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "staging"))
	require.NoError(t, err)
	require.Equal(t, []string{workingrender.Modules(root, "staging"), filepath.Join(root, "deployments", "modules")}, staging)
}

// A branch that cannot be read is refused by name. Answering either way would
// decide silently what the reader asked about: include a tree that may be
// another branch's, or hide the only tree there is.
func TestADeliveryBranchThatCannotBeReadIsRefusedNotGuessed(t *testing.T) {
	workspace := writeCellWorkspace(t, [3]string{resources.WorkspaceConfigurationName, "environments:\n",
		"environments:\n  - name: blue\n    namespace: acme\n    gitops:\n      repo-url: https://github.com/acme/example.git\n      path: deployments/environments/blue\n      branch: blue-delivery\n"})
	_, err := EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "blue"))
	require.ErrorContains(t, err, `environment "blue" delivers to https://github.com/acme/example.git on branch "blue-delivery"`)
	require.ErrorContains(t, err, "this checkout's own branch could not be read")

	// A detached HEAD is the same refusal: git names it "HEAD", which is no
	// branch an environment can declare.
	runGit(t, workspace.Dir(), "init", "-q", "--initial-branch=main")
	runGit(t, workspace.Dir(), "add", "-A")
	runGit(t, workspace.Dir(), "commit", "-q", "-m", "the workspace")
	runGit(t, workspace.Dir(), "checkout", "-q", "--detach", "HEAD")
	_, err = EnvironmentModuleTrees(context.Background(), workspace, selectedEnvironment(t, workspace, "blue"))
	require.ErrorContains(t, err, "this checkout's own branch could not be read")
}
