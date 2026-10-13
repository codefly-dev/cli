package orchestration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/codefly-dev/core/agents/contract"
	agentservices "github.com/codefly-dev/core/agents/services"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/services"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
)

type provenanceBuilderPeer struct {
	builderv0.UnimplementedBuilderServer
	requests chan *builderv0.DeploymentRequest
}

func (peer *provenanceBuilderPeer) Deploy(_ context.Context, request *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	peer.requests <- request
	return &builderv0.DeploymentResponse{State: &builderv0.DeploymentStatus{State: builderv0.DeploymentStatus_SUCCESS}}, nil
}

func TestDeploymentRequestCarriesRenderingComposition(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		name := "checkout"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			write := func(path, contents string) {
				path = filepath.Join(root, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
			}
			// The imported platform composes this package as saas, while the
			// package's physical cache contains its unrelated development workspace.
			coordinate := "    path: ../cache/module\n"
			if pinned {
				coordinate = "    source: github.com/codefly-dev/module-saas-starter\n    version: v0.0.93\n    module: module\n"
			}
			write("platform/workspace.codefly.yaml", "name: platform-core\nlayout: modules\nmodules:\n  - name: saas\n"+coordinate)
			write("product/workspace.codefly.yaml", "name: platform-obin\nlayout: modules\nworkspaces:\n  - name: platform-core\n    path: ../platform\nsolutions:\n  - name: lastlogin-go\n    path: solutions/lastlogin-go\n")
			write("product/solutions/lastlogin-go/module.codefly.yaml", "kind: module\nname: lastlogin-go\nservices: []\n")
			write("cache/workspace.codefly.yaml", "name: saas-starter-dev\nlayout: modules\nmodules:\n  - name: saas-starter\n    path: module\n")
			write("cache/module/module.codefly.yaml", "kind: module\nname: saas-starter\nservices: []\n")
			location := filepath.Join(root, "cache/module/services/accounts")
			require.NoError(t, os.MkdirAll(location, 0o755))
			workspace, err := resources.LoadWorkspaceFromDir(ctx, filepath.Join(root, "product"))
			require.NoError(t, err)
			repoWorkspace, err := resources.FindWorkspaceUpFrom(ctx, location)
			require.NoError(t, err)
			_, found := repoWorkspace.Member("saas-starter")
			require.True(t, found)
			_, found = repoWorkspace.Member("saas")
			require.False(t, found)
			if pinned {
				resolution, err := workspace.ResolveModule(ctx, workspace.Modules[0])
				require.NoError(t, err)
				require.Equal(t, resources.ResolutionPinned, resolution.Kind)
			}

			dependency := &resources.ServiceDependency{Module: "saas", Name: "cache", Kind: resources.DependencyKindRuntime}
			network := resources.NewNetworkInstance("cache", 6379)
			network.Access = resources.NewContainerNetworkAccess()
			mappings := []*basev0.NetworkMapping{{Endpoint: &basev0.Endpoint{Module: "saas", Service: "cache", Name: "redis", Api: "tcp", Visibility: resources.VisibilityInternal}, Instances: []*basev0.NetworkInstance{network}}}
			_, err = resources.ResolveDependencyNetworkMappings(repoWorkspace, "saas", []*resources.ServiceDependency{dependency}, mappings)
			require.ErrorIs(t, err, resources.ErrUnjudgedProvenance, "the old disk provenance refuses the composed name")

			peer := &provenanceBuilderPeer{requests: make(chan *builderv0.DeploymentRequest, 3)}
			server := grpc.NewServer()
			builderv0.RegisterBuilderServer(server, peer)
			conn, err := grpc.NewClient(serveGRPC(t, server), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			instance := &services.Instance{
				Workspace: repoWorkspace, // The handoff must replace even a stale instance workspace.
				Module:    &resources.Module{Name: "saas"},
				Service:   &resources.Service{Name: "accounts", ServiceDependencies: []*resources.ServiceDependency{dependency}},
				Info:      &agentv0.AgentInformation{Contract: &agentv0.AgentContract{Capabilities: []string{contract.DeploymentCompositionProvenance}}},
			}
			instance.Builder = &services.BuilderInstance{Instance: instance, Builder: agentservices.NewBuilderAgentClient(conn)}
			builder := &Builder{world: &World{Workspace: workspace}, instance: instance}
			request := &builderv0.DeploymentRequest{DependenciesNetworkMappings: mappings}
			_, err = builder.deployRequest(ctx, request)
			require.NoError(t, err)
			received := <-peer.requests
			require.Same(t, workspace, instance.Builder.Workspace)
			require.Equal(t, "dependency inputs accepted", agentDependencyJudgment(t, location, "saas", dependency, received))
			withoutProvenance := proto.CloneOf(received)
			withoutProvenance.CompositionProvenance = nil
			require.Contains(t, agentDependencyJudgment(t, location, "saas", dependency, withoutProvenance), resources.ErrUnjudgedProvenance.Error())

			require.Equal(t, []*builderv0.CompositionMember{
				{Name: "saas", Role: builderv0.CompositionMember_ROLE_MODULE, Workspace: "platform-core"},
				{Name: "lastlogin-go", Role: builderv0.CompositionMember_ROLE_SOLUTION, Workspace: "platform-obin"},
			}, received.GetCompositionProvenance().GetMembers())

			// No dependencies still carries the complete composition; this is not
			// conditional on a pin or the presence of a cross-module edge.
			_, err = builder.deployRequest(ctx, &builderv0.DeploymentRequest{})
			require.NoError(t, err)
			require.Equal(t, received.GetCompositionProvenance(), (<-peer.requests).GetCompositionProvenance())

			// This solution's own dev workspace incorrectly admits the route as
			// a module-to-module edge. The request restores the stricter verdict.
			write("solution-cache/workspace.codefly.yaml", "name: solution-dev\nlayout: modules\nmodules:\n  - name: lastlogin-go\n    path: module\n  - name: saas\n    path: ../cache/module\n")
			write("solution-cache/module/module.codefly.yaml", "kind: module\nname: lastlogin-go\nservices: []\n")
			solutionLocation := filepath.Join(root, "solution-cache/module/services/api")
			require.NoError(t, os.MkdirAll(solutionLocation, 0o755))
			require.Equal(t, "dependency inputs accepted", agentDependencyJudgment(t, solutionLocation, "lastlogin-go", dependency, withoutProvenance))
			require.Contains(t, agentDependencyJudgment(t, solutionLocation, "lastlogin-go", dependency, received), resources.ErrSolutionReachesThroughHost.Error())

			instance.Module.Name = "lastlogin-go"
			_, err = builder.deployRequest(ctx, request)
			require.ErrorIs(t, err, resources.ErrSolutionReachesThroughHost)
			require.Empty(t, peer.requests, "a solution's direct route must never reach the agent")

			instance.Info.Contract.Capabilities = nil
			_, err = builder.deployRequest(ctx, &builderv0.DeploymentRequest{})
			require.ErrorContains(t, err, contract.DeploymentCompositionProvenance)
			require.Empty(t, peer.requests, "an older agent must not silently ignore membership")
		})
	}
}

