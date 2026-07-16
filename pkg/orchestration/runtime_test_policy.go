package orchestration

import (
	"context"
	"fmt"

	"github.com/codefly-dev/core/architecture"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	"github.com/codefly-dev/core/wool"
)

type RuntimeTestPolicy struct {
	ExecutorManager
	dependencies   *architecture.ServiceDependencies
	origin         string
	dependencyMode agentv0.TestDependencyMode
}

func NewRuntimeTestPolicy(_ context.Context, dependencies *architecture.ServiceDependencies, changeManager ExecutorManager, origin string, dependencyMode agentv0.TestDependencyMode) (*RuntimeTestPolicy, error) {
	switch dependencyMode {
	case agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE,
		agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES,
		agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_STACK:
	default:
		return nil, fmt.Errorf("unsupported test dependency mode %s", dependencyMode.String())
	}
	return &RuntimeTestPolicy{
		dependencies:    dependencies,
		ExecutorManager: changeManager,
		origin:          origin,
		dependencyMode:  dependencyMode,
	}, nil
}

func (policy *RuntimeTestPolicy) Execute(ctx context.Context, action Action) ([]Action, error) {
	w := wool.Get(ctx).In("RuntimeTestPolicy.Execute", wool.Field("action", action))
	var executor OutputProcessorFunc
	var err error
	if action.Type == RuntimeStart && action.Service == policy.origin && policy.dependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_START_DEPENDENCIES {
		// This is a sequencing barrier: dependency RuntimeStart actions in the
		// same group have completed, but the target process must remain stopped.
		executor = func(context.Context) (*OutputProperty, error) { return OnInit(), nil }
	} else {
		executor, err = policy.GetExecutor(ctx, action)
	}
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
		if policy.dependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
			return []Action{action.Next(RuntimeLoad)}, nil
		}
		return policy.BasicNext(ctx, outputProperty, action, RuntimeLoad)
	case RuntimeLoad:
		if policy.dependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
			return []Action{action.Next(RuntimeInit)}, nil
		}
		return policy.BasicNext(ctx, outputProperty, action, RuntimeInit)
	case RuntimeInit:
		if policy.dependencyMode == agentv0.TestDependencyMode_TEST_DEPENDENCY_MODE_NONE {
			return []Action{action.Next(RuntimeTest)}, nil
		}
		return policy.BasicNext(ctx, outputProperty, action, RuntimeStart)
	case RuntimeStart:
		if action.Service == policy.origin {
			return []Action{action.Next(RuntimeTest)}, nil
		}
		return nil, nil
	case RuntimeTest:
		// We are good
		return nil, nil
	default:
		return nil, w.NewError("unknown action type %s", action.Type)
	}
}

func (policy *RuntimeTestPolicy) BasicNext(ctx context.Context, change *OutputProperty, action Action, nextType ActionType) ([]Action, error) {
	return BasicNext(ctx, policy.dependencies, change, action, nextType)
}

func (policy *RuntimeTestPolicy) Restrict(ctx context.Context, unique string) error {
	w := wool.Get(ctx).In("RuntimeTestPolicy.Restrict")
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

var _ PlaybookPolicy = &RuntimeTestPolicy{}
