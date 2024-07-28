package orchestration

import (
	"context"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/wool"
)

type PauseManager struct {
	action  *Action
	spinner *spinner.Spinner
}

func NewPauseManager() *PauseManager {
	return &PauseManager{spinner: cli.Spinner()}
}

func (pause *PauseManager) IsPause(ctx context.Context, next []Action) (*Action, bool) {
	w := wool.Get(ctx).In("PauseManager.IsPause")
	// In Pause, we get only one action Wait
	if len(next) != 1 {
		return nil, false
	}
	if next[0].Failed {
		failed := next[0]
		w.Debug("got a failure", wool.Field("action", failed))
		pause.action = &failed
		pause.spinner.Start()
		return &next[0], true
	}
	return nil, false
}

func (pause *PauseManager) Handle(ctx context.Context, action Action) bool {
	w := wool.Get(ctx).In("PauseManager.Handle")
	if pause.action == nil {
		return false
	}
	w.Debug("received", wool.Field("action", action))
	w.Debug("waiting for", wool.Field("action", pause.action))
	if pause.action.Service == action.Service && pause.action.Type == action.Type {
		// This "un-pause"
		w.Debug("un-pausing")
		pause.action = nil
		pause.spinner.Stop()
		return false
	}
	w.Debug("NOPE: GOT", wool.Field("action", action))
	return true
}
