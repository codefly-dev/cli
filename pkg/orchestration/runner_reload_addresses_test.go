package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	coreservices "github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
)

func mappingOn(module, service, endpoint string, port uint32) *basev0.NetworkMapping {
	return &basev0.NetworkMapping{
		Endpoint:  &basev0.Endpoint{Module: module, Service: service, Name: endpoint, Api: "rest"},
		Instances: []*basev0.NetworkInstance{{Host: "127.0.0.1", Port: port, Address: "http://127.0.0.1:1", Access: resources.NewNativeNetworkAccess()}},
	}
}

// The ports a service was accepted at are what core pins, keyed the way its
// override seam keys them; a mapping with no instance pins nothing.
func TestAcceptedPortsArePinnedByEndpointDestination(t *testing.T) {
	rest := mappingOn("app", "api", "rest", 51234)
	none := &basev0.NetworkMapping{Endpoint: &basev0.Endpoint{Module: "app", Service: "api", Name: "grpc", Api: "grpc"}}
	require.Equal(t, map[string]uint16{resources.EndpointDestination(rest.Endpoint): 51234}, acceptedPortOverrides([]*basev0.NetworkMapping{rest, none, nil}))
}

// A hot-reload agent keeps its process across a reload only while that
// process still serves where the shared state says it does.
func TestAHotReloadProcessWhoseAddressesMovedIsRestarted(t *testing.T) {
	before := []*basev0.NetworkMapping{mappingOn("app", "api", "rest", 51234)}
	runner := &Runner{networkMappings: before}
	require.False(t, runner.addressesMoved(), "never started: nothing moved")
	runner.markStarted()
	require.False(t, runner.addressesMoved(), "the addresses it was started with")
	runner.networkMappings = []*basev0.NetworkMapping{mappingOn("app", "api", "rest", 50695)}
	require.True(t, runner.addressesMoved(), "the shared state moved the service")
}

// The host gateway's route carriers are derived on every ask: a memoised set
// would hand the gateway a producer's first address for the rest of the run.
func TestTheGatewaysRoutesAreDerivedOnEveryAsk(t *testing.T) {
	world, workspace := hostRoutesWorld(t, SnapshotMode, nil)
	gateway := parityService(t, workspace, "host", "gateway")
	module, err := workspace.LoadModuleFromName(context.Background(), "host")
	require.NoError(t, err)
	identity, err := gateway.Identity()
	require.NoError(t, err)
	instance := &coreservices.Instance{Workspace: workspace, Module: module, Service: gateway, Identity: identity}
	runner := &Runner{instance: instance, world: world, runtimeContext: resources.RuntimeContextContainer}
	first := runner.hostRoutes()
	second := runner.hostRoutes()
	require.NotEmpty(t, first)
	require.Equal(t, first, second)
	require.NotEqual(t, reflect.ValueOf(first).Pointer(), reflect.ValueOf(second).Pointer(), "a fresh derivation each time, not one kept from the first ask")
}

// The output environment file is a map of the run's current environment: a
// key written again replaces its line, so a reader never meets two values for
// one key, and the latest is the true one.
func TestTheOutputEnvironmentFileKeepsOneValuePerKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "runtime.env")
	write := func(pairs ...string) {
		t.Helper()
		var environments []*resources.EnvironmentVariable
		for i := 0; i+1 < len(pairs); i += 2 {
			environments = append(environments, &resources.EnvironmentVariable{Key: pairs[i], Value: pairs[i+1]})
		}
		require.NoError(t, appendEnvironmentVariablesToFile(ctx, path, environments))
	}
	// Every key is a real carrier spelling: the writer now holds the whole
	// batch to the grammar the supported reader parses, so a placeholder like
	// "KEEP" is refused rather than written (output_records.go).
	write("CODEFLY__ENDPOINT__APP__API__REST__REST", "http://127.0.0.1:55634", "CODEFLY__KEPT", "one")
	write("CODEFLY__ENDPOINT__APP__API__REST__REST", "http://127.0.0.1:50695", "CODEFLY__ADDED", "two")
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "CODEFLY__ENDPOINT__APP__API__REST__REST=http://127.0.0.1:50695\nCODEFLY__KEPT=one\nCODEFLY__ADDED=two\n", string(content), "one line per key, in first-seen order, with the latest value")
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Equal(t, 1, strings.Count(string(content), "CODEFLY__ENDPOINT__APP__API__REST__REST="))

	link := filepath.Join(t.TempDir(), "link.env")
	require.NoError(t, os.Symlink(path, link))
	require.ErrorContains(t, appendEnvironmentVariablesToFile(ctx, link, nil), "symbolic link")
}
