package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

// RuntimeValidationPolicy loads and initializes the origin's runtime closure,
// then dispatches exactly one agent-owned validation RPC for the origin. Its
// dependencies are prerequisites, not implicit validation targets.
type RuntimeValidationPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
	origin       string
	terminal     ActionType
}

func NewRuntimeValidationPolicy(_ context.Context, dependencies *architecture.ServiceDependencies, manager ExecutorManager, origin string, terminal ActionType) (*RuntimeValidationPolicy, error) {
	if terminal != RuntimeLint && terminal != RuntimeBuild {
		return nil, fmt.Errorf("unsupported runtime validation action %s", terminal)
	}
	return &RuntimeValidationPolicy{
		ExecutorManager: manager,
		dependencies:    dependencies,
		origin:          origin,
		terminal:        terminal,
	}, nil
}

func (policy *RuntimeValidationPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("RuntimeValidationPolicy.Execute", wool.Field("action", action))
	executor, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get validation executor")
	}
	output, err := executor(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot execute validation action")
	}
	if output == nil {
		return nil, nil
	}
	if !output.Valid() {
		return nil, w.NewError("invalid outputProperty: %v", output)
	}
	if output.Wait {
		return []Action{action.Failing()}, nil
	}

	switch action.Type {
	case RuntimeBegin:
		return BasicNext(ctx, policy.dependencies, output, action, RuntimeLoad)
	case RuntimeLoad:
		return BasicNext(ctx, policy.dependencies, output, action, RuntimeInit)
	case RuntimeInit:
		if action.Service != policy.origin {
			return nil, nil
		}
		return []Action{action.Next(policy.terminal)}, nil
	case policy.terminal:
		return nil, nil
	default:
		return nil, w.NewError("unknown validation action %s", action.Type)
	}
}

func (policy *RuntimeValidationPolicy) Restrict(ctx context.Context, unique string) error {
	if policy.dependencies == nil {
		return nil
	}
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return wool.Get(ctx).In("RuntimeValidationPolicy.Restrict").Wrapf(err, "cannot restrict dependencies")
	}
	policy.dependencies = dependencies
	return nil
}

var _ PlaybookPolicy = &RuntimeValidationPolicy{}
