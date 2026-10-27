package cmd

import "testing"

func TestClearAndStopCommandsReturnErrors(t *testing.T) {
	for name, command := range map[string]struct {
		run  bool
		runE bool
	}{
		"clear": {run: ClearCmd.Run != nil, runE: ClearCmd.RunE != nil},
		"stop":  {run: StopCmd.Run != nil, runE: StopCmd.RunE != nil},
	} {
		if command.run || !command.runE {
			t.Fatalf("%s must return errors through RunE", name)
		}
	}
}

func TestStopOptionsDoNotInheritClearFlags(t *testing.T) {
	clearKeepProcesses = true
	clearKeepContainers = false
	clearDryRun = true
	t.Cleanup(func() {
		clearKeepProcesses = false
		clearKeepContainers = false
		clearDryRun = false
	})

	got := stopClearOptions()
	if got.verb != "stop" || got.keepProcesses || !got.keepContainers || got.dryRun {
		t.Fatalf("stop options inherited clear state: %+v", got)
	}
}
