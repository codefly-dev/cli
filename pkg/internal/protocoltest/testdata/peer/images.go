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
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
	"google.golang.org/protobuf/proto"
)

func (b *builderPeer) Init(_ context.Context, req *builderv1.InitRequest) (*builderv1.InitResponse, error) {
	return &builderv1.InitResponse{State: &builderv1.InitStatus{State: builderv1.InitStatus_SUCCESS}}, b.p.record("Builder.Init", req)
}

func (*builderPeer) BuildCapabilities(context.Context, *builderv1.BuildCapabilitiesRequest) (*builderv1.BuildCapabilitiesResponse, error) {
	return &builderv1.BuildCapabilitiesResponse{BuildxSelection: true}, nil
}

func (b *builderPeer) Build(_ context.Context, req *builderv1.BuildRequest) (*builderv1.BuildResponse, error) {
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
	return &builderv1.BuildResponse{State: &builderv1.BuildStatus{State: builderv1.BuildStatus_SUCCESS}, Result: &builderv1.BuildResult{Kind: &builderv1.BuildResult_DockerBuildPlan{DockerBuildPlan: plan}}}, nil
}

func (b *builderPeer) SBOM(_ context.Context, req *builderv1.SBOMRequest) (*builderv1.SBOMResponse, error) {
	if err := b.p.record("Builder.SBOM", req); err != nil {
		return nil, err
	}
	if os.Getenv("CODEFLY_TEST_PEER_SBOM_ERROR") == "1" {
		return nil, fmt.Errorf("intentional test SBOM failure after owned build")
	}
	response := &builderv1.SBOMResponse{State: &builderv1.SBOMStatus{State: builderv1.SBOMStatus_COMPLETE}, Scope: builderv1.SBOMScope_SBOM_SCOPE_IMAGE}
	for _, subject := range req.Subjects {
		response.Images = append(response.Images, &builderv1.ImageSBOM{Digest: subject.Digest, Platform: subject.Platform, Subjects: []*builderv1.ImageSubject{proto.CloneOf(subject)}, Bom: &agentv1.Bom{Metadata: &agentv1.Metadata{Component: &agentv1.Component{Name: "test-only-subject", Type: agentv1.ComponentType_CONTAINER}}, Components: []*agentv1.Component{{Name: "test-only-inventory"}}}, Tool: "test-peer", Sha256: "test-only-checksum"})
	}
	return response, nil
}

func (r *runtimePeer) Lint(_ context.Context, req *runtimev1.LintRequest) (*runtimev1.LintResponse, error) {
	return &runtimev1.LintResponse{Status: &runtimev1.LintStatus{State: runtimev1.LintStatus_SUCCESS}}, r.p.record("Runtime.Lint", req)
}

func (r *runtimePeer) Build(_ context.Context, req *runtimev1.BuildRequest) (*runtimev1.BuildResponse, error) {
	return &runtimev1.BuildResponse{Status: &runtimev1.BuildStatus{State: runtimev1.BuildStatus_SUCCESS}}, r.p.record("Runtime.Build", req)
}
