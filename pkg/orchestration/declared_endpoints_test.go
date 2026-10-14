package orchestration

import (
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// The endpoints a manifest declares and the agent's Load did not return are
// named, in the manifest's order; a Load that returns every declared endpoint
// drops none, and a service with no manifest declares none.
func TestDroppedDeclaredEndpointsAreTheOnesTheAgentDidNotReturn(t *testing.T) {
	service := &resources.Service{Name: "backend", Endpoints: []*resources.Endpoint{{Name: "http", API: "http"}, {Name: "grpc", API: "grpc"}}}
	require.Equal(t, []string{"http"}, droppedDeclaredEndpoints(service, []*basev0.Endpoint{{Name: "grpc"}}))
	require.Equal(t, []string{"http", "grpc"}, droppedDeclaredEndpoints(service, nil), "a Load that returns nothing drops every declared endpoint")
	require.Nil(t, droppedDeclaredEndpoints(service, []*basev0.Endpoint{{Name: "http"}, {Name: "grpc"}}))
	require.Nil(t, droppedDeclaredEndpoints(nil, nil))
}
