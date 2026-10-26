package generate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/core/composition"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/standards"
	"github.com/stretchr/testify/require"
	googleproto "google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// The bindings a playground generates are read by the run it starts, so they
// resolve in that run's naming scope: the declared one when --naming-scope is
// omitted, the flag's when it is passed, none when it is passed empty — the
// rule `codefly run` applies. --check holds the file to the scope it was
// prepared for.
func TestRunnableBindingsResolveInTheRunsNamingScope(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	declareLocalNamingScope(t, ctx, root, "alpha")
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")

	addresses := map[string]string{}
	for _, tc := range []struct {
		name, flag, scope string
		explicit          bool
	}{
		{name: "omitted keeps the declared scope", scope: "alpha"},
		{name: "an override replaces it", flag: "beta", explicit: true, scope: "beta"},
		{name: "explicitly empty clears it", flag: "", explicit: true, scope: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetRunnableBindingsFlags(t)
			if tc.explicit {
				require.NoError(t, RunnableBindingsCmd.Flags().Set("naming-scope", tc.flag))
			}
			require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
			want := nativeFixtureAddress(t, ctx, root, "documents", "runtime-worker", "connect", tc.scope)
			for _, value := range readPreparedValues(t, target) {
				prepared, err := corerunnable.DecodePrepared([]byte(value))
				require.NoError(t, err)
				require.Equal(t, want, prepared.GetCall().GetAddress())
			}
			addresses[tc.scope] = want
		})
	}
	require.Len(t, addresses, 3)
	require.NotEqual(t, addresses["alpha"], addresses["beta"], "two scopes resolve two addresses from one declaration")
	require.NotEqual(t, addresses["alpha"], addresses[""])

	// The file now holds the unscoped bindings: --check accepts that scope and
	// refuses the declared one, naming the command that regenerates it.
	resetRunnableBindingsFlags(t)
	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.Flags().Set("naming-scope", ""))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	resetRunnableBindingsFlags(t)
	runnableBindingsCheck = true
	err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	require.ErrorContains(t, err, "out of date")
	require.ErrorContains(t, err, "--naming-scope alpha")
}

// Through a declared call endpoint only the address moves: it is the hop's,
// resolved in the run's scope, while the operation, its Connect procedure, the
// contract, its digest and the policy are exactly the direct binding's.
func TestRunnableBindingsCallThroughTheDeclaredEndpoint(t *testing.T) {
	ctx := context.Background()
	second := markedMethod("ApplyTextAgain", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", conformingOperation())
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation(), second)), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	addFixtureHop(t, ctx, root)
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")

	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	direct := readPreparedValues(t, target)
	require.Len(t, direct, 2)

	require.NoError(t, RunnableBindingsCmd.Flags().Set("naming-scope", "rtpg"))
	require.NoError(t, RunnableBindingsCmd.Flags().Set("call-endpoint", "authority/hop/http"))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	through := readPreparedValues(t, target)
	require.Len(t, through, len(direct))

	hop := nativeFixtureAddress(t, ctx, root, "authority", "hop", "http", "rtpg")
	owner := nativeFixtureAddress(t, ctx, root, "documents", "runtime-worker", "connect", "rtpg")
	require.NotEqual(t, hop, owner)
	for key, value := range through {
		viaHop, err := corerunnable.DecodePrepared([]byte(value))
		require.NoError(t, err)
		straight, err := corerunnable.DecodePrepared([]byte(direct[key]))
		require.NoError(t, err)
		require.Equal(t, hop, viaHop.GetCall().GetAddress(), "%s is sent to the hop", key)
		require.NotEqual(t, straight.GetCall().GetAddress(), viaHop.GetCall().GetAddress())
		require.True(t, googleproto.Equal(straight.GetOperation(), viaHop.GetOperation()), "operation")
		require.True(t, googleproto.Equal(straight.GetCall().GetConnect(), viaHop.GetCall().GetConnect()), "procedure")
		require.Equal(t, straight.GetOperation().GetSpelling(), viaHop.GetCall().GetConnect().GetProcedure())
		require.True(t, googleproto.Equal(straight.GetContract(), viaHop.GetContract()), "contract")
		require.Equal(t, straight.GetContractDigest(), viaHop.GetContractDigest())
		require.True(t, googleproto.Equal(straight.GetPolicy(), viaHop.GetPolicy()), "policy")
	}

	// --check holds the file to the endpoint and scope too.
	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	runnableBindingsCallEndpoint = ""
	require.ErrorContains(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "out of date")
}

