package orchestration

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

type BuildPolicy struct {
	ExecutorManager
	world        *World
	dependencies *architecture.ServiceDependencies
}

func NewBuildPolicy(ctx context.Context, hub *Hub, world *World) (*BuildPolicy, error) {
	executorManager, err := NewBuildExecutor(ctx, hub)
	if err != nil {
		return nil, wool.Get(ctx).Wrapf(err, "cannot create BuildExecutor")
	}
	return &BuildPolicy{world: world, dependencies: world.Dependencies, ExecutorManager: executorManager}, nil
}

func (policy *BuildPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("BuildPolicy.Execute", wool.Field("action", action))
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
		return policy.BasicNext(ctx, outputProperty, action, BuilderBuild)
	case BuilderBuild:
		// We are good
		return nil, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (policy *BuildPolicy) BasicNext(ctx context.Context, change *OutputProperty, action Action, nextType ActionType) ([]Action, error) {
	return BasicNext(ctx, policy.world.Dependencies, change, action, nextType)
}

func (policy *BuildPolicy) Restrict(ctx context.Context, unique string) error {
	w := wool.Get(ctx).In("BuildPolicy.Restrict")
	dependencies, err := policy.world.Dependencies.Restrict(ctx, unique)
	if err != nil {
		return w.Wrapf(err, "cannot get Dependencies")
	}
	policy.dependencies = dependencies
	w.Trace("restricted", wool.Field("graph", policy.dependencies.Print()))
	return nil
}

var _ PlaybookPolicy = &BuildPolicy{}
