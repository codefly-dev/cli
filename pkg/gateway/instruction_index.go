package gateway

// ARCHITECTURE: repository instruction documents are inspected by Codefly's
// rooted source behavior and translated through the shared Tooling contract.
// The Gateway validates and optionally narrows only typed scope metadata; it
// never reads a document or interprets Markdown itself.

import (
	"context"
	"fmt"

	codecore "github.com/codefly-dev/core/code"
	"github.com/codefly-dev/core/failures"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	codev0 "github.com/codefly-dev/core/generated/go/codefly/services/code/v0"
	toolingv0 "github.com/codefly-dev/core/generated/go/codefly/services/tooling/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

type gatewaySourceToolingExecutor struct {
	server *Server
}

func (executor gatewaySourceToolingExecutor) Execute(ctx context.Context, request *codev0.CodeRequest) (*codev0.CodeResponse, error) {
	return executor.server.sourceExecute(ctx, request)
}

// GetInstructionIndex obtains one repository-rooted typed projection from the
// Codefly source boundary. A requested code unit is a scope filter, not a new
// parser root, so parent guidance cascades and sibling guidance stays isolated.
func (s *Server) GetInstructionIndex(ctx context.Context, req *gatewayv1.GetInstructionIndexRequest) (*gatewayv1.GetInstructionIndexResponse, error) {
	requestedService := ""
	if req != nil {
		requestedService = req.GetService()
	}
	if err := s.validateService(requestedService); err != nil {
		return nil, err
	}

	var inspected *gatewayv1.CodeUnitTarget
	var scopePath string
	if req != nil && req.GetCodeUnit() != nil {
		targets, err := s.normalizeCodeUnitTargets([]*gatewayv1.CodeUnitTarget{req.GetCodeUnit()})
		if err != nil {
			return &gatewayv1.GetInstructionIndexResponse{
				CodeUnit: cloneCodeUnitTarget(req.GetCodeUnit()),
				Failure:  failures.New(basev0.FailureCode_FAILURE_CODE_INVALID_ARGUMENT, "gateway.get-instruction-index", err.Error()),
			}, nil
		}
		target := targets[0]
		inspected = &gatewayv1.CodeUnitTarget{Id: target.id, Path: target.path}
		scopePath = target.path
	}

	tooling := codecore.NewSourceTooling(gatewaySourceToolingExecutor{server: s})
	response, err := tooling.GetInstructionIndex(ctx, &toolingv0.GetInstructionIndexRequest{})
	if err != nil {
		return &gatewayv1.GetInstructionIndexResponse{CodeUnit: inspected, Failure: gatewayInstructionIndexFailure(s.serviceRoot(), err)}, nil
	}
	if response.GetIndex() == nil {
		return &gatewayv1.GetInstructionIndexResponse{
			CodeUnit: inspected,
			Failure:  failures.Ensure(response.GetFailure(), basev0.FailureCode_FAILURE_CODE_INTERNAL, "gateway.get-instruction-index", "source tooling returned no instruction index"),
		}, nil
	}
	index := response.GetIndex()
	if inspected != nil {
		index = codecore.FilterInstructionIndex(index, scopePath)
	}
	return &gatewayv1.GetInstructionIndexResponse{
		Index: index, Failure: failures.Clone(response.GetFailure()), CodeUnit: inspected,
	}, nil
}

func gatewayInstructionIndexFailure(root string, err error) *basev0.Failure {
	message := gatewayErrorMessage(root, err)
	if failure, ok := failures.Extract(err); ok {
		cloned := failures.Clone(failure)
		cloned.Message = gatewayErrorMessage(root, fmt.Errorf("%s", cloned.GetMessage()))
		return cloned
	}
	return failures.FromError("gateway.get-instruction-index", fmt.Errorf("%s", message))
}
