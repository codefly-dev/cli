package orchestration

import (
	"context"
	"encoding/json"
	"fmt"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/authority/libraries/hop/go/routing"
	"github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// rpcHopWorkspace composes module `owner`, each of whose services publishes
// one protobuf contract (package <service>.v1, service Service, method Call)
// on a grpc and a connect endpoint, as robin, tracing, annotations and model
// do, with the foreign methods its module contract declares; and a hop service
// that reads the generated routes.
func rpcHopWorkspace(t *testing.T, services []string, foreign []string) *resources.Workspace {
	t.Helper()
	contract := "schema: codefly/module-contract/v1\nprincipal: owner\nnamespaces: [owner]\nqueues: []\nscope_ceilings: [{resource_kind: owner.things, actions: [read]}]\nbindings: []\n"
	if len(foreign) > 0 {
		contract += "foreign_methods:\n"
		for _, method := range foreign {
			contract += "  - method: " + method + "\n    requires: [{resource_kind: owner.things, actions: [read]}]\n"
		}
	}
	files := map[string]string{
		"workspace.codefly.yaml":                              "name: routes\nlayout: modules\nmodules:\n - name: owner\n - name: authority\n",
		"modules/owner/module.codefly.yaml":                   "name: owner\nservices:\n" + bulletList(services),
		"modules/owner/module.contract.codefly.yaml":          contract,
		"modules/authority/module.codefly.yaml":               "name: authority\nservices:\n - name: hop\n",
		"modules/authority/services/hop/service.codefly.yaml": "name: hop\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nworkspace-configuration-dependencies: [hop-routes]\n",
	}
	catalog := composition.APIContractCatalog{Schema: composition.APIContractCatalogSchema, Package: "owner/owner", Version: "1.0.0"}
	manifest := "schema: codefly/module-package/v2\nkind: module-package\nid: owner/owner\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n"
	for _, service := range services {
		pkg := service + ".v1"
		set := &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
			Name: proto.String(service + ".proto"), Package: proto.String(pkg), Syntax: proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Message")}},
			Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Service"), Method: []*descriptorpb.MethodDescriptorProto{{
				Name: proto.String("Call"), InputType: proto.String("." + pkg + ".Message"), OutputType: proto.String("." + pkg + ".Message"),
			}}}},
		}}}
		raw, err := proto.Marshal(set)
		require.NoError(t, err)
		path := "contracts/api/" + service + ".binpb"
		digest := composition.APIContractDigest(raw)
		files["modules/owner/"+path] = string(raw)
		files["modules/owner/services/"+service+"/service.codefly.yaml"] = "name: " + service + "\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\nendpoints:\n    - name: grpc\n      api: grpc\n    - name: connect\n      api: connect\n"
		manifest += fmt.Sprintf(" - name: %s\n   endpoints: [connect, grpc]\n   api-contracts:\n", service)
		for _, endpoint := range []string{"connect", "grpc"} {
			manifest += fmt.Sprintf("    - endpoint: %s\n      kind: protobuf\n      package: %s\n      path: %s\n      digest: %s\n", endpoint, pkg, path, digest)
			catalog.Endpoints = append(catalog.Endpoints, composition.APIContractEndpoint{Service: service, Endpoint: endpoint, API: endpoint, Kind: composition.APIContractKindProtobuf,
				Package: pkg, Path: path, Digest: digest, Services: composition.ProtobufServices(set, pkg)})
		}
	}
	raw, err := json.Marshal(catalog)
	require.NoError(t, err)
	files["modules/owner/"+composition.APIContractCatalogFileName] = string(raw)
	files["modules/owner/module.package.codefly.yaml"] = manifest
	return writeTempWorkspace(t, files)
}

func bulletList(names []string) string {
	var out strings.Builder
	for _, name := range names {
		out.WriteString(" - name: " + name + "\n")
	}
	return out.String()
}

func hopRoutesOf(t *testing.T, workspace *resources.Workspace) (routing.Routing, error) {
	t.Helper()
	hop := &basev0.Configuration{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{Name: "hop", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "MODULES", Value: "owner"}}}}}
	group, err := generatedHopRoutes(context.Background(), workspace, []*basev0.Configuration{hop})
	if err != nil {
		return routing.Routing{}, err
	}
	raw := hopRoutingSnapshot(group)
	require.NotEmpty(t, raw)
	routes, err := routing.DecodeRouting(raw)
	require.NoError(t, err)
	return *routes, nil
}

