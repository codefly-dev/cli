package orchestration

import (
	"context"
	"fmt"
	"slices"

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
		return policy.emit(0), nil
	}
	for index, planned := range policy.actions {
		if planned.Service == action.Service && planned.Type == action.Type {
			return policy.emit(policy.successor(index)), nil
		}
	}
	return nil, fmt.Errorf("action %s on %s is not in the snapshot plan", action.Type, action.Service)
}

// phaseEnd is one past the last action of the phase holding index. Restrict
// appends the plan phase by phase, so a phase is a contiguous run of one
// action type.
func (policy *SnapshotPolicy) phaseEnd(index int) int {
	end := index
	for end < len(policy.actions) && policy.actions[end].Type == policy.actions[index].Type {
		end++
	}
	return end
}

// successor is the index of the action that follows the one at index.
//
// For a phase whose services run one after another that is simply the next
// action. For a concurrent phase it is the first action after the whole phase:
// every service of such a phase is emitted together and runs together, so each
// of them is followed by the phase that comes after, not by a sibling that is
// already running. Having all of them name the same successor is what lets the
// following phase start exactly once however the siblings finished.
func (policy *SnapshotPolicy) successor(index int) int {
	if concurrentPhase(policy.actions[index].Type) {
		return policy.phaseEnd(index)
	}
	return index + 1
}

// emit is the action group that starts at index: the whole phase when its
// services run concurrently, one action otherwise, and nothing once the plan is
// exhausted.
func (policy *SnapshotPolicy) emit(index int) []Action {
	if index >= len(policy.actions) {
		return nil
	}
	if !concurrentPhase(policy.actions[index].Type) {
		return []Action{policy.actions[index]}
	}
	return slices.Clone(policy.actions[index:policy.phaseEnd(index)])
}

// ConcurrentActions reports that this group may run its actions at the same
// time, which it does for a build phase and nothing else.
//
// What makes the build phase the one safe phase is not that building is slow:
// it is that each service's build reads and writes only its own things. Its
// recipe and the archive of it live under its own build root
// (buildRecipeRoot); the agent that emitted that recipe is its own process,
// because core keys a service instance by the service and not by its agent; the
// context it stages and the metadata file buildx writes are freshly named
// temporary paths; the registry layer cache it reads is scoped to its own
// service and recipe (scopedBuildCache); and the reuse entry it records is one
// file named by the digest of its own inputs, written by rename. The Go module
// proxy the builds share is finished and read-only by the time any of them
// runs, because the plan phase fetched every graph before this phase starts.
//
// The phases around it are not safe and stay one at a time. Load and Init
// exchange endpoints through the shared state the next service's Init reads.
// Plan fetches Go module graphs into one shared module cache, pruning it
// between fetches. Deploy renders each service's manifests into the delivery
// tree, which is the signed artifact and must not depend on who got there
// first.
func (policy *SnapshotPolicy) ConcurrentActions(actions []Action) bool {
	if len(actions) < 2 {
		return false
	}
	for _, action := range actions {
		if !concurrentPhase(action.Type) || action.Failed {
			return false
		}
	}
	return true
}

// concurrentPhase reports whether a phase of this type runs its services at the
// same time.
func concurrentPhase(actionType ActionType) bool {
	return actionType == BuilderBuild
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
	// Every recipe is planned — and the Go modules it declares fetched — before
	// any image is built, so no build needs a credential that is only valid at
	// the start of the render.
	for _, phase := range []struct {
		kind  ActionType
		stage resources.Stage
	}{
		{BuilderLoad, resources.StageBuild},
		{BuilderInit, resources.StageBuild},
		{BuilderPlan, resources.StageBuild},
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
