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
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/network"
	"github.com/codefly-dev/core/resources"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type peer struct {
	root      string
	code      *codecore.DefaultCodeServer
	mu        sync.Mutex
	identity  *basev0.ServiceIdentity
	listeners []net.Listener
	load      *runtimev0.LoadStatus
	init      *runtimev0.InitStatus
	start     *runtimev0.StartStatus
}

func (p *peer) record(method string, request proto.Message) error {
	data, err := protojson.Marshal(request)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(p.root, ".protocol-test"), 0o700); err != nil {
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
	agentv0.UnimplementedAgentServer
	p *peer
}

func (d *discovery) GetAgentInformation(ctx context.Context, req *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	if err := d.p.record("Agent.GetAgentInformation", req); err != nil {
		return nil, err
	}
	declaration := contract.Current()
	declaration.Capabilities = nil
	switch os.Getenv("CODEFLY_TEST_PEER_CONTRACT") {
	case "missing":
		declaration.ProtocolVersion = 0
	case "future":
		declaration.ProtocolVersion++
	}
	return &agentv0.AgentInformation{Contract: declaration, Capabilities: []*agentv0.Capability{{Type: agentv0.Capability_RUNTIME}, {Type: agentv0.Capability_BUILDER}}}, nil
}

func (d *discovery) ListCommands(_ context.Context, req *agentv0.ListCommandsRequest) (*agentv0.ListCommandsResponse, error) {
	return &agentv0.ListCommandsResponse{}, d.p.record("Agent.ListCommands", req)
}

func (d *discovery) RunPluginCommand(_ context.Context, req *agentv0.RunPluginCommandRequest) (*agentv0.RunPluginCommandResponse, error) {
	return &agentv0.RunPluginCommandResponse{}, d.p.record("Agent.RunCommand", req)
}

type runtimePeer struct {
	runtimev0.UnimplementedRuntimeServer
	p *peer
}

func (p *peer) bindIdentity(ctx context.Context, identity *basev0.ServiceIdentity) (*resources.Service, error) {
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

func (r *runtimePeer) Load(ctx context.Context, req *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	service, err := r.p.bindIdentity(ctx, req.GetIdentity())
	if err != nil {
		return nil, err
	}
	if err := r.p.record("Runtime.Load", req); err != nil {
		return nil, err
	}
	service.WithModule(req.GetIdentity().GetModule())
	endpoints, err := service.LoadEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.identity = proto.Clone(req.GetIdentity()).(*basev0.ServiceIdentity)
	r.p.load = &runtimev0.LoadStatus{State: runtimev0.LoadStatus_READY}
	return &runtimev0.LoadResponse{Status: r.p.load, Endpoints: endpoints}, nil
}

func (r *runtimePeer) Init(_ context.Context, req *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	if err := r.p.record("Runtime.Init", req); err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	var mappings []*basev0.NetworkMapping
	var configs []*basev0.Configuration
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
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		mappings = append(mappings, &basev0.NetworkMapping{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{network.Native(endpoint, port)}})
		configs = append(configs, &basev0.Configuration{Origin: identity, Infos: []*basev0.ConfigurationInformation{{Name: "runtime", ConfigurationValues: []*basev0.ConfigurationValue{{Key: "connection", Value: "tcp://" + listener.Addr().String()}}}}})
	}
	r.p.init = &runtimev0.InitStatus{State: runtimev0.InitStatus_READY}
	return &runtimev0.InitResponse{Status: r.p.init, NetworkMappings: mappings, RuntimeConfigurations: configs}, nil
}

func (r *runtimePeer) Start(_ context.Context, req *runtimev0.StartRequest) (*runtimev0.StartResponse, error) {
	if err := r.p.record("Runtime.Start", req); err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.start = &runtimev0.StartStatus{State: runtimev0.StartStatus_STARTED, Generation: 1}
	return &runtimev0.StartResponse{Status: r.p.start}, nil
}

func (r *runtimePeer) Stop(_ context.Context, req *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	if err := r.p.record("Runtime.Stop", req); err != nil {
		return nil, err
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	for _, listener := range r.p.listeners {
		_ = listener.Close()
	}
	r.p.listeners = nil
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}

func (r *runtimePeer) Destroy(_ context.Context, req *runtimev0.DestroyRequest) (*runtimev0.DestroyResponse, error) {
	return &runtimev0.DestroyResponse{Status: &runtimev0.DestroyStatus{State: runtimev0.DestroyStatus_SUCCESS}}, r.p.record("Runtime.Destroy", req)
}

func (r *runtimePeer) Information(_ context.Context, _ *runtimev0.InformationRequest) (*runtimev0.InformationResponse, error) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return &runtimev0.InformationResponse{LoadStatus: r.p.load, InitStatus: r.p.init, StartStatus: r.p.start}, nil
}

func (r *runtimePeer) Test(_ context.Context, req *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	if err := r.p.record("Runtime.Test", req); err != nil {
		return nil, err
	}
	return &runtimev0.TestResponse{
		Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_SUCCESS},
		Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED},
		Counts: &runtimev0.TestCounts{Total: 1, Passed: 1},
		Run:    &runtimev0.TestRun{Runner: "cli-protocol-test", RequestedSelection: req.GetSelection(), SelectionId: req.GetSelectionId()},
	}, nil
}

type builderPeer struct {
	builderv0.UnimplementedBuilderServer
	p *peer
}

func (b *builderPeer) Load(ctx context.Context, req *builderv0.LoadRequest) (*builderv0.LoadResponse, error) {
	if _, err := b.p.bindIdentity(ctx, req.GetIdentity()); err != nil {
		return nil, err
	}
	return &builderv0.LoadResponse{State: &builderv0.LoadStatus{State: builderv0.LoadStatus_READY}}, b.p.record("Builder.Load", req)
}

func (b *builderPeer) Configure(_ context.Context, req *builderv0.ConfigureRequest) (*builderv0.ConfigureResponse, error) {
	return &builderv0.ConfigureResponse{State: &builderv0.ConfigureStatus{State: builderv0.ConfigureStatus_SUCCESS}, EffectiveYaml: "opaque: peer-response\n"}, b.p.record("Builder.Configure", req)
}

type codePeer struct {
	codev0.UnimplementedCodeServer
	p *peer
}

func (c *codePeer) Execute(ctx context.Context, req *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	if err := c.p.record("Code.Execute", req); err != nil {
		return nil, err
	}
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	var name string
	switch req.GetOperation().(type) {
	case *codev0.CodeRequest_GetProjectInfo:
		name = "project"
	case *codev0.CodeRequest_GetSemanticIndex:
		name = "semantic"
	}
	if name != "" {
		data, err := os.ReadFile(filepath.Join(c.p.root, ".protocol-test", name+".json"))
		if err == nil {
			response := &codev0.CodeResponse{}
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
