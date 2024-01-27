package manager

import (
	"context"
	"fmt"
	"strings"

	"github.com/codefly-dev/cli/pkg/architecture"
	"github.com/codefly-dev/core/wool"
)

type ActionType string

const (
	RuntimeCreate ActionType = "runtime-create"
	RuntimeLoad   ActionType = "runtime-load"
	RuntimeInit   ActionType = "runtime-init"
	RuntimeStart  ActionType = "runtime-run"
	RuntimeStop   ActionType = "runtime-stop"
)

// Implement some kind of state machine

type Action struct {
	Type    ActionType
	Service string
	round   int
}

func (action *Action) String() string {
	return fmt.Sprintf("%s:%s (%d)", action.Type, action.Service, action.round)
}

type ActionGroup struct {
	actions []Action
	round   int
}

func (g *ActionGroup) NewActionPlan() *ActionPlan {
	return NewActionPlan()
}

func NewActionPlan() *ActionPlan {
	return &ActionPlan{
		known: make(map[Action]bool),
	}
}

func (g *ActionGroup) String() string {
	var actions []string
	for _, action := range g.actions {
		actions = append(actions, action.String())
	}
	return fmt.Sprintf("[%s] (round %d)", strings.Join(actions, " -> "), g.round)
}

func (action *Action) Next(t ActionType) Action {
	return Action{
		Type:    t,
		Service: action.Service,
		round:   action.round,
	}
}

func (action *Action) NextFor(t ActionType, services ...architecture.Service) []Action {
	var out []Action
	for _, service := range services {
		out = append(out, Action{Type: t, Service: service.Unique, round: action.round})
	}
	return out
}

type ActionManager struct {
	actions chan ActionGroup
	round   int
}

func NewActionManager() *ActionManager {
	// Use a 1-buffered channel to ensure order
	return &ActionManager{
		actions: make(chan ActionGroup),
	}
}

// Send actions as a group
func (manager *ActionManager) Send(ctx context.Context, actions ...Action) {
	if len(actions) == 0 {
		return
	}
	w := wool.Get(ctx).In("ActionManager:Send")
	manager.round++
	for i := range actions {
		actions[i].round = manager.round
	}
	group := ActionGroup{actions: actions, round: manager.round}
	w.Debug("sending actions", wool.Field("actions", group.String()))
	go func() {
		manager.actions <- group
	}()
	w.Focus("sent actions", wool.Field("actions", group.String()))
}

func (manager *ActionManager) Group() chan ActionGroup {
	return manager.actions
}

type ActionPlan struct {
	round   int
	actions []Action
	known   map[Action]bool
}

func (plan *ActionPlan) Add(actions ...Action) {
	for _, action := range actions {
		action.round = plan.round
		if _, ok := plan.known[action]; !ok {
			plan.actions = append(plan.actions, actions...)
			plan.known[action] = true
		}
	}
}
