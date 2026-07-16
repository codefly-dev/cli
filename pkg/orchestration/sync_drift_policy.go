package orchestration

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

// SyncDriftPolicy loads and initializes the origin's builder dependency
// closure, then dispatches a single non-mutating sync request to the origin.
// Dependencies contribute endpoints and shared state but are never themselves
// synchronized by a drift check.
type SyncDriftPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
	origin       string
}

func NewSyncDriftPolicy(_ context.Context, dependencies *architecture.ServiceDependencies, manager ExecutorManager, origin string) (*SyncDriftPolicy, error) {
	return &SyncDriftPolicy{
		ExecutorManager: manager,
		dependencies:    dependencies,
		origin:          origin,
	}, nil
}

func (policy *SyncDriftPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("SyncDriftPolicy.Execute", wool.Field("action", action))
	executor, err := policy.GetExecutor(ctx, action)
	if err != nil {
		return nil, w.Wrapf(err, "cannot get sync-drift executor")
	}
	output, err := executor(ctx)
	if err != nil {
		return nil, w.Wrapf(err, "cannot execute sync-drift action")
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
	case BuilderBegin:
		return BasicNext(ctx, policy.dependencies, output, action, BuilderLoad)
	case BuilderLoad:
		return BasicNext(ctx, policy.dependencies, output, action, BuilderInit)
	case BuilderInit:
		if action.Service != policy.origin {
			return nil, nil
		}
		return []Action{action.Next(BuilderSync)}, nil
	case BuilderSync:
		return nil, nil
	default:
		return nil, w.NewError("unknown sync-drift action %s", action.Type)
	}
}

func (policy *SyncDriftPolicy) Restrict(ctx context.Context, unique string) error {
	if policy.dependencies == nil {
		return nil
	}
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return wool.Get(ctx).In("SyncDriftPolicy.Restrict").Wrapf(err, "cannot restrict dependencies")
	}
	policy.dependencies = dependencies
	return nil
}

var _ PlaybookPolicy = &SyncDriftPolicy{}