// Exercise Core's real request-backed agent judge with the received wire request.
// Stop at Prepare, after dependency collection, so this host-boundary regression
// needs neither provider templates/toolchains nor a cluster.
func agentDependencyJudgment(t *testing.T, location, module string, dependency *resources.ServiceDependency, received *builderv0.DeploymentRequest) string {
	t.Helper()
	base := &agentservices.Base{}
	require.NoError(t, base.HeadlessLoad(t.Context(), &basev0.ServiceIdentity{Name: "accounts", Module: module, WorkspacePath: location}))
	base.Service = &resources.Service{ServiceDependencies: []*resources.ServiceDependency{dependency}}
	base.EnvironmentVariables.SetIdentity(&basev0.ServiceIdentity{Name: "accounts", Module: module})
	wrapper := &agentservices.BuilderWrapper{Base: base}
	request := proto.CloneOf(received)
	request.Environment = &basev0.Environment{Name: "staging"}
	request.Deployment = &builderv0.Deployment{Kind: &builderv0.Deployment_Kubernetes{Kubernetes: &builderv0.KubernetesDeployment{
		Namespace: "test", Destination: t.TempDir(), Profile: builderv0.KubernetesOutputProfile_KUBERNETES_OUTPUT_PROFILE_EPHEMERAL_LOCAL_APPLY_V1,
	}}}
	response, err := wrapper.DeployKustomize(t.Context(), request, agentservices.KustomizeDeployment{
		EnvironmentVariables: base.EnvironmentVariables, Templates: fstest.MapFS{},
		Inputs: agentservices.DeploymentInputs{DependencyEndpoints: true},
		Prepare: func(context.Context, *agentservices.KustomizeDeploymentContext) error {
			return errors.New("dependency inputs accepted")
		},
	})
	require.NoError(t, err)
	return response.GetState().GetMessage()
}