// A service publishing the same contract on a grpc and a connect endpoint is
// one destination since authority#31: the run plans its routes instead of
// refusing them as ambiguous, and a gRPC call resolves to the Connect
// listener, which serves gRPC too. The hop routing library before #31 refused
// this composition at plan time.
func TestHopRoutesPlanOneServicePublishedOnGRPCAndConnect(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	routes, err := hopRoutesOf(t, rpcHopWorkspace(t, []string{"chat"}, nil))
	require.NoError(t, err)
	require.Len(t, routes.Endpoints, 2)
	destination, _, err := routes.Resolve("owner", "grpc", "/chat.v1.Service/Call")
	require.NoError(t, err)
	require.Equal(t, "connect", destination.Endpoint)
	require.Equal(t, "${endpoint:owner/chat/connect}", destination.Address)
}

// A module's foreign methods — robin's side of annotations' owner protocol,
// tracing's OTLP Export — are routed to its one gRPC/Connect service, so the
// hop has a destination for every method the authority holds a policy for.
func TestHopRoutesCarryTheForeignMethodsOfTheOneRPCService(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	for name, foreign := range map[string][]string{
		"robin":   {"/annotations.v1.TargetAuthorityService/ListKinds", "/annotations.v1.TargetAuthorityService/CheckPinned", "/annotations.v1.TargetAuthorityService/Reach"},
		"tracing": {"/opentelemetry.proto.collector.trace.v1.TraceService/Export", "/opentelemetry.proto.collector.logs.v1.LogsService/Export"},
	} {
		t.Run(name, func(t *testing.T) {
			routes, err := hopRoutesOf(t, rpcHopWorkspace(t, []string{"chat"}, foreign))
			require.NoError(t, err)
			for _, endpoint := range routes.Endpoints {
				require.Subset(t, endpoint.Methods, foreign, "%s carries the foreign methods", endpoint.Endpoint)
				require.Contains(t, endpoint.Methods, "/chat.v1.Service/Call")
			}
			for _, method := range foreign {
				destination, _, err := routes.Resolve("owner", "grpc", method)
				require.NoError(t, err, method)
				require.Equal(t, "chat", destination.Service)
				require.Equal(t, "connect", destination.Endpoint)
			}
		})
	}
}

// A foreign method names no service, so a module with two gRPC/Connect
// services is refused by name rather than routed to a guess; one already
// published by a contract is refused as well.
func TestHopRoutesRefuseForeignMethodsTheyCannotPlace(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	_, err := hopRoutesOf(t, rpcHopWorkspace(t, []string{"chat", "host"}, []string{"/annotations.v1.TargetAuthorityService/ListKinds"}))
	require.ErrorContains(t, err, "hop routes for owner: its module contract declares foreign methods /annotations.v1.TargetAuthorityService/ListKinds, which name no service, and 2 services publish gRPC or Connect contracts [chat host]")
	_, err = hopRoutesOf(t, rpcHopWorkspace(t, []string{"chat"}, []string{"/chat.v1.Service/Call"}))
	require.ErrorContains(t, err, "foreign method /chat.v1.Service/Call is also declared by the contract chat/")
	// Without foreign methods, two services each on both listeners plan.
	_, err = hopRoutesOf(t, rpcHopWorkspace(t, []string{"chat", "host"}, nil))
	require.NoError(t, err)
}

func TestForeignMethodSelectsOnlyItsPublishedService(t *testing.T) {
	method := "/foreign.v1.Service/Read"
	endpoints := []routing.RouteEndpoint{
		{Service: "chat", Endpoint: "grpc", Transport: "grpc"},
		{Service: "chat", Endpoint: "connect", Transport: "connect"},
		{Service: "worker", Endpoint: "grpc", Transport: "grpc"},
		{Service: "web", Endpoint: "http", Transport: "http"},
	}
	foreign := []modulecontract.ForeignMethod{{Service: "chat", Method: method}}
	require.NoError(t, routeForeignMethods(endpoints, foreign))
	require.Equal(t, []string{method}, endpoints[0].Methods)
	require.Equal(t, []string{method}, endpoints[1].Methods)
	require.Empty(t, endpoints[2].Methods)
	require.ErrorContains(t, routeForeignMethods(endpoints, foreign), "also declared")
	foreign[0].Service = "missing"
	require.ErrorContains(t, routeForeignMethods(endpoints, foreign), "publishes no gRPC or Connect contract")
	foreign[0].Service = "web"
	require.ErrorContains(t, routeForeignMethods(endpoints, foreign), "publishes no gRPC or Connect contract")
}

