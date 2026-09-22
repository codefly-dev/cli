package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func recordCI(method string) error {
	file, err := os.OpenFile(os.Getenv("TEST_SOURCE_CI_RECORD"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(file, "%s %s\n", method, executable)
	return err
}

// CI tests stop at a deliberate Package refusal after checking that validation
// and packaging reached the same privately selected executable. Tests that need
// the later stages point TEST_SOURCE_PACKAGE_EXECUTABLE at the bytes to emit.
func (*discoveryBuilder) Package(_ context.Context, req *builderv0.PackageRequest) (*builderv0.PackageResponse, error) {
	if err := recordCI("Builder.Package"); err != nil {
		return nil, err
	}
	template := os.Getenv("TEST_SOURCE_PACKAGE_EXECUTABLE")
	if template == "" {
		return nil, status.Error(codes.FailedPrecondition, "CI package boundary reached")
	}
	payload, err := os.ReadFile(template)
	if err != nil {
		return nil, err
	}
	var artifacts []*builderv0.PackageArtifact
	for _, target := range req.GetTargets() {
		path := filepath.Join(req.GetOutputDirectory(), fmt.Sprintf("%s-%s-%s", req.GetArtifactName(), target.GetOs(), target.GetArchitecture()))
		if err := os.MkdirAll(req.GetOutputDirectory(), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, payload, 0o700); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, &builderv0.PackageArtifact{
			Kind:   builderv0.PackageArtifact_EXECUTABLE,
			Path:   path,
			Target: target,
		})
	}
	return &builderv0.PackageResponse{
		State:     &builderv0.PackageStatus{State: builderv0.PackageStatus_SUCCESS},
		Artifacts: artifacts,
	}, nil
}

// TEST_SOURCE_AUDIT_VULNERABLE reports one actionable HIGH finding so release
// gating can be exercised; otherwise the source audits clean.
func (*discoveryBuilder) Audit(context.Context, *builderv0.AuditRequest) (*builderv0.AuditResponse, error) {
	if err := recordCI("Builder.Audit"); err != nil {
		return nil, err
	}
	response := &builderv0.AuditResponse{
		State: &builderv0.AuditStatus{State: builderv0.AuditStatus_CLEAN},
		Tool:  "test-source-auditor",
	}
	if os.Getenv("TEST_SOURCE_AUDIT_VULNERABLE") != "" {
		response.State = &builderv0.AuditStatus{State: builderv0.AuditStatus_FINDINGS}
		response.Findings = []*builderv0.AuditFinding{{
			Id:             "TEST-SOURCE-VULN-1",
			Severity:       builderv0.AuditFinding_HIGH,
			Package:        "example.test/vulnerable",
			CurrentVersion: "1.0.0",
			FixedVersion:   "1.0.1",
		}}
	}
	return response, nil
}

type ciRuntime struct {
	runtimev0.UnimplementedRuntimeServer
}

func (*ciRuntime) Load(context.Context, *runtimev0.LoadRequest) (*runtimev0.LoadResponse, error) {
	return &runtimev0.LoadResponse{Status: &runtimev0.LoadStatus{State: runtimev0.LoadStatus_READY}}, nil
}

func (*ciRuntime) Init(context.Context, *runtimev0.InitRequest) (*runtimev0.InitResponse, error) {
	return &runtimev0.InitResponse{Status: &runtimev0.InitStatus{State: runtimev0.InitStatus_READY}}, nil
}

func (*ciRuntime) Test(context.Context, *runtimev0.TestRequest) (*runtimev0.TestResponse, error) {
	if err := recordCI("Runtime.Test"); err != nil {
		return nil, err
	}
	return &runtimev0.TestResponse{Status: &runtimev0.TestStatus{State: runtimev0.TestStatus_SUCCESS}, Result: &runtimev0.TestRunResult{State: runtimev0.TestRunResult_PASSED}, Counts: &runtimev0.TestCounts{Total: 1, Passed: 1}}, nil
}

func (*ciRuntime) Stop(context.Context, *runtimev0.StopRequest) (*runtimev0.StopResponse, error) {
	return &runtimev0.StopResponse{Status: &runtimev0.StopStatus{State: runtimev0.StopStatus_SUCCESS}}, nil
}
