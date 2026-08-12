package dockerexec

import (
	"context"
	"math"
	"strings"

	codeflygateway "github.com/codefly-dev/cli/pkg/gateway"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (g *Gateway) RunCommand(ctx context.Context, req *gatewayv1.RunCommandRequest) (*gatewayv1.RunCommandResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "run command request is required")
	}
	if err := codeflygateway.ValidateUnstructuredUse(req.GetUnstructuredUse()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := g.base.validateService(req.GetService()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.GetCommand()) == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}
	if req.GetTimeoutSeconds() > 600 {
		return nil, status.Error(codes.InvalidArgument, "timeout_seconds must be at most 600")
	}
	workingDir := req.GetWorkingDir()
	if workingDir == "" {
		workingDir = g.base.workDir
	}
	argv := append([]string{req.GetCommand()}, req.GetArgs()...)
	stdout, stderr, exitCode, err := g.base.runStdin(ctx, workingDir, int(req.GetTimeoutSeconds()), req.GetStdin(), argv...)
	if err != nil {
		return &gatewayv1.RunCommandResponse{ExitCode: -1, Stderr: err.Error()}, nil
	}
	return &gatewayv1.RunCommandResponse{Stdout: stdout, Stderr: stderr, ExitCode: gatewayExitCode(exitCode)}, nil
}

func gatewayExitCode(exitCode int) int32 {
	if exitCode < math.MinInt32 || exitCode > math.MaxInt32 {
		return -1
	}
	return int32(exitCode) //nolint:gosec // the explicit bounds above prove the transport conversion.
}
