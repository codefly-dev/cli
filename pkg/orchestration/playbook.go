package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

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
	policy PlaybookPolicy

	world *World

	actions *ActionManager

	executed []Action

	signaller *Signaller

	pause *PauseManager

	ignorer IgnoreFunc

	// Convenient
	_stopAfter StopAfterFunc

	// Consistency
	lock sync.RWMutex
	// mostly for testing
	stopLock sync.RWMutex

	// failedService records WHICH service's action errored the flow (and the
	// phase), so the top-level command reports the real culprit instead of always
	// blaming the origin. Set once, under lock, on the first action error.
	failedService string
	failedPhase   ActionType
}

// recordFailure remembers the first service whose action errored.
func (playbook *Playbook) recordFailure(action Action) {
	playbook.lock.Lock()
	defer playbook.lock.Unlock()
	if playbook.failedService == "" {
		playbook.failedService = action.Service
		playbook.failedPhase = action.Type
	}
}

// FailedService returns the service whose action failed the flow, the phase it
// failed in, and ok=false if the flow did not fail on a service action.
func (playbook *Playbook) FailedService() (string, ActionType, bool) {
	playbook.lock.RLock()
	defer playbook.lock.RUnlock()
	return playbook.failedService, playbook.failedPhase, playbook.failedService != ""
}

type StopAfterFunc func(ctx context.Context, action Action) bool

// stopAfterRoots stops the playbook once every root has completed actionType.
// With one root that is the action itself; with several, stopping at the first
// would return before the other roots had been carried that far. Called only
// from the Work loop, which is the single goroutine driving a playbook.
func stopAfterRoots(roots []string, actionType ActionType) StopAfterFunc {
	remaining := make(map[string]bool, len(roots))
	for _, root := range roots {
		remaining[root] = true
	}
	return func(_ context.Context, action Action) bool {
		if action.Type != actionType || !remaining[action.Service] {
			return false
		}
		delete(remaining, action.Service)
		return len(remaining) == 0
	}
}