// A hop forwards a call; it serves nothing. An owner that does not serve the
// operation, serves no Connect endpoint, or publishes a stale contract is
// refused exactly as without one, and nothing is written.
func TestRunnableBindingsThroughAHopStillHoldTheOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		connect bool
		publish []string
		stale   bool
		want    string
	}{
		{name: "no connect endpoint", want: standards.CONNECT},
		{name: "served surface not published", connect: true, want: "no connect endpoint"},
		{name: "lookup not served", connect: true, publish: []string{applyTextMethod}, want: "no connect endpoint"},
		{name: "stale contract catalog", connect: true, publish: []string{applyTextMethod, conformingOperation().LookupMethod}, stale: true, want: "codefly generate contracts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var beside []*resources.Endpoint
			if tc.connect {
				beside = append(beside, connectEndpoint())
			}
			root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", beside...)
			t.Chdir(root)
			resetRunnablesFlags(t)
			resetRunnableBindingsFlags(t)
			require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
			if tc.publish != nil {
				publishFixtureConnectEndpoint(t, moduleDir, "connect", tc.publish)
			}
			if tc.stale {
				catalog, err := composition.LoadAPIContractCatalog(moduleDir)
				require.NoError(t, err)
				changed := descriptorSet(t, ingestionFile(conformingOperation(), markedMethod("Extra", ".documents.ingest.v1.ApplyTextRequest", ".documents.ingest.v1.ApplyTextResponse", nil)))
				require.NoError(t, os.WriteFile(filepath.Join(moduleDir, filepath.FromSlash(catalog.Endpoints[0].Path)), changed, 0o644))
			}
			addFixtureHop(t, ctx, root)
			require.NoError(t, RunnableBindingsCmd.Flags().Set("call-endpoint", "authority/hop/http"))
			err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
			require.ErrorContains(t, err, tc.want)
			require.NoFileExists(t, filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
		})
	}
}

// A call endpoint is a declared HTTP-based endpoint of a workspace module, or
// generation stops and says which part is wrong.
func TestRunnableBindingsRefuseAnUnusableCallEndpoint(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	addFixtureHop(t, ctx, root)
	for _, tc := range []struct{ coordinate, want string }{
		{"authority/hop", "names <module>/<service>/<endpoint>"},
		{"Authority/hop/http", "malformed"},
		{"nowhere/hop/http", "nowhere"},
		{"authority/nothing/http", "nothing"},
		{"authority/hop/missing", "declares no endpoint missing (it declares [grpc http])"},
		{"authority/hop/grpc", "is a grpc endpoint, and a prepared call is sent as HTTP"},
		{"authority/hop/http::grpc", "is a http endpoint"},
	} {
		t.Run(tc.coordinate, func(t *testing.T) {
			resetRunnableBindingsFlags(t)
			require.NoError(t, RunnableBindingsCmd.Flags().Set("call-endpoint", tc.coordinate))
			err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
			require.ErrorContains(t, err, tc.want)
			require.NoFileExists(t, filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env"))
		})
	}
}

// The environment declares where its calls go, so a plain invocation and its
// --check prepare through the hop with no flag; --call-endpoint stays an
// explicit override, and its empty value sends each call to its owner.
func TestAnEnvironmentDeclaresWhereItsRunnableCallsGo(t *testing.T) {
	ctx := context.Background()
	root, moduleDir := saveRunnableFixture(t, ctx, descriptorSet(t, ingestionFile(conformingOperation())), "0.1.0", connectEndpoint())
	t.Chdir(root)
	resetRunnablesFlags(t)
	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnablesCmd.RunE(RunnablesCmd, []string{"documents"}))
	publishFixtureConnectEndpoint(t, moduleDir, "connect", nil)
	addFixtureHop(t, ctx, root)
	declareLocalCallEndpoint(t, ctx, root, "authority/hop/http")
	target := filepath.Join(root, "configurations", "local", RunnableBindingsGroup+".env")
	addressOf := func() string {
		t.Helper()
		values := readPreparedValues(t, target)
		require.NotEmpty(t, values)
		var address string
		for _, value := range values {
			prepared, err := corerunnable.DecodePrepared([]byte(value))
			require.NoError(t, err)
			address = prepared.GetCall().GetAddress()
		}
		return address
	}

	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	hop := nativeFixtureAddress(t, ctx, root, "authority", "hop", "http", "")
	require.Equal(t, hop, addressOf(), "the declared endpoint is used with no flag")
	runnableBindingsCheck = true
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil), "a plain --check is deterministic against the declaration")

	resetRunnableBindingsFlags(t)
	require.NoError(t, RunnableBindingsCmd.Flags().Set("call-endpoint", ""))
	require.NoError(t, RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil))
	owner := nativeFixtureAddress(t, ctx, root, "documents", "runtime-worker", "connect", "")
	require.Equal(t, owner, addressOf(), "an explicit empty override sends each call to its owner")

	resetRunnableBindingsFlags(t)
	runnableBindingsCheck = true
	err := RunnableBindingsCmd.RunE(RunnableBindingsCmd, nil)
	require.ErrorContains(t, err, "out of date")
	require.ErrorContains(t, err, "runnable-bindings --env local`", "the declared endpoint needs no flag to regenerate")
}

