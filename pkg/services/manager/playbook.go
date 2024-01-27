package manager

import (
	"context"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/wool"
)

type PlaybookPolicy interface {
	// ExecutorManager handles action to result of action
	ExecutorManager
	// Execute and determines what to do next
	Execute(ctx context.Context, action Action) ([]Action, error)
	// Restrict the dependencies
	Restrict(ctx context.Context, service string) error
}

type Playbook struct {
	dependencies *architecture.ServiceDependencies
	policy       PlaybookPolicy

	actions  *ActionManager
	executed []Action

	signaller *Signaller

	ignorer IgnoreFunc

	// Convenient
	stopper
}

type stopper func(ctx context.Context, action Action) bool

func (playbook *Playbook) WithStopping(policy stopper) *Playbook {
	playbook.stopper = policy
	return playbook
}

func (playbook *Playbook) WithPolicy(policy PlaybookPolicy) *Playbook {
	playbook.policy = policy
	return playbook
}

func (playbook *Playbook) WithSignallerFunc(signaller CreateSignalFunc) *Playbook {
	playbook.signaller = NewSignaller()
	playbook.signaller.WithCreateSignal(signaller)
	return playbook
}

func NewPlaybook(ctx context.Context, dependencies *architecture.ServiceDependencies) (*Playbook, error) {
	return &Playbook{
		dependencies: dependencies,
		actions:      NewActionManager(),
		signaller:    NewSignaller(),
	}, nil
}

func (playbook *Playbook) Restrict(ctx context.Context, service string) error {
	w := wool.Get(ctx).In("Playbook.Restrict")
	err := playbook.policy.Restrict(ctx, service)
	if err != nil {
		return w.Wrapf(err, "cannot restrict policy")
	}
	return nil
}

func (playbook *Playbook) Start(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Playbook.Start")
	err := playbook.Restrict(ctx, action.Service)
	if err != nil {
		return w.Wrapf(err, "cannot restrict policy")
	}
	w.Debug("sending action", wool.Field("action", action.String()))
	playbook.actions.Send(ctx, action)
	return playbook.Work(ctx)
}

func (playbook *Playbook) Executed() []Action {
	return playbook.executed
}

func (playbook *Playbook) stop(ctx context.Context, action Action) bool {
	return playbook.stopper != nil && playbook.stopper(ctx, action)
}

func (playbook *Playbook) ignore(ctx context.Context, action Action) bool {
	return playbook.ignorer != nil && playbook.ignorer(ctx, action)
}

func (playbook *Playbook) previouslyExecuted(ctx context.Context, action Action) bool {
	for _, a := range playbook.executed {
		if a == action {
			return true
		}
	}
	return false
}

func (playbook *Playbook) Work(ctx context.Context) error {
	w := wool.Get(ctx).In("work")
	w.Debug("waiting for groups")
	for {
		select {
		case <-ctx.Done():
			w.Info("context cancelled")
			return nil
		case group := <-playbook.actions.Group():
			w.Focus("received group", wool.Field("group", group.String()))
			plan := group.NewActionPlan()
			for _, action := range group.actions {
				if playbook.ignore(ctx, action) {
					w.Debug("ignoring action", wool.Field("action", action))
					continue
				}
				if playbook.previouslyExecuted(ctx, action) {
					w.Debug("previously executed", wool.Field("action", action))
					continue
				}

				w.Debug("received action", wool.Field("action", action))
				next, err := playbook.policy.Execute(ctx, action)
				if err != nil {
					return w.Wrapf(err, "invalid execution for action: %v", action)
				}
				// Do not add the "Create one"
				if action.Type != RuntimeCreate {
					playbook.executed = append(playbook.executed, action)
				}
				w.Focus("action", wool.Field("action", action))

				playbook.signal(ctx, action)

				plan.Add(next...)
				if playbook.stop(ctx, action) {
					w.Info("stopping", wool.Field("action", action))
					return nil
				}
				w.Focus("done with action", wool.Field("action", action))
			}
			// Done looping on actions
			playbook.actions.Send(ctx, plan.actions...)
			w.Focus("done with action group", wool.Field("group", group.String()))
		}
	}
}

type IgnoreFunc func(ctx context.Context, action Action) bool

func (playbook *Playbook) WithIgnore(ignore IgnoreFunc) {
	playbook.ignorer = ignore
}

func (playbook *Playbook) signal(ctx context.Context, action Action) {
	if playbook.signaller != nil {
		playbook.signaller.signal(playbook, action)
	}
}

func (playbook *Playbook) Signals() chan Signal {
	return playbook.signaller.signals
}

// ActionManager mostly for testing
func (playbook *Playbook) ActionManager() *ActionManager {
	return playbook.actions
}
