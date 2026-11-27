// This executable belongs only to CLI tests. It implements controlled protocol
// responses and records requests; it is not a released agent or a toolchain.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/agents/services"
	codecore "github.com/codefly-dev/core/code"
	"github.com/codefly-dev/core/code/semantic"
	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	codev1 "github.com/codefly-dev/core/generated/go/codefly/services/code/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type peer struct {
	root      string
	code      *codecore.DefaultCodeServer
	mu        sync.Mutex
	identity  *basev1.ServiceIdentity
	listeners []net.Listener
	load      *runtimev1.LoadStatus
	init      *runtimev1.InitStatus
	start     *runtimev1.StartStatus
}

func (p *peer) record(method string, request proto.Message) error {
	data, err := protojson.Marshal(request)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err = os.MkdirAll(filepath.Join(p.root, ".protocol-test"), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(p.root, ".protocol-test", "calls.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(struct {
		Method  string          `json:"method"`
		Request json.RawMessage `json:"request"`
		PID     int             `json:"pid"`
	}{method, data, os.Getpid()})
}

type discovery struct {
	agentv1.UnimplementedAgentServer
	p *peer
}

func (d *discovery) GetAgentInformation(_ context.Context, req *agentv1.AgentInformationRequest) (*agentv1.AgentInformation, error) {
	if err := d.p.record("Agent.GetAgentInformation", req); err != nil {
		return nil, err
	}
	declaration := contract.Current()
	declaration.Capabilities = nil
	if os.Getenv("CODEFLY_TEST_PEER_IMAGES") == "1" {
		declaration = contract.Current()
		declaration.Capabilities = append(declaration.Capabilities, contract.RuntimeInitImage)
	}
	switch os.Getenv("CODEFLY_TEST_PEER_CONTRACT") {
	case "missing":
		declaration.ProtocolVersion = 0
	case "future":
		declaration.ProtocolVersion++
	}
	info := &agentv1.AgentInformation{Contract: declaration, Capabilities: []*agentv1.Capability{{Type: agentv1.Capability_RUNTIME}, {Type: agentv1.Capability_BUILDER}}}
	switch os.Getenv("CODEFLY_TEST_PEER_VALIDATION") {
	case "unsupported":
		info.Validation = &agentv1.ValidationCapabilities{}
	case "supported", "dependencies", "stack":
		dependencyMode := agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE
		switch os.Getenv("CODEFLY_TEST_PEER_VALIDATION") {
		case "dependencies":
			dependencyMode = agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES
		case "stack":
			dependencyMode = agentv1.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK
		}
		info.Validation = &agentv1.ValidationCapabilities{
			Lint:    &agentv1.ValidationOperationCapability{Supported: true},
			Compile: &agentv1.ValidationOperationCapability{Supported: true},
			Test:    &agentv1.TestValidationCapability{Supported: true, Suites: []*agentv1.TestSuiteCapability{{Name: "unit", DefaultSuite: true, DependencyMode: dependencyMode}}},
		}
	}
	if os.Getenv("CODEFLY_TEST_PEER_IMAGES") == "1" && info.Validation != nil {
		info.Validation.ArtifactBuild = &agentv1.ValidationOperationCapability{Supported: true}
	}
	return info, nil
}

func (d *discovery) ListCommands(_ context.Context, req *agentv1.ListCommandsRequest) (*agentv1.ListCommandsResponse, error) {
	return &agentv1.ListCommandsResponse{}, d.p.record("Agent.ListCommands", req)
}

func (d *discovery) RunPluginCommand(_ context.Context, req *agentv1.RunPluginCommandRequest) (*agentv1.RunPluginCommandResponse, error) {
	return &agentv1.RunPluginCommandResponse{}, d.p.record("Agent.RunCommand", req)
}

type runtimePeer struct {
	runtimev1.UnimplementedRuntimeServer
	p *peer
}

