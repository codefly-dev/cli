package orchestration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/codefly-dev/core/architecture"
	"github.com/codefly-dev/core/wool"
)

type ActionType string

const (
	RuntimeBegin ActionType = "runtime-begin"
	RuntimeLoad  ActionType = "runtime-load"
	RuntimeInit  ActionType = "runtime-init"
	RuntimeStart ActionType = "runtime-run"
	RuntimeTest  ActionType = "runtime-test"
	RuntimeStop  ActionType = "runtime-stop"

	BuilderBegin  ActionType = "builder-begin"
	BuilderLoad   ActionType = "builder-load"
	BuilderInit   ActionType = "builder-init"
	BuilderBuild  ActionType = "builder-build"
	BuilderSync   ActionType = "builder-sync"
	BuilderDeploy ActionType = "builder-deploy"
)

// Implement some kind of SharedState machine

type Action struct {
	Type    ActionType
	Service string
	Failed  bool
	Round   int
}

func (action *Action) String() string {
	return fmt.Sprintf("%s:%s (%d)", action.Type, action.Service, action.Round)
}

func (action *Action) ShortString() string {
	return fmt.Sprintf("%s:%s", action.Type, action.Service)
}

type ActionGroup struct {
	actions []Action
	round   int
}

func (g *ActionGroup) String() string {
	var actions []string
	for _, action := range g.actions {
		actions = append(actions, action.String())
	}
	return fmt.Sprintf("[%s] (Round %d)", strings.Join(actions, " -> "), g.round)
}

func (action *Action) Next(t ActionType) Action {
	return Action{
		Type:    t,
		Service: action.Service,
		Round:   action.Round,
	}
}

func (action *Action) NextFor(t ActionType, services ...architecture.Service) []Action {
	var out []Action
	for _, service := range services {
		out = append(out, Action{Type: t, Service: service.Unique, Round: action.Round})
	}
	return out
}

func (action *Action) Failing() Action {
	return Action{
		Type:    action.Type,
		Service: action.Service,
		Failed:  true,
		Round:   action.Round,
	}
}

type ActionManager struct {
	sync.Mutex
	actions chan ActionGroup
	round   int
	// pending tracks in-flight detached send() delivery goroutines so a
	// shutting-down reader can drain them deterministically (see drainPending)
	// instead of guessing a fixed wait.
	pending sync.WaitGroup
}

func NewActionManager() *ActionManager {
	// Use a 1-buffered channel to ensure order
	return &ActionManager{
		actions: make(chan ActionGroup, 1),
	}
}

func (manager *ActionManager) Round() int {
	manager.Mutex.Lock()
	defer manager.Mutex.Unlock()
	return manager.round
}

func (manager *ActionManager) bumpRound() {
	manager.Mutex.Lock()
	manager.round++
	manager.Mutex.Unlock()
}

// Send actions as a group
func (manager *ActionManager) send(ctx context.Context, actions ...Action) {
	if len(actions) == 0 {
		return
	}
	w := wool.Get(ctx).In("ActionManager:Send")
	round := manager.Round()
	for i := range actions {
		actions[i].Round = round
	}
	group := ActionGroup{actions: actions, round: round}
	w.Trace("sending actions", wool.Field("actions", group))
	// Deliver asynchronously, but DETACHED from the caller's ctx. send() is
	// fire-and-forget — callers (notably the Follow loop driving a hot-reload
	// restart) bound their callback ctx and cancel it the instant the call
	// returns. Tying delivery to that ctx loses the race against this
	// goroutine, silently DROPPING the queued action (e.g. the runtime-run
	// that restarts the rebuilt binary — hot-reload then stops but never comes
	// back). WithoutCancel keeps wool values for logging; an independent
	// timeout is the leak-guard so a permanently-absent reader can't pile up
	// goroutines forever.
	// If the caller's ctx is already cancelled we're in an actual shutdown, not
	// the brief hot-reload callback cancellation the WithoutCancel detach guards
	// against. Don't spawn a goroutine that would idle for the full delivery
	// window during a SIGINT/SIGTERM pile-up.
	select {
	case <-ctx.Done():
		w.Trace("ctx already cancelled, not delivering actions", wool.Field("actions", group))
		return
	default:
	}
	deliverCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	manager.pending.Add(1)
	go func() {
		defer manager.pending.Done()
		defer cancel()
		select {
		case manager.actions <- group:
		case <-deliverCtx.Done():
			w.Warn("dropped action group: no reader within delivery window", wool.Field("actions", group))
		}
	}()
	w.Trace("sent actions", wool.Field("actions", group))
}

// drainPending unblocks any in-flight send() delivery goroutines when the
// reader is shutting down. It reads (and discards) queued groups until every
// detached sender has completed — delivered or self-timed-out. Termination is
// deterministic: it's gated on the pending WaitGroup, so it returns the instant
// the last sender drains rather than after a guessed fixed interval. Each
// blocked sender is freed by exactly one channel read here, so this can't spin.
func (manager *ActionManager) drainPending() {
	done := make(chan struct{})
	go func() {
		manager.pending.Wait()
		close(done)
	}()
	for {
		select {
		case <-manager.actions:
		case <-done:
			return
		}
	}
}

func (manager *ActionManager) Group() chan ActionGroup {
	return manager.actions
}

func (manager *ActionManager) NewActionPlan() *ActionPlan {
	round := manager.Round()
	return &ActionPlan{
		round: round,
		known: make(map[Action]bool),
	}
}

type ActionPlan struct {
	round   int
	actions []Action
	known   map[Action]bool
}

func (plan *ActionPlan) Add(ctx context.Context, actions ...Action) {
	w := wool.Get(ctx).In("ActionPlan:Add")
	w.Trace("considering actions in plan", wool.Field("actions", actions))
	for _, action := range actions {
		action.Round = plan.round
		if _, ok := plan.known[action]; !ok {
			w.Trace("adding action to plan", wool.Field("action", action.String()))
			plan.actions = append(plan.actions, action)
			plan.known[action] = true
		} else {
			w.Trace("action already known", wool.Field("action", action.String()))
		}
	}
}
