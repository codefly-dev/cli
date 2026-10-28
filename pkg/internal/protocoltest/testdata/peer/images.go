package main

// Optional recipe behavior for CLI-owned image boundary tests. It never calls
// Docker; the CLI remains the builder. Inventory below is synthetic transport
// evidence, not a package scan.
import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	coreservices "github.com/codefly-dev/core/agents/services"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"google.golang.org/protobuf/proto"
)

func (b *builderPeer) Init(_ context.Context, req *builderv0.InitRequest) (*builderv0.InitResponse, error) {
	return &builderv0.InitResponse{State: &builderv0.InitStatus{State: builderv0.InitStatus_SUCCESS}}, b.p.record("Builder.Init", req)
}

func (*builderPeer) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{BuildxSelection: true}, nil
}

func (b *builderPeer) Build(_ context.Context, req *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	if err := b.p.record("Builder.Build", req); err != nil {
		return nil, err
	}
	if os.Getenv("CODEFLY_TEST_PEER_IMAGES") != "1" {
		return nil, fmt.Errorf("test recipe behavior was not selected")
	}
	output := req.GetOutputDirectory()
	if err := os.MkdirAll(output, 0700); err != nil {
		return nil, err
	}
	for name, contents := range map[string]string{"Dockerfile": "FROM scratch\nCOPY code /app\n", "dockerignore": ".protocol-test\n"} {
		if err := os.WriteFile(filepath.Join(output, name), []byte(contents), 0600); err != nil {
			return nil, err
		}
	}
	plan, err := coreservices.SingleImageBuildPlan(output, "codefly-dev/app/subject:0.0.0", []string{"linux/arm64", "linux/amd64"}, []string{"Dockerfile", "dockerignore"})
	if err != nil {
		return nil, err
	}
	if err := b.p.record("Builder.EmittedPlan", plan); err != nil {
		return nil, err
	}
	return &builderv0.BuildResponse{State: &builderv0.BuildStatus{State: builderv0.BuildStatus_SUCCESS}, Result: &builderv0.BuildResult{Kind: &builderv0.BuildResult_DockerBuildPlan{DockerBuildPlan: plan}}}, nil
}

func (b *builderPeer) SBOM(_ context.Context, req *builderv0.SBOMRequest) (*builderv0.SBOMResponse, error) {
	if err := b.p.record("Builder.SBOM", req); err != nil {
		return nil, err
	}
	if os.Getenv("CODEFLY_TEST_PEER_SBOM_ERROR") == "1" {
		return nil, fmt.Errorf("intentional test SBOM failure after owned build")
	}
	response := &builderv0.SBOMResponse{State: &builderv0.SBOMStatus{State: builderv0.SBOMStatus_COMPLETE}, Scope: builderv0.SBOMScope_SBOM_SCOPE_IMAGE}
	for _, subject := range req.Subjects {
		response.Images = append(response.Images, &builderv0.ImageSBOM{Digest: subject.Digest, Platform: subject.Platform, Subjects: []*builderv0.ImageSubject{proto.CloneOf(subject)}, Bom: &agentv0.Bom{Metadata: &agentv0.Metadata{Component: &agentv0.Component{Name: "test-only-subject", Type: agentv0.ComponentType_CONTAINER}}, Components: []*agentv0.Component{{Name: "test-only-inventory"}}}, Tool: "test-peer", Sha256: "test-only-checksum"})
	}
	return response, nil
}

func (r *runtimePeer) Lint(_ context.Context, req *runtimev0.LintRequest) (*runtimev0.LintResponse, error) {
	return &runtimev0.LintResponse{Status: &runtimev0.LintStatus{State: runtimev0.LintStatus_SUCCESS}}, r.p.record("Runtime.Lint", req)
}

func (r *runtimePeer) Build(_ context.Context, req *runtimev0.BuildRequest) (*runtimev0.BuildResponse, error) {
	return &runtimev0.BuildResponse{Status: &runtimev0.BuildStatus{State: runtimev0.BuildStatus_SUCCESS}}, r.p.record("Runtime.Build", req)
}