func (p *peer) bindIdentity(ctx context.Context, identity *basev1.ServiceIdentity) (*resources.Service, error) {
	location := filepath.Join(identity.GetWorkspacePath(), identity.GetRelativeToWorkspace())
	service, err := resources.LoadServiceFromDir(ctx, location)
	if err != nil {
		return nil, err
	}
	service.WithModule(identity.GetModule())
	if source, ok := service.Spec["source-dir"].(string); ok {
		location = filepath.Join(location, source)
	}
	location, err = filepath.EvalSymlinks(location)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.root != location {
		if err := p.code.Close(); err != nil {
			return nil, err
		}
		p.root = location
		p.code = codecore.NewDefaultCodeServer(location, codecore.WithSemanticAnalyzer(semantic.New()))
	}
	return service, nil
}

func (r *runtimePeer) Load(ctx context.Context, req *runtimev1.LoadRequest) (*runtimev1.LoadResponse, error) {
	service, err := r.p.bindIdentity(ctx, req.GetIdentity())
	if err != nil {
		return nil, err
	}
	if err = r.p.record("Runtime.Load", req); err != nil {
		return nil, err
	}
	service.WithModule(req.GetIdentity().GetModule())
	endpoints, err := service.LoadEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.identity = proto.CloneOf(req.GetIdentity())
	r.p.load = &runtimev1.LoadStatus{State: runtimev1.LoadStatus_READY}
	return &runtimev1.LoadResponse{Status: r.p.load, Endpoints: endpoints}, nil
}

func (r *runtimePeer) Init(_ context.Context, req *runtimev1.InitRequest) (*runtimev1.InitResponse, error) {
	if err := r.p.record("Runtime.Init", req); err != nil {
		return nil, err
	}
	if message := os.Getenv("CODEFLY_TEST_PEER_INIT_ERROR"); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	var mappings []*basev1.NetworkMapping
	var configs []*basev1.Configuration
	for _, proposed := range req.GetProposedNetworkMappings() {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		r.p.listeners = append(r.p.listeners, listener)
		endpoint := proposed.GetEndpoint()
		identity := endpoint.GetModule() + "/" + endpoint.GetService()
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				_, _ = fmt.Fprintln(conn, identity)
				_ = conn.Close()
			}
		}()
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			return nil, fmt.Errorf("expected TCP listener, got %T", listener.Addr())
		}
		port := addr.AddrPort().Port()
		mappings = append(mappings, &basev1.NetworkMapping{Endpoint: endpoint, Instances: []*basev1.NetworkInstance{network.Native(endpoint, port)}})
		configs = append(configs, &basev1.Configuration{Origin: identity, Infos: []*basev1.ConfigurationInformation{{Name: "runtime", ConfigurationValues: []*basev1.ConfigurationValue{{Key: "connection", Value: "tcp://" + listener.Addr().String()}}}}})
	}
	r.p.init = &runtimev1.InitStatus{State: runtimev1.InitStatus_READY}
	return &runtimev1.InitResponse{Status: r.p.init, NetworkMappings: mappings, RuntimeConfigurations: configs}, nil
}

func (r *runtimePeer) Start(_ context.Context, req *runtimev1.StartRequest) (*runtimev1.StartResponse, error) {
	if err := r.p.record("Runtime.Start", req); err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.start = &runtimev1.StartStatus{State: runtimev1.StartStatus_STARTED, Generation: 1}
	return &runtimev1.StartResponse{Status: r.p.start}, nil
}

func (r *runtimePeer) Stop(_ context.Context, req *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	if err := r.p.record("Runtime.Stop", req); err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	for _, listener := range r.p.listeners {
		_ = listener.Close()
	}
	r.p.listeners = nil
	return &runtimev1.StopResponse{Status: &runtimev1.StopStatus{State: runtimev1.StopStatus_SUCCESS}}, nil
}

