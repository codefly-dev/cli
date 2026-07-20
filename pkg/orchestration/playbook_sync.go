package orchestration

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

type SyncPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
	origin       string
}

func NewSyncPolicy(ctx context.Context, dependencies *architecture.ServiceDependencies, changeManager ExecutorManager) (*SyncPolicy, error) {
	return &SyncPolicy{dependencies: dependencies, ExecutorManager: changeManager}, nil
}

func (policy *SyncPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("SyncPolicy.Execute", wool.Field("action", action))
	executor, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process executor")
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

	switch action.Type {
	case BuilderBegin:
		return policy.BasicNext(ctx, outputProperty, action, BuilderLoad)
	case BuilderLoad:
		return policy.BasicNext(ctx, outputProperty, action, BuilderInit)
	case BuilderInit:
		if action.Service != policy.origin {
			return nil, nil
		}
		return []Action{action.Next(BuilderSync)}, nil
	case BuilderSync:
		// We are good
		return nil, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (policy *SyncPolicy) BasicNext(ctx context.Context, change *OutputProperty, action Action, nextType ActionType) ([]Action, error) {
	return BasicNext(ctx, policy.dependencies, change, action, nextType)
}

func (policy *SyncPolicy) Restrict(ctx context.Context, unique string) error {
	w := wool.Get(ctx).In("SyncPolicy.Restrict")
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return w.Wrapf(err, "cannot get Dependencies")
	}
	policy.dependencies = dependencies
	policy.origin = unique
	w.Trace("restricted", wool.Field("graph", policy.dependencies.Print()))
	return nil
}

var _ PlaybookPolicy = &SyncPolicy{}