func TestHopRoutesReadForeignServiceSelectionsFromContract(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := rpcHopWorkspace(t, []string{"chat", "worker"}, []string{"/foreign.v1.Service/Read", "/foreign.v1.Service/Execute"})
	file := filepath.Join(workspace.Dir(), "modules", "owner", modulecontract.FileName)
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	body := strings.Replace(string(raw), "  - method: /foreign.v1.Service/Read", "  - service: chat\n    method: /foreign.v1.Service/Read", 1)
	body = strings.Replace(body, "  - method: /foreign.v1.Service/Execute", "  - service: worker\n    method: /foreign.v1.Service/Execute", 1)
	require.NoError(t, os.WriteFile(file, []byte(body), 0600))
	routes, err := hopRoutesOf(t, workspace)
	require.NoError(t, err)
	for method, service := range map[string]string{"/foreign.v1.Service/Read": "chat", "/foreign.v1.Service/Execute": "worker"} {
		destination, _, err := routes.Resolve("owner", "grpc", method)
		require.NoError(t, err)
		require.Equal(t, service, destination.Service)
	}

}

// A host that stands a hop on some compositions and not others declares
// hop-routes always: saas's gateway is the hop on a cell and stands none where
// the composition names no module for it. Core's reader raises "no
// configuration found" from the DECLARATION, at runtime-init before any of the
// service's code runs, so the group must be delivered — EMPTY — rather than
// omitted. A composition that does name modules still receives its routes, and
// a workspace no service of which declares the group still receives nothing.
func TestHopRoutesAreDeliveredEmptyWhenNoModuleIsNamed(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := rpcHopWorkspace(t, []string{"chat"}, nil)
	hop := func(modules string) []*basev0.Configuration {
		return []*basev0.Configuration{{Origin: resources.ConfigurationWorkspace, Infos: []*basev0.ConfigurationInformation{{
			Name: "hop", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "MODULES", Value: modules}},
		}}}}
	}

	// No module named, and no hop group at all: the declaring service is handed
	// the group with an empty ROUTES, so its declaration resolves.
	for name, configs := range map[string][]*basev0.Configuration{
		"empty hop/MODULES": hop(""),
		"no hop group":      nil,
	} {
		t.Run(name, func(t *testing.T) {
			group, err := generatedHopRoutes(context.Background(), workspace, configs)
			require.NoError(t, err)
			require.NotNil(t, group, "a declared group delivered as nothing is refused by core's reader at runtime-init")
			require.Len(t, group.GetInfos(), 1)
			require.Equal(t, hopRoutesGroup, group.GetInfos()[0].GetName())
			values := group.GetInfos()[0].GetConfigurationValues()
			require.Len(t, values, 1)
			require.Equal(t, "ROUTES", values[0].GetKey())
			require.Empty(t, values[0].GetValue())
			// The group defines a value, so `codefly doctor workspace` reads it
			// from the same resolved inventory as the run instead of reporting
			// the group missing, and the routing snapshot is unchanged from no
			// group at all, so no run is told its routes moved.
			require.Empty(t, hopRoutingSnapshot(group))
		})
	}

	// Naming a module still generates that module's routes.
	routes, err := hopRoutesOf(t, workspace)
	require.NoError(t, err)
	require.Len(t, routes.Endpoints, 2)

	// A workspace no service of which declares the group receives nothing,
	// empty or not: there is no reader to deliver it to.
	none := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                              "name: routes\nlayout: modules\nmodules:\n - name: authority\n",
		"modules/authority/module.codefly.yaml":               "name: authority\nservices:\n - name: hop\n",
		"modules/authority/services/hop/service.codefly.yaml": "name: hop\nversion: 0.0.0\nagent: {kind: runtime::service, name: go-grpc, version: 0.0.16, publisher: codefly.ai}\n",
	})
	for _, configs := range [][]*basev0.Configuration{hop(""), hop("owner"), nil} {
		group, groupErr := generatedHopRoutes(context.Background(), none, configs)
		require.NoError(t, groupErr)
		require.Nil(t, group)
	}
}
