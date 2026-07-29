package orchestration

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

// SnapshotPolicy builds an immutable artifact and renders its deployment
// through the same builder instance.
type SnapshotPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
}

func NewSnapshotPolicy(_ context.Context, dependencies *architecture.ServiceDependencies, manager ExecutorManager) (*SnapshotPolicy, error) {
	return &SnapshotPolicy{ExecutorManager: manager, dependencies: dependencies}, nil
}

func (policy *SnapshotPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("SnapshotPolicy.Execute", wool.Field("action", action))
	executor, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process executor")
	}
	output, err := executor(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot process outputProperty")
	}
	if output == nil {
		return nil, nil
	}
	if !output.Valid() {
		return nil, w.NewError("invalid outputProperty: only one property may be true: %v", output)
	}

	switch action.Type {
	case BuilderBegin:
		return BasicNext(ctx, policy.dependencies, output, action, BuilderLoad)
	case BuilderLoad:
		return BasicNext(ctx, policy.dependencies, output, action, BuilderInit)
	case BuilderInit:
		return BasicNext(ctx, policy.dependencies, output, action, BuilderBuild)
	case BuilderBuild:
		return BasicNext(ctx, policy.dependencies, output, action, BuilderDeploy)
	case BuilderDeploy:
		return nil, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (policy *SnapshotPolicy) Restrict(ctx context.Context, unique string) error {
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return wool.Get(ctx).In("SnapshotPolicy.Restrict").Wrapf(err, "cannot get dependencies")
	}
	policy.dependencies = dependencies
	return nil
}

var _ PlaybookPolicy = &SnapshotPolicy{}