// declareLocalCallEndpoint declares the local environment with a runnable call
// endpoint, as workspace.codefly.yaml spells it.
func declareLocalCallEndpoint(t *testing.T, ctx context.Context, root, endpoint string) {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	var value resources.YAMLValue
	require.NoError(t, yaml.Unmarshal([]byte(endpoint), &value))
	workspace.Environments = append(workspace.Environments, &resources.Environment{Name: "local", Extensions: map[string]resources.YAMLValue{"runnable-call-endpoint": value}})
	require.NoError(t, workspace.SaveToDirUnsafe(ctx, root))
}

// declareLocalNamingScope declares the local environment with a naming scope.
func declareLocalNamingScope(t *testing.T, ctx context.Context, root, scope string) {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	workspace.Environments = append(workspace.Environments, &resources.Environment{Name: "local", NamingScope: scope})
	require.NoError(t, workspace.SaveToDirUnsafe(ctx, root))
}

// addFixtureHop composes a second module whose hop declares a gRPC face and an
// HTTP face, as the authority's does. It serves and publishes nothing.
func addFixtureHop(t *testing.T, ctx context.Context, root string) {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	workspace.Modules = append(workspace.Modules, &resources.ModuleReference{Name: "authority"})
	require.NoError(t, workspace.SaveToDirUnsafe(ctx, root))
	moduleDir := filepath.Join(root, "modules", "authority")
	module := &resources.Module{Kind: resources.ModuleKind, Name: "authority", ServiceReferences: []*resources.ServiceReference{{Name: "hop"}}}
	module.WithDir(moduleDir)
	require.NoError(t, module.Save(ctx))
	service := &resources.Service{
		Name:    "hop",
		Version: "0.0.1",
		Agent:   &resources.Agent{Kind: resources.ServiceAgent, Name: "go-grpc", Version: "0.0.1", Publisher: "codefly.dev"},
		Endpoints: []*resources.Endpoint{
			{Name: "grpc", API: standards.GRPC, Visibility: resources.VisibilityPublic, Exposure: resources.ExposureNone},
			{Name: "http", API: standards.HTTP, Visibility: resources.VisibilityPublic, Exposure: resources.ExposureNone},
		},
	}
	service.WithDir(filepath.Join(moduleDir, "services", "hop"))
	require.NoError(t, service.Save(ctx))
}

// nativeFixtureAddress is the address the shared native resolver gives one
// declared endpoint in a naming scope, read from the saved declaration.
func nativeFixtureAddress(t *testing.T, ctx context.Context, root, moduleName, serviceName, endpointName, scope string) string {
	t.Helper()
	workspace, err := resources.LoadWorkspaceFromDir(ctx, root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(ctx, moduleName)
	require.NoError(t, err)
	service, err := module.LoadServiceFromName(ctx, serviceName)
	require.NoError(t, err)
	for _, endpoint := range service.Endpoints {
		if endpoint.Name == endpointName {
			resolved, err := common.ResolveNative(ctx, workspace.Name, moduleName, serviceName, scope, endpoint)
			require.NoError(t, err)
			require.NotEmpty(t, resolved.Address)
			return resolved.Address
		}
	}
	t.Fatalf("%s/%s declares no endpoint %s", moduleName, serviceName, endpointName)
	return ""
}
