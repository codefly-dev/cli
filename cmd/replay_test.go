package cmd

import "testing"

func TestReplayRequiresTrack(t *testing.T) {
	previous := track
	track = ""
	defer func() { track = previous }()
	if err := ReplayCmd.RunE(ReplayCmd, nil); err == nil {
		t.Fatal("replay without --track returned success")
	}
}
