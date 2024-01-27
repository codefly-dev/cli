package manager

import (
	"context"

	"github.com/codefly-dev/cli/pkg/architecture"
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
	changer, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process changer")
	}
	outputProperty, err := changer(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty")
	}
	w.Focus("got outputProperty", wool.Field("outputProperty", outputProperty))
	if !outputProperty.Valid() {
		return nil, w.NewError("invalid outputProperty: only one of the property of output must be true: %v", outputProperty)
	}

	switch action.Type {
	case RuntimeCreate:
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
	w := wool.Get(ctx).In("RuntimeStartPolicy.BasicNext")
	var next []Action
	if change.OnInit {
		required, err := policy.dependencies.OrderTo(ctx, action.Service)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get required")
		}
		next = append(next, action.NextFor(nextType, required...)...)
		next = append(next, action.Next(nextType))
	}
	if change.IndependentUpdate {
		next = append(next, action.Next(nextType))

	}
	if change.UpdateWithRequiredPropagation {
		next = append(next, action.Next(nextType))
		deps, err := policy.dependencies.DirectDependents(ctx, action.Service)
		if err != nil {
			return nil, w.Wrapf(err, "cannot get required")
		}
		next = append(next, action.NextFor(nextType, deps...)...)
	}
	return next, nil
}

func (policy *RuntimeStartPolicy) Restrict(ctx context.Context, unique string) error {
	w := wool.Get(ctx).In("RuntimeStartPolicy.Restrict")
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return w.Wrapf(err, "cannot get dependencies")
	}
	policy.dependencies = dependencies
	w.Debug("restricted", wool.Field("graph", policy.dependencies.Print()))
	return nil
}

var _ PlaybookPolicy = &RuntimeStartPolicy{}
