package manager

import (
	"context"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/wool"
)

type DeployPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
}

func NewDeployPolicy(ctx context.Context, dependencies *architecture.ServiceDependencies, changeManager ExecutorManager) (*DeployPolicy, error) {
	return &DeployPolicy{dependencies: dependencies, ExecutorManager: changeManager}, nil
}

func (policy *DeployPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("DeployPolicy.Execute", wool.Field("action", action))
	executor, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process executor")
	}
	outputProperty, err := executor(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty")
	}
	if outputProperty == nil {
		w.Debug("no outputProperty: not doing anything")
		return nil, nil
	}
	w.Debug("got outputProperty", wool.Field("outputProperty", outputProperty))
	if !outputProperty.Valid() {
		return nil, w.NewError("invalid outputProperty: only one of the property of output must be true: %v", outputProperty)
	}

	switch action.Type {
	case BuilderBegin:
		return policy.BasicNext(ctx, outputProperty, action, BuilderLoad)
	case BuilderLoad:
		return policy.BasicNext(ctx, outputProperty, action, BuilderInit)
	case BuilderInit:
		return policy.BasicNext(ctx, outputProperty, action, BuilderDeploy)
	case BuilderDeploy:
		// We are good
		return nil, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (policy *DeployPolicy) BasicNext(ctx context.Context, change *OutputProperty, action Action, nextType ActionType) ([]Action, error) {
	return BasicNext(ctx, policy.dependencies, change, action, nextType)
}

func (policy *DeployPolicy) Restrict(ctx context.Context, unique string) error {
	w := wool.Get(ctx).In("DeployPolicy.Restrict")
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return w.Wrapf(err, "cannot get Dependencies")
	}
	policy.dependencies = dependencies
	w.Debug("restricted", wool.Field("graph", policy.dependencies.Print()))
	return nil
}

var _ PlaybookPolicy = &DeployPolicy{}
