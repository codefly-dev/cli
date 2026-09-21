package main

import (
	"context"
	"fmt"
	"os"

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
// and packaging reached the same privately selected executable.
func (*discoveryBuilder) Package(context.Context, *builderv0.PackageRequest) (*builderv0.PackageResponse, error) {
	if err := recordCI("Builder.Package"); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.FailedPrecondition, "CI package boundary reached")
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
