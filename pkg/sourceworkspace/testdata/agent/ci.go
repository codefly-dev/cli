package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	runtimev1 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v1"
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
func (*discoveryBuilder) Package(_ context.Context, req *builderv1.PackageRequest) (*builderv1.PackageResponse, error) {
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
	var artifacts []*builderv1.PackageArtifact
	for _, target := range req.GetTargets() {
		path := filepath.Join(req.GetOutputDirectory(), fmt.Sprintf("%s-%s-%s", req.GetArtifactName(), target.GetOs(), target.GetArchitecture()))
		if err := os.MkdirAll(req.GetOutputDirectory(), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, payload, 0o700); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, &builderv1.PackageArtifact{
			Kind:   builderv1.PackageArtifact_EXECUTABLE,
			Path:   path,
			Target: target,
		})
	}
	return &builderv1.PackageResponse{
		State:     &builderv1.PackageStatus{State: builderv1.PackageStatus_SUCCESS},
		Artifacts: artifacts,
	}, nil
}

// TEST_SOURCE_AUDIT_VULNERABLE reports one actionable HIGH finding so release
// gating can be exercised; otherwise the source audits clean.
func (*discoveryBuilder) Audit(context.Context, *builderv1.AuditRequest) (*builderv1.AuditResponse, error) {
	if err := recordCI("Builder.Audit"); err != nil {
		return nil, err
	}
	response := &builderv1.AuditResponse{
		State: &builderv1.AuditStatus{State: builderv1.AuditStatus_CLEAN},
		Tool:  "test-source-auditor",
	}
	if os.Getenv("TEST_SOURCE_AUDIT_VULNERABLE") != "" {
		response.State = &builderv1.AuditStatus{State: builderv1.AuditStatus_FINDINGS}
		response.Findings = []*builderv1.AuditFinding{{
			Id:             "TEST-SOURCE-VULN-1",
			Severity:       builderv1.AuditFinding_HIGH,
			Package:        "example.test/vulnerable",
			CurrentVersion: "1.0.0",
			FixedVersion:   "1.0.1",
		}}
	}
	return response, nil
}

type ciRuntime struct {
	runtimev1.UnimplementedRuntimeServer
}

func (*ciRuntime) Load(context.Context, *runtimev1.LoadRequest) (*runtimev1.LoadResponse, error) {
	return &runtimev1.LoadResponse{Status: &runtimev1.LoadStatus{State: runtimev1.LoadStatus_READY}}, nil
}

func (*ciRuntime) Init(context.Context, *runtimev1.InitRequest) (*runtimev1.InitResponse, error) {
	return &runtimev1.InitResponse{Status: &runtimev1.InitStatus{State: runtimev1.InitStatus_READY}}, nil
}

func (*ciRuntime) Test(context.Context, *runtimev1.TestRequest) (*runtimev1.TestResponse, error) {
	if err := recordCI("Runtime.Test"); err != nil {
		return nil, err
	}
	return &runtimev1.TestResponse{Status: &runtimev1.TestStatus{State: runtimev1.TestStatus_SUCCESS}, Result: &runtimev1.TestRunResult{State: runtimev1.TestRunResult_PASSED}, Counts: &runtimev1.TestCounts{Total: 1, Passed: 1}}, nil
}

func (*ciRuntime) Stop(context.Context, *runtimev1.StopRequest) (*runtimev1.StopResponse, error) {
	return &runtimev1.StopResponse{Status: &runtimev1.StopStatus{State: runtimev1.StopStatus_SUCCESS}}, nil
}
