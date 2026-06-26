package orchestration

import (
	"context"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
)

type RuntimeStartPolicy struct {
	ExecutorManager
	dependencies *architecture.ServiceDependencies
}

// stateEmitter is implemented by the Flow so the policy can report per-service
// lifecycle transitions for the live status without depending on it concretely.
type stateEmitter interface {
	emitState(service string, state tui.ServiceState, port int)
	ServicePort(service string) int
}

// runtimePhase maps a runtime action to the lifecycle state a service enters
// while that action runs. The bool is false for non-runtime actions, which
// emit nothing.
func runtimePhase(t ActionType) (tui.ServiceState, bool) {
	switch t {
	case RuntimeLoad:
		return tui.StateLoading, true
	case RuntimeInit:
		return tui.StateInitializing, true
	case RuntimeStart:
		return tui.StateStarting, true
	default:
		return 0, false
	}
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

	// Report per-service lifecycle to the live status. Only for real (non-Failed)
	// runtime actions: a Failed action is a gated re-queue that just returns
	// Pause(), so emitting "starting" there would falsely flag a blocked
	// dependent as in flight. The executor below blocks for the whole phase, so
	// the "entering" emit is what makes a slow Init (e.g. vault's 30s health
	// wait) visible as it happens rather than only at timeout.
	emitter, _ := policy.ExecutorManager.(stateEmitter)
	phase, isRuntime := runtimePhase(action.Type)
	if emitter != nil && isRuntime && !action.Failed {
		emitter.emitState(action.Service, phase, emitter.ServicePort(action.Service))
	}

	outputProperty, err := executor(ctx)
	if err != nil {
		if emitter != nil && isRuntime && !action.Failed {
			emitter.emitState(action.Service, tui.StateFailed, 0)
		}
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
		// We are good — the service started and passed its readiness check.
		if emitter != nil {
			emitter.emitState(action.Service, tui.StateRunning, emitter.ServicePort(action.Service))
		}
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
