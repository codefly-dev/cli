package orchestration

import (
	"context"
	"sync"

	"github.com/briandowns/spinner"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/wool"
)

type PauseManager struct {
	// action is written from Playbook.Work (single goroutine in production)
	// and read by the same goroutine via IsPause/Handle. Guarded by mu so
	// external callers (Flow.Stop in particular) can safely Clear it.
	mu      sync.Mutex
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
		pause.mu.Lock()
		pause.action = &failed
		pause.mu.Unlock()
		pause.spinner.Start()
		return &next[0], true
	}
	return nil, false
}

func (pause *PauseManager) Handle(ctx context.Context, action Action) bool {
	w := wool.Get(ctx).In("PauseManager.Handle")
	pause.mu.Lock()
	current := pause.action
	pause.mu.Unlock()
	if current == nil {
		return false
	}
	w.Debug("received", wool.Field("action", action))
	w.Debug("waiting for", wool.Field("action", current))
	if current.Service == action.Service && current.Type == action.Type {
		// This "un-pause"
		w.Debug("un-pausing")
		pause.mu.Lock()
		pause.action = nil
		pause.mu.Unlock()
		pause.spinner.Stop()
		return false
	}
	w.Debug("NOPE: GOT", wool.Field("action", action))
	return true
}

// Clear force-resets the pause state. Called on external Flow.Stop /
// Flow.Shutdown to prevent a stale pause from outliving its flow: the
// spinner would otherwise keep spinning even as the whole stack tears
// down.
func (pause *PauseManager) Clear() {
	if pause == nil {
		return
	}
	pause.mu.Lock()
	had := pause.action != nil
	pause.action = nil
	pause.mu.Unlock()
	if had && pause.spinner != nil {
		pause.spinner.Stop()
	}
}
