package cmd

import (
	"context"
	"reflect"
	"testing"
)

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

func TestParseCodeflyOwnedPIDs(t *testing.T) {
	out := []byte(`  10 /usr/local/bin/codefly run service api
  11 /Users/me/.codefly/agents/go/bin/agent serve
  12 /usr/bin/editor /repo/codefly.dev/file.go
  13 /usr/local/bin/codefly stop
bad /usr/local/bin/codefly
`)
	if got, want := parseCodeflyOwnedPIDs(out, 13), []int{10, 11}; !reflect.DeepEqual(got, want) {
		t.Fatalf("owned PIDs = %v, want %v", got, want)
	}
}

func TestCodeflyOwnedPIDsReturnsProcessErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := codeflyOwnedPIDs(ctx, -1); err == nil {
		t.Fatal("expected cancelled process enumeration to return an error")
	}
}
