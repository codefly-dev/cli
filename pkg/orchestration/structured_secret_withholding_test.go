package orchestration

import (
	"context"
	"testing"

	"github.com/codefly-dev/cli/pkg/remotenetwork"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// structuredSecretWorkspace puts a STRUCTURED secret in the composition root:
// `configurations/local/vault.secret.yaml`, which core loads as an information
// block carrying Data{Secret: true} and no configuration values at all. One
// service declares the group; the other declares nothing.
func structuredSecretWorkspace(t *testing.T) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: payments\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n    - name: api\n",
		// Declares nothing: whatever it receives is the composition root's.
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"modules/payments/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - vault\n",
		"configurations/local/vault.secret.yaml": "root-token: s3cret\npolicies:\n    - read\n",
		// A plain group beside it, so "the root still reaches a non-declarer" is
		// visible in the same resolution and the test cannot pass by delivering
		// nothing at all.
		"configurations/local/work-context.env": "authority-url=https://authority.example\n",
	})
}

// A composition root's STRUCTURED secret is withheld from a service that does
// not declare its group — in a run and in a render alike.
//
// Classifying values alone missed this entirely. Core loads `<name>.secret.yaml`
// as an information block whose Data carries Secret: true and which usually has
// no values, so there was nothing for a value-level classifier to withhold: the
// group came back untouched and reached every service of the composition. The
// credential narrowing said "under `run` and in a render alike" and was false
// for exactly the shape an operator reaches for when a credential is a document
// rather than a key. (Layer-5 round-five NEW-2.)
func TestACompositionRootsStructuredSecretIsWithheldFromANonDeclarer(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []Mode{RunMode, SnapshotMode} {
		t.Run(string(mode), func(t *testing.T) {
			workspace := structuredSecretWorkspace(t)
			world, worker := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = mode })

			confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
			require.NoError(t, err)
			require.NotContains(t, groupSet(confs), "vault",
				"a structured secret the service does not declare must not reach it")
			require.Contains(t, groupSet(confs), "work-context",
				"the root's non-secret groups still reach every service")

			// And the declarer does receive it, so this is a boundary and not a
			// group that stopped being delivered.
			api, err := loadService(ctx, t, workspace, "payments", "api")
			require.NoError(t, err)
			declared, err := world.workspaceConfigurationsFor(ctx, api, nil, resources.NewContainerNetworkAccess())
			require.NoError(t, err)
			require.Contains(t, groupSet(declared), "vault",
				"a service that declares the group receives the structured secret")
			require.True(t, structuredSecretData(declared, "vault"),
				"and receives it as the structured secret it is")
		})
	}
}

// The same withholding is what lets such a composition render at all.
//
// `promotableConfiguration` refuses a structured secret — it has no typed
// Kubernetes key reference to carry one — so while the root's `.secret.yaml`
// reached every service, a promotable render of ANY service of the composition
// failed with "requires typed Kubernetes key references". That is a regression
// this PR introduced by making the render resolve root groups, and it is closed
// by withholding rather than by relaxing the render: a service that does not ask
// for a structured secret has no reason to carry one into its manifest.
//
// A service that DOES declare such a group still cannot be rendered promotably.
// That limit is core's and predates this PR — a declared structured secret hit
// the same refusal before it — so it is left alone here.
func TestARenderWithAnUndeclaredRootStructuredSecretSucceeds(t *testing.T) {
	ctx := context.Background()
	world, worker := referenceValidityWorld(t, structuredSecretWorkspace(t),
		func(world *World) { world.Mode = SnapshotMode })

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err)

	_, safe, references, err := promotableDeploymentConfigurations(&basev0.Configuration{}, confs, "secret-worker")
	require.NoError(t, err,
		"a root structured secret this service never asked for must not stop it rendering")
	require.Empty(t, references, "and no secret reference is rendered for it")
	require.NotContains(t, groupSet(safe), "vault")
}

// structuredSecretData reports whether the named group arrived as a structured
// secret: an information block whose Data carries the secret flag.
func structuredSecretData(confs []*basev0.Configuration, group string) bool {
	for _, conf := range confs {
		for _, info := range conf.GetInfos() {
			if info.GetName() == group && info.GetData().GetSecret() {
				return true
			}
		}
	}
	return false
}

// A withheld root credential that carries an ${endpoint:…} is not reported as a
// lost value.
//
// The drop detection reads the outcome — every value whose loaded form carried a
// reference must still be there — and a credential withheld from a non-declarer
// is deliberately absent. Without the `withheld` set it would be judged as a
// value that did not survive resolution, which in a render is a refusal: a
// composition with a credential-named root value holding a reference could not
// render any service that does not declare the group. The two guards have to
// know about each other, and this is what says so.
func TestAWithheldRootCredentialIsNotJudgedAsALostValue(t *testing.T) {
	ctx := context.Background()
	workspace := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: public\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		// A credential-named key whose value is a reference: withheld from a
		// non-declarer, and carrying exactly what the drop detection looks for.
		"configurations/local/work-context.env": "authority-token=${endpoint:platform/authority/admin}\n" +
			"authority-url=https://authority.example\n",
	})
	world, worker := referenceValidityWorld(t, workspace)
	require.True(t, world.deploys(), "a render is where a lost value is refused")

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err,
		"a credential withheld on purpose must not be reported as a value that did not survive")
	_, delivered := groupValue(confs, "work-context", "authority-token")
	require.False(t, delivered, "and it really is withheld")
	_, other := groupValue(confs, "work-context", "authority-url")
	require.True(t, other, "the group's non-credential value still arrives")
}