func (playbook *Playbook) WithStoppingAfter(policy StopAfterFunc) *Playbook {
	playbook.stopLock.Lock()
	defer playbook.stopLock.Unlock()
	playbook._stopAfter = policy
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

func NewPlaybook(_ context.Context, world *World) (*Playbook, error) {
	return &Playbook{
		world:     world,
		pause:     NewPauseManager(world),
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
	return playbook.seedGroup(ctx, action)
}

// seedGroup sends the opening actions as one group, so a run rooted in several
// services fans out from all of them in the same round.
func (playbook *Playbook) seedGroup(ctx context.Context, actions ...Action) error {
	w := wool.Get(ctx).In("Playbook.Seed")
	for _, action := range actions {
		w.Debug("sending action", wool.Field("action", action.ShortString()))
	}
	playbook.actions.bumpRound()
	w.Debug("round", wool.Field("round", playbook.actions.round))
	playbook.actions.send(ctx, actions...)
	return nil
}

// Begin restricts the policy to the run's root and seeds it.
//
// Restriction narrows the dependency graph to one root's closure, so it applies
// only when there is a single root: with several, the flow has already scoped
// the graph to the run's own service set, and narrowing it to any one root
// would make the others unresolvable.
func (playbook *Playbook) Begin(ctx context.Context, actions ...Action) error {
	w := wool.Get(ctx).In("Playbook.Begin")
	// Widening this to a variadic made "no actions" expressible where it used to
	// be a compile error, and an empty seed is indistinguishable at runtime from
	// a run whose services never finish: send() drops an empty group, so Work
	// would block on an action queue nothing will ever fill, until the whole run
	// is cancelled. Refuse it here rather than hang.
	if len(actions) == 0 {
		return w.NewError("cannot begin a playbook with no actions")
	}
	if len(actions) == 1 {
		if err := playbook.Restrict(ctx, actions[0].Service); err != nil {
			return w.Wrapf(err, "cannot restrict policy")
		}
	}
	if err := playbook.seedGroup(ctx, actions...); err != nil {
		return w.Wrapf(err, "cannot seed")
	}
	return playbook.Work(ctx)
}

// ScopedPolicy is implemented by a policy that plans from a dependency graph,
// and can plan from the whole graph it was handed instead of the closure of one
// service.
type ScopedPolicy interface {
	// RestrictToScope plans every service of the graph the policy holds.
	RestrictToScope(ctx context.Context) error
}

// BeginScoped seeds a run rooted in several services. Restriction narrows the
// graph to one root's closure, which would leave the other roots unresolvable,
// so the policy plans from the graph the flow has already scoped to the run's
// own service set (Begin's note on several roots).
func (playbook *Playbook) BeginScoped(ctx context.Context, actions ...Action) error {
	w := wool.Get(ctx).In("Playbook.BeginScoped")
	if len(actions) == 0 {
		return w.NewError("cannot begin a playbook with no actions")
	}
	if scoped, ok := playbook.policy.(ScopedPolicy); ok {
		if err := scoped.RestrictToScope(ctx); err != nil {
			return w.Wrapf(err, "cannot plan the scoped run")
		}
	}
	if err := playbook.seedGroup(ctx, actions...); err != nil {
		return w.Wrapf(err, "cannot seed")
	}
	return playbook.Work(ctx)
}

// Executed returns the list of actions that were executed
// Since it is public, we lock
func (playbook *Playbook) Executed() []Action {
	if playbook == nil {
		return nil
	}
	playbook.lock.RLock()
	out := make([]Action, 0, len(playbook.executed))
	out = append(out, playbook.executed...)
	playbook.lock.RUnlock()
	return out
}

func (playbook *Playbook) stopAfter(ctx context.Context, action Action) bool {
	playbook.stopLock.RLock()
	defer playbook.stopLock.RUnlock()
	if playbook._stopAfter == nil {
		return false
	}
	stop := playbook._stopAfter(ctx, action)
	return stop

}

func (playbook *Playbook) ignore(ctx context.Context, action Action) bool {
	return playbook.ignorer != nil && playbook.ignorer(ctx, action)
}

func (playbook *Playbook) previouslyExecuted(_ context.Context, action Action) bool {
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
	w.Trace("waiting for groups")
	for {
		select {
		case <-ctx.Done():
			// Bookkeeping for this loop's own exit, carrying no fact the
			// caller that cancelled does not already have. It must not be
			// INFO: with no wool provider on the context these land on the
			// fallback console, which writes to the process's stdout — and an
			// embedder may be serving JSON-RPC there.
			w.Trace("context cancelled")
			// send() delivers action groups from detached goroutines that block
			// on the actions channel until they either succeed or hit their own
			// 30s timeout. As the last reader leaving, drain them so those
			// in-flight deliveries complete and exit immediately instead of
			// blocking up to 30s (delaying shutdown, piling up goroutines).
			// Deterministic: gated on the senders' WaitGroup, not a fixed wait.
			playbook.actions.drainPending()
			return nil
		case group := <-playbook.actions.Group():
			plan := playbook.actions.NewActionPlan()
			concurrent, err := playbook.runsConcurrently(group.actions)
			if err != nil {
				return err
			}
			if concurrent {
				stop, err := playbook.workConcurrently(ctx, group.actions, plan)
				switch {
				case err != nil:
					return err
				case stop:
					// Matching the serial path: a playbook that has reached its
					// stopping point queues nothing further.
					return nil
				}
				playbook.actions.send(ctx, plan.actions...)
				continue
			}
			// A build phase run one service at a time is still a phase to
			// the operator watching: its images are counted the same way.
			endPhase := playbook.world.buildProgress().beginPhase(buildPhaseSize(group.actions))
			for _, action := range group.actions {
				if playbook.pause.Handle(ctx, action) {
					w.Trace("discarding action", wool.Field("action", action))
					continue
				}

				if playbook.ignore(ctx, action) {
					w.Trace("ignoring action", wool.Field("action", action))
					continue
				}
				if playbook.previouslyExecuted(ctx, action) {
					w.Trace("previously executed", wool.Field("action", action))
					continue
				}
				w.Trace("executing action", wool.Field("action", action))

				playbook.record(action)

				// This execute the action and obtain the next actions to run
				// If some failure: next becomes []Action of type Failing
				next, err := playbook.policy.Execute(ctx, action)
				if err != nil {
					// Remember WHICH service failed so the top-level command can
					// attribute the failure correctly (not to the origin).
					playbook.recordFailure(action)
					// Keep the wrap short so the actual error stays close
					// to the start of the rendered message. The original
					// `%v action` printed the full Action struct, pushing
					// the underlying cause past terminal width and making
					// the message look truncated.
					return w.Wrapf(err, "action %s on %s failed", action.Type, action.Service)
				}

				if p, ok := playbook.pause.IsPause(ctx, next); ok {
					w.Warn(fmt.Sprintf("service %s failing, waiting for successful re-load", action.Service))
					// Record/Signal pause
					playbook.record(*p)
					playbook.signal(ctx, *p)
					continue
				}

				w.Trace("after exec", wool.Field("next", next))

				plan.Add(ctx, next...)

				playbook.signal(ctx, action)

				if playbook.stopAfter(ctx, action) {
					w.Trace("stopping", wool.Field("action", action))
					endPhase()
					return nil
				}
				w.Trace("done with action", wool.Field("action", action))

			}
			// Done looping on actions
			endPhase()
			playbook.actions.send(ctx, plan.actions...)
		}
	}
}

// ConcurrentPolicy is implemented by a policy that can tell the playbook an
// action group's members are independent of one another and may run at the same
// time. A policy that does not implement it has every group run one action
// after another, which is what every mode but a snapshot's build phase does.
type ConcurrentPolicy interface {
	ConcurrentActions(actions []Action) bool
}

// runsConcurrently reports whether this group is one the policy declares
// independent and the host is allowed to run in parallel.
//
// A bound that cannot be read is an error and not a reason to fall back to one
// action at a time: an operator who mistyped the override would otherwise get a
// render that silently takes the time it was meant to stop taking, with nothing
// said about why.
func (playbook *Playbook) runsConcurrently(actions []Action) (bool, error) {
	if len(actions) < 2 {
		return false, nil
	}
	policy, ok := playbook.policy.(ConcurrentPolicy)
	if !ok || !policy.ConcurrentActions(actions) {
		return false, nil
	}
	bound, err := buildConcurrency()
	if err != nil {
		return false, err
	}
	return bound > 1, nil
}

// workConcurrently executes one group's actions at the same time, under a
// bound, and then does the playbook's own bookkeeping for them serially in the
// group's order. It reports whether the playbook has reached its stopping point.
//
// Only the actions run concurrently. Everything the rest of the flow observes —
// which actions are recorded as executed and in what order, which failure is
// attributed to the flow, the order signals are raised in, and the order the
// next actions are queued in — is replayed afterwards in the order the group
// was given, never the order the actions happened to finish in. A render feeds
// a signed publish, so the artifact must not be able to tell whether its images
// were built one at a time or all at once.
func (playbook *Playbook) workConcurrently(ctx context.Context, actions []Action, plan *ActionPlan) (bool, error) {
	w := wool.Get(ctx).In("work.concurrent")
	bound, err := buildConcurrency()
	if err != nil {
		return false, err
	}
	// A plan is mostly waiting on registry reads (the reuse decision), not
	// compiling, so it is bounded by the network and not by the cores.
	planning := len(actions) > 0 && actions[0].Type == BuilderPlan
	if planning {
		if bound, err = planConcurrency(); err != nil {
			return false, err
		}
	}

	admitted := playbook.admit(ctx, actions)
	if len(admitted) == 0 {
		return false, nil
	}
	if len(admitted) == 1 {
		bound = 1
	}
	w.Info("building services at the same time", wool.Field("services", len(admitted)),
		wool.Field("at_once", bound), wool.Field("override", buildConcurrencyEnv))

	// A build is the one action whose own progress output is worth suppressing
	// while others are running; telling the world how many are in flight is how
	// each build knows (buildsConcurrently).
	if !planning {
		restore := playbook.world.withConcurrentBuilds(len(admitted))
		defer restore()
		// What the operator sees of the phase: one line per build starting and
		// finishing, and once the phase is over, how many images it kept.
		defer playbook.world.buildProgress().beginPhase(len(admitted))()
	}

	next := make([][]Action, len(admitted))
	failures := make([]error, len(admitted))
	var group errgroup.Group
	group.SetLimit(bound)
	// Launched inline, unlike the readiness probes, which must launch from
	// their own goroutine because they read results while later probes are
	// still blocked on SetLimit. Nothing here is read until every action is
	// done, so blocking on the bound is exactly the wanted behaviour.
	//
	// Started longest-recorded-build first rather than in the group's order,
	// because a bounded phase ends when its last build ends and the plan's
	// topological order starts the longest one last (build_dispatch.go). Only
	// the starts move: every result stays at its own action's index, so what
	// follows reads the group in the group's order whatever this permutation
	// was.
	order := make([]int, len(admitted))
	for index := range order {
		order[index] = index
	}
	if !planning {
		order = playbook.dispatchOrder(ctx, admitted)
	}
	for _, index := range order {
		group.Go(func() error {
			outcome, execErr := playbook.policy.Execute(ctx, admitted[index])
			next[index], failures[index] = outcome, execErr
			// Never non-nil: a sibling build that is already minutes in has
			// layers pushed and a cache entry to record, and cancelling it
			// would throw that away to report a failure the operator will see
			// anyway. Letting the group finish costs at most the longest build
			// still running and names every service that failed instead of
			// whichever one failed first.
			return nil
		})
	}
	_ = group.Wait()

	if err = playbook.recordConcurrentFailures(admitted, failures); err != nil {
		return false, err
	}
	return playbook.settleConcurrent(ctx, w, admitted, next, plan), nil
}

// admit applies the filters the serial loop applies — a paused action, an
// ignored one, one already executed — and records what is left, in order.
func (playbook *Playbook) admit(ctx context.Context, actions []Action) []Action {
	w := wool.Get(ctx).In("work.admit")
	admitted := make([]Action, 0, len(actions))
	for _, action := range actions {
		switch {
		case playbook.pause.Handle(ctx, action):
			w.Trace("discarding action", wool.Field("action", action))
		case playbook.ignore(ctx, action):
			w.Trace("ignoring action", wool.Field("action", action))
		case playbook.previouslyExecuted(ctx, action):
			w.Trace("previously executed", wool.Field("action", action))
		default:
			playbook.record(action)
			admitted = append(admitted, action)
		}
	}
	return admitted
}

// buildPhaseSize is how many image builds a group holds: the size of the build
// phase it is, or 0 for a group of anything else, which opens no phase worth
// reporting.
func buildPhaseSize(actions []Action) int {
	size := 0
	for _, action := range actions {
		if action.Type == BuilderBuild {
			size++
		}
	}
	return size
}

// recordConcurrentFailures attributes the group's failures in the group's order
// and returns one error naming every service that failed.
//
// Every failure is reported, not just the first: the whole point of running the
// group together is that one run tells the operator everything that is wrong,
// instead of one failing build per render.
func (playbook *Playbook) recordConcurrentFailures(admitted []Action, failures []error) error {
	var failed []string
	var causes []error
	for index, action := range admitted {
		if failures[index] == nil {
			continue
		}
		playbook.recordFailure(action)
		failed = append(failed, action.Service)
		causes = append(causes, fmt.Errorf("%s: %w", action.Service, failures[index]))
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("action %s failed on %d of %d services (%s): %w",
		admitted[0].Type, len(failed), len(admitted), strings.Join(failed, ", "), errors.Join(causes...))
}

// settleConcurrent does the per-action bookkeeping of a completed group in the
// group's order: the pause a result asks for, the next actions it queues, its
// signal, and whether it is the playbook's stopping point.
func (playbook *Playbook) settleConcurrent(
	ctx context.Context, w *wool.Wool, admitted []Action, next [][]Action, plan *ActionPlan,
) bool {
	stop := false
	for index, action := range admitted {
		if p, ok := playbook.pause.IsPause(ctx, next[index]); ok {
			w.Warn(fmt.Sprintf("service %s failing, waiting for successful re-load", action.Service))
			playbook.record(*p)
			playbook.signal(ctx, *p)
			continue
		}
		plan.Add(ctx, next[index]...)
		playbook.signal(ctx, action)
		// Asked of every action rather than returned on the first: the group is
		// already finished, so the remaining signals and queued actions are
		// owed whatever the stopping policy says about an earlier one.
		if playbook.stopAfter(ctx, action) {
			w.Trace("stopping", wool.Field("action", action))
			stop = true
		}
	}
	return stop
}

type IgnoreFunc func(ctx context.Context, action Action) bool

func (playbook *Playbook) WithIgnore(ignore IgnoreFunc) {
	playbook.ignorer = ignore
}

func (playbook *Playbook) signal(_ context.Context, action Action) {
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
	if action.Type == RuntimeBegin {
		return
	}
	if action.Failed {
		return
	}
	playbook.executed = append(playbook.executed, action)
}
