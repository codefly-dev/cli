package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// SnapshotPolicy builds an immutable artifact and renders its deployment
// through the same builder instance.
type SnapshotPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
	standAlone   bool
	actions      []Action
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return policy.next(action, output)
}

func (policy *SnapshotPolicy) next(action Action, output *OutputProperty) ([]Action, error) {
	if output == nil || !output.Valid() || output.Wait || action.Failed {
		return nil, fmt.Errorf("snapshot action %s on %s did not complete", action.Type, action.Service)
	}
	if len(policy.actions) == 0 {
		return nil, fmt.Errorf("snapshot has no resolved actions")
	}
	if action.Type == BuilderBegin {
		return []Action{policy.actions[0]}, nil
	}
	for index, planned := range policy.actions {
		if planned.Service == action.Service && planned.Type == action.Type {
			if index+1 == len(policy.actions) {
				return nil, nil
			}
			return []Action{policy.actions[index+1]}, nil
		}
	}
	return nil, fmt.Errorf("action %s on %s is not in the snapshot plan", action.Type, action.Service)
}

func (policy *SnapshotPolicy) completed(action Action) bool {
	if len(policy.actions) == 0 {
		return false
	}
	last := policy.actions[len(policy.actions)-1]
	return action.Service == last.Service && action.Type == last.Type
}

func (policy *SnapshotPolicy) Restrict(ctx context.Context, unique string) error {
	dependencies, err := policy.dependencies.Restrict(ctx, unique)
	if err != nil {
		return wool.Get(ctx).In("SnapshotPolicy.Restrict").Wrapf(err, "cannot get dependencies")
	}
	policy.actions = nil
	// Every member of the union closure needs a builder, but only edges for
	// the current stage constrain its order. Complete builds before rendering.
	for _, phase := range []struct {
		kind  ActionType
		stage resources.Stage
	}{
		{BuilderLoad, resources.StageBuild},
		{BuilderInit, resources.StageBuild},
		{BuilderBuild, resources.StageBuild},
		{BuilderDeploy, resources.StageRun},
	} {
		graph, err := dependencies.ForStage(phase.stage)
		if err != nil {
			return err
		}
		order, err := graph.Graph().TopologicalSort()
		if err != nil {
			return err
		}
		for _, service := range order {
			if policy.standAlone && service != unique {
				continue
			}
			policy.actions = append(policy.actions, Action{Type: phase.kind, Service: service})
		}
	}
	return nil
}

var _ PlaybookPolicy = &SnapshotPolicy{}
