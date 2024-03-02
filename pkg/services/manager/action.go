package manager

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/wool"
)

type ActionType string

const (
	RuntimeBegin ActionType = "runtime-begin"
	RuntimeLoad  ActionType = "runtime-load"
	RuntimeInit  ActionType = "runtime-init"
	RuntimeStart ActionType = "runtime-run"
	RuntimeStop  ActionType = "runtime-stop"

	BuilderBegin  ActionType = "builder-begin"
	BuilderLoad   ActionType = "builder-load"
	BuilderInit   ActionType = "builder-init"
	BuilderBuild  ActionType = "builder-build"
	BuilderSync   ActionType = "builder-sync"
	BuilderDeploy ActionType = "builder-deploy"
)

// Implement some kind of sharedState machine

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
}

func NewActionManager() *ActionManager {
	// Use a 1-buffered channel to ensure order
	return &ActionManager{
		actions: make(chan ActionGroup),
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
	w.Debug("sending actions", wool.Field("actions", group))
	go func() {
		manager.actions <- group
	}()
	w.Debug("sent actions", wool.Field("actions", group))
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
	w.Debug("considering actions in plan", wool.Field("actions", actions))
	for _, action := range actions {
		action.Round = plan.round
		if _, ok := plan.known[action]; !ok {
			w.Debug("adding action to plan", wool.Field("action", action.String()))
			plan.actions = append(plan.actions, action)
			plan.known[action] = true
		} else {
			w.Debug("action already known", wool.Field("action", action.String()))
		}
	}
}
