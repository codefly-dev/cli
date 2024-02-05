package manager

import (
	"context"
)

// OutputProperty determines the effect on the execution of the action
// OnInit: first time we got an output
// IndependentUpdate: underlying has updated but nothing is needed
// RequirePropagation: underlying has been updated and needs to be propagated
type OutputProperty struct {
	OnInit                        bool
	Wait                          bool
	IndependentUpdate             bool
	UpdateWithRequiredPropagation bool
}

func (o *OutputProperty) String() string {
	if o.OnInit {
		return "OnInit"
	}
	if o.IndependentUpdate {
		return "IndependentUpdate"
	}
	if o.UpdateWithRequiredPropagation {
		return "UpdateWithRequiredPropagation"
	}
	if o.Wait {
		return "Pause"
	}
	return "unknown"
}

func (o *OutputProperty) Valid() bool {
	// Only one must be true
	trues := 0
	if o.OnInit {
		trues++
	}
	if o.IndependentUpdate {
		trues++
	}
	if o.UpdateWithRequiredPropagation {
		trues++
	}
	if o.Wait {
		trues++
	}
	return trues == 1
}

type OutputProcessor interface {
	Process(ctx context.Context) (*OutputProperty, error)
}

type OutputProcessorFunc func(ctx context.Context) (*OutputProperty, error)

func OnInit() *OutputProperty {
	return &OutputProperty{OnInit: true}
}

func IndependentUpdate() *OutputProperty {
	return &OutputProperty{IndependentUpdate: true}
}

func RequirePropagation() *OutputProperty {
	return &OutputProperty{UpdateWithRequiredPropagation: true}
}

func Pause() *OutputProperty {
	return &OutputProperty{Wait: true}
}

type ExecutorManager interface {
	GetExecutor(ctx context.Context, action Action) (OutputProcessorFunc, error)
}