// requireKnownRootGroup, through the resolution rather than on its own.
//
// It fires when a World's root-name source cannot name a group the manager
// delivers run-wide: producer discovery was planned without that group, so its
// references were resolved against mappings nobody bound and its values may be
// silently absent. The guard was tested directly but its CALL SITE was not, so
// deleting the call left every test green.
func TestTheResolutionRefusesARootGroupItDidNotPlanFor(t *testing.T) {
	loader := staticWorkspaceLoader{
		confs:       []*basev0.Configuration{workspaceConfiguration("work-context", "authority-url", "https://authority.example")},
		rootConfigs: []string{"work-context"},
	}
	// The manager knows the group is composition-root; the World cannot name it.
	// NewFlow binds both from one loader, so this is the inconsistency the guard
	// exists to refuse rather than ship.
	world := &World{ConfigurationManager: loadedWorkspaceManager(t, loader)}
	world.providedWorkspaceConfigurationInfos = func() []*basev0.ConfigurationInformation {
		return workspaceConfigurationInfos(loader.Configurations())
	}
	require.Empty(t, world.compositionRootWorkspaceConfigurationGroups(),
		"the case is a World whose root-name source is missing")

	_, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewNativeNetworkAccess())
	require.Error(t, err, "a root group the resolution never planned for must not be delivered")
	require.Contains(t, err.Error(), "work-context")
	require.Contains(t, err.Error(), "CompositionRootWorkspaceConfigurationNames")
}

// withheldCredentialWorkspace puts a credential in the composition root whose
// value references an endpoint only the producer's own module may reach, beside
// an ordinary value. One service declares the group; the other declares
// nothing.
func withheldCredentialWorkspace(t *testing.T, visibility string) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: " + visibility + "\n",
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n    - name: api\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"modules/payments/services/api/service.codefly.yaml": "kind: service\nname: api\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - work-context\n",
		"configurations/local/work-context.env": "authority-token=${endpoint:platform/authority/admin}\n" +
			"authority-url=https://authority.example\n",
	})
}

// A value a service does not receive imposes no obligation on it.
//
// The withholding used to happen at delivery, after the reference check and
// after producer discovery had both run over the whole effective set. So a root
// credential the consumer was deliberately not given still had to be valid and
// still had to resolve FOR that consumer: here, a credential referencing an
// endpoint private to the producer's module refused every service outside it —
// over a value none of them would ever read. (Layer-4 round-five F4.)
//
// The pair is the point. The same composition is refused for the service that
// DECLARES the group, because that one does receive the value and may not reach
// the endpoint, and resolves for the one that does not. The boundary is enforced
// exactly where the value goes.
func TestAWithheldCredentialsReferenceIsNotTheNonDeclarersObligation(t *testing.T) {
	ctx := context.Background()
	workspace := withheldCredentialWorkspace(t, "private")
	world, worker := referenceValidityWorld(t, workspace)

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err,
		"a credential this service is not given must not refuse it, whatever its value references")
	_, hasCredential := groupValue(confs, "work-context", "authority-token")
	require.False(t, hasCredential, "and it really is withheld")
	url, hasURL := groupValue(confs, "work-context", "authority-url")
	require.True(t, hasURL, "the group's ordinary value still arrives")
	require.Equal(t, "https://authority.example", url)

	// The declarer receives the value, so the boundary applies to it.
	api, err := loadService(ctx, t, workspace, "payments", "api")
	require.NoError(t, err)
	_, err = world.workspaceConfigurationsFor(ctx, api, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "the service that asks for the credential is held to the producer's export boundary")
	require.Contains(t, err.Error(), "is private to module")
}

// The same rule for the other obligation a reference imposes: that an address
// can be derived for it.
//
// In a render a reference whose producer's address cannot be derived is fatal,
// and rightly — a committed manifest missing a value stays invisible until a
// client dials it. But a withheld credential's producer is nobody's producer
// here: discovery does not look for it, so the render of a service that never
// receives the value is not refused over it.
func TestAWithheldCredentialsProducerIsNotDiscoveredForTheNonDeclarer(t *testing.T) {
	ctx := context.Background()
	workspace := withheldCredentialWorkspace(t, "public")
	world, worker := referenceValidityWorld(t, workspace, func(world *World) {
		// Nothing can turn the producer's identity into an address, which is
		// what a render refuses over — when the value is the consumer's.
		manager, err := remotenetwork.NewRemoteManager(ctx, nil)
		require.NoError(t, err)
		world.RemoteNetworkManager = manager
	})
	require.True(t, world.deploys())

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err,
		"a render must not be refused over an address only a withheld credential needed")
	_, hasCredential := groupValue(confs, "work-context", "authority-token")
	require.False(t, hasCredential)

	// And the declarer's render IS refused, with the derivation's own reason.
	api, err := loadService(ctx, t, workspace, "payments", "api")
	require.NoError(t, err)
	_, err = world.workspaceConfigurationsFor(ctx, api, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "the service that receives the credential still needs its address")
	require.Contains(t, err.Error(), "cannot derive the addresses of platform/authority")
}
