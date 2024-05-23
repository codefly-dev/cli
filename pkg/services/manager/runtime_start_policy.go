package manager

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

type RuntimeStartPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
}

func NewRuntimeStartPolicy(ctx context.Context, dependencies *architecture.ServiceDependencies, changeManager ExecutorManager) (*RuntimeStartPolicy, error) {
	return &RuntimeStartPolicy{dependencies: dependencies, ExecutorManager: changeManager}, nil
}

func (policy *RuntimeStartPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("RuntimeStartPolicy.Execute", wool.Field("action", action))
	executor, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process executor")
	}
	if executor == nil {
		return nil, nil
	}
	outputProperty, err := executor(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty")
	}
	if outputProperty == nil {
		w.Trace("no outputProperty: not doing anything")
		return nil, nil
	}
	w.Trace("got outputProperty", wool.Field("outputProperty", outputProperty))
	if !outputProperty.Valid() {
		return nil, w.NewError("invalid outputProperty: only one of the property of output must be true: %v", outputProperty)
	}
	if outputProperty.Wait {
		return []Action{action.Failing()}, nil
	}

	switch action.Type {
	case RuntimeBegin:
		return policy.BasicNext(ctx, outputProperty, action, RuntimeLoad)
	case RuntimeLoad:
		return policy.BasicNext(ctx, outputProperty, action, RuntimeInit)
	case RuntimeInit:
		return policy.BasicNext(ctx, outputProperty, action, RuntimeStart)
	case RuntimeStart:
		// We are good
		return nil, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (policy *RuntimeStartPolicy) BasicNext(ctx context.Context, change *OutputProperty, action Action, nextType ActionType) ([]Action, error) {
	return BasicNext(ctx, policy.dependencies, change, action, nextType)
}

func (policy *RuntimeStartPolicy) Restrict(ctx context.Context, unique string) error {
	w := wool.Get(ctx).In("RuntimeStartPolicy.Restrict")
	if policy.dependencies == nil {
		return nil
	}
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return w.Wrapf(err, "cannot get Dependencies")
	}
	policy.dependencies = dependencies
	w.Trace("restricted", wool.Field("graph", policy.dependencies.Print()))
	return nil
}

var _ PlaybookPolicy = &RuntimeStartPolicy{}
