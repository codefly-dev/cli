package orchestration

import (
	"context"
	"testing"
	"time"

	"github.com/codefly-dev/core/wool"
)

// TestActionManager_SendSurvivesCallerCancel is the regression for the
// hot-reload "stops but never restarts" bug. send() is fire-and-forget; the
// Follow loop that drives a restart bounds its callback ctx and cancels it the
// instant the call returns. If delivery is tied to that ctx, the queued action
// (the runtime-run that restarts the rebuilt binary) is dropped before any
// reader sees it. Delivery must be detached from the caller's ctx: a reader
// that arrives slightly later must still receive the group.
func TestActionManager_SendSurvivesCallerCancel(t *testing.T) {
	ctx := wool.Get(context.Background()).Inject(context.Background())
	mgr := NewActionManager()
	mgr.bumpRound()

	// Caller ctx is cancelled the instant send() returns — exactly what the
	// Follow loop's bounded callback ctx does.
	callerCtx, cancel := context.WithCancel(ctx)
	mgr.send(callerCtx, Action{Type: RuntimeStart, Service: "mind/mind"})
	cancel()

	// A reader that arrives a beat later (Work was busy finishing the stop)
	// must still receive the action.
	time.Sleep(50 * time.Millisecond)
	select {
	case group := <-mgr.Group():
		if len(group.actions) != 1 || group.actions[0].Type != RuntimeStart {
			t.Fatalf("unexpected group: %+v", group)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("action was dropped after caller cancelled its ctx — hot-reload restart is lost")
	}
}
