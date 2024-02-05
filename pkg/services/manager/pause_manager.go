package manager

import (
	"github.com/briandowns/spinner"
	"github.com/codefly-dev/cli/pkg/cli"
)

type PauseManager struct {
	action  *Action
	spinner *spinner.Spinner
}

func NewPauseManager() *PauseManager {
	return &PauseManager{spinner: cli.Spinner()}
}

func (pause *PauseManager) Set(action Action) {
	pause.action = &action
}

func (pause *PauseManager) IsPause(next []Action) (*Action, bool) {
	// In Pause, we get only one action Wait
	if len(next) != 1 {
		return nil, false
	}
	if next[0].Type == RuntimeFailing {
		pause.spinner.Start()
		return &next[0], true
	}
	return nil, false
}

func (pause *PauseManager) Handle(action Action) bool {
	if pause.action == nil {
		return false
	}
	if pause.action.Service == action.Service && pause.action.Type == action.Type {
		// This "un-pause"
		pause.action = nil
		pause.spinner.Stop()
		return false
	}
	return true
}
