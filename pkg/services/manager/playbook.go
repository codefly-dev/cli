package manager

import (
	"context"
	"sync"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/wool"
)

type PlaybookPolicy interface {
	// ExecutorManager handles action to result of action
	ExecutorManager
	// Execute and determines what to do next
	Execute(ctx context.Context, action Action) ([]Action, error)
	// Restrict the Dependencies
	Restrict(ctx context.Context, service string) error
}

type Playbook struct {
	dependencies *architecture.ServiceDependencies
	policy       PlaybookPolicy

	world *World

	actions *ActionManager

	executed []Action

	signaller *Signaller

	ignorer IgnoreFunc

	// Convenient
	stopper

	// Consistency
	lock sync.RWMutex
	// mostly for testing
	stopLock sync.RWMutex
}

type stopper func(ctx context.Context, action Action) bool

func (playbook *Playbook) WithStopping(policy stopper) *Playbook {
	playbook.stopLock.Lock()
	defer playbook.stopLock.Unlock()
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

func NewPlaybook(ctx context.Context, world *World) (*Playbook, error) {
	return &Playbook{
		world:     world,
		actions:   NewActionManager(),
		signaller: NewSignaller(),
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

func (playbook *Playbook) Seed(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Playbook.Begin")
	w.Debug("sending action", wool.Field("action", action.String()))
	playbook.actions.bumpRound()
	playbook.actions.send(ctx, action)
	return nil
}

func (playbook *Playbook) Begin(ctx context.Context, action Action) error {
	w := wool.Get(ctx).In("Playbook.Begin")
	err := playbook.Restrict(ctx, action.Service)
	if err != nil {
		return w.Wrapf(err, "cannot restrict policy")
	}
	err = playbook.Seed(ctx, action)
	if err != nil {
		return w.Wrapf(err, "cannot seed")
	}
	return playbook.Work(ctx)
}

// Executed returns the list of actions that were executed
// Since it is public, we lock
func (playbook *Playbook) Executed() []Action {
	playbook.lock.RLock()
	var out []Action
	for _, action := range playbook.executed {
		out = append(out, action)
	}
	playbook.lock.RUnlock()
	return out
}

func (playbook *Playbook) stop(ctx context.Context, action Action) bool {
	playbook.stopLock.RLock()
	defer playbook.stopLock.RUnlock()
	if playbook.stopper == nil {
		return false
	}
	stop := playbook.stopper(ctx, action)
	return stop

}

func (playbook *Playbook) ignore(ctx context.Context, action Action) bool {
	return playbook.ignorer != nil && playbook.ignorer(ctx, action)
}

func (playbook *Playbook) previouslyExecuted(ctx context.Context, action Action) bool {
	executed := playbook.Executed()
	for _, a := range executed {
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
			w.Debug("received group", wool.Field("group", group.String()))
			plan := playbook.actions.NewActionPlan()
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

				w.Debug("action", wool.Field("action", action))

				plan.Add(ctx, next...)

				playbook.record(action)

				playbook.signal(ctx, action)

				if playbook.stop(ctx, action) {
					w.Info("stopping", wool.Field("action", action))
					return nil
				}
				w.Debug("done with action", wool.Field("action", action))
			}
			// Done looping on actions
			playbook.actions.send(ctx, plan.actions...)
			w.Debug("done with action group", wool.Field("group", group.String()))
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

func (playbook *Playbook) record(action Action) {
	playbook.lock.Lock()
	defer playbook.lock.Unlock()
	if action.Type != RuntimeBegin {
		playbook.executed = append(playbook.executed, action)
	}
}