func (r *runtimePeer) Destroy(_ context.Context, req *runtimev1.DestroyRequest) (*runtimev1.DestroyResponse, error) {
	return &runtimev1.DestroyResponse{Status: &runtimev1.DestroyStatus{State: runtimev1.DestroyStatus_SUCCESS}}, r.p.record("Runtime.Destroy", req)
}

func (r *runtimePeer) Information(_ context.Context, _ *runtimev1.InformationRequest) (*runtimev1.InformationResponse, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return &runtimev1.InformationResponse{LoadStatus: r.p.load, InitStatus: r.p.init, StartStatus: r.p.start}, nil
}

func (r *runtimePeer) Test(_ context.Context, req *runtimev1.TestRequest) (*runtimev1.TestResponse, error) {
	if err := r.p.record("Runtime.Test", req); err != nil {
		return nil, err
	}
	resp, err := new(services.RuntimeWrapper).TestResponse()
	if err != nil {
		return nil, err
	}
	resp.Result = &runtimev1.TestRunResult{State: runtimev1.TestRunResult_PASSED}
	resp.Counts = &runtimev1.TestCounts{Total: 1, Passed: 1}
	resp.Run = &runtimev1.TestRun{Runner: "cli-protocol-test", RequestedSelection: req.GetSelection(), SelectionId: req.GetSelectionId()}
	return resp, nil
}

type builderPeer struct {
	builderv1.UnimplementedBuilderServer
	p *peer
}

func (b *builderPeer) Load(ctx context.Context, req *builderv1.LoadRequest) (*builderv1.LoadResponse, error) {
	if _, err := b.p.bindIdentity(ctx, req.GetIdentity()); err != nil {
		return nil, err
	}
	return &builderv1.LoadResponse{State: &builderv1.LoadStatus{State: builderv1.LoadStatus_READY}}, b.p.record("Builder.Load", req)
}

func (b *builderPeer) Configure(_ context.Context, req *builderv1.ConfigureRequest) (*builderv1.ConfigureResponse, error) {
	return &builderv1.ConfigureResponse{State: &builderv1.ConfigureStatus{State: builderv1.ConfigureStatus_SUCCESS}, EffectiveYaml: "opaque: peer-response\n"}, b.p.record("Builder.Configure", req)
}

type codePeer struct {
	codev1.UnimplementedCodeServer
	p *peer
}

func (c *codePeer) Execute(ctx context.Context, req *codev1.CodeRequest) (*codev1.CodeResponse, error) {
	if err := c.p.record("Code.Execute", req); err != nil {
		return nil, err
	}
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	var name string
	switch req.GetOperation().(type) {
	case *codev1.CodeRequest_GetProjectInfo:
		name = "project"
	case *codev1.CodeRequest_GetSemanticIndex:
		name = "semantic"
	}
	if name != "" {
		data, err := os.ReadFile(filepath.Join(c.p.root, ".protocol-test", name+".json"))
		if err == nil {
			response := &codev1.CodeResponse{}
			return response, protojson.Unmarshal(data, response)
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return c.p.code.Execute(ctx, req)
}

func main() {
	if os.Getenv(agents.WorkDirEnvironment) == "" {
		if err := os.Setenv(agents.WorkDirEnvironment, os.Getenv("CODEFLY_TEST_PEER_ROOT")); err != nil {
			panic(err)
		}
	}
	var base services.Base
	settings := struct {
		SourceDir string `yaml:"source-dir"`
	}{}
	root, err := base.ResolveSourceLocation(context.Background(), &settings, func() string { return settings.SourceDir })
	if err != nil {
		panic(err)
	}
	p := &peer{root: root, code: codecore.NewDefaultCodeServer(root, codecore.WithSemanticAnalyzer(semantic.New()))}
	defer func() { _ = p.code.Close() }()
	c := &codePeer{p: p}
	agents.Serve(agents.PluginRegistration{Agent: &discovery{p: p}, Runtime: &runtimePeer{p: p}, Builder: &builderPeer{p: p}, Code: c, Tooling: codecore.NewSourceTooling(c)})
}
