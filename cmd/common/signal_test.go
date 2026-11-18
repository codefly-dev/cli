package common

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestContextFromSignalsCancelsFirstAndForcesSecond(t *testing.T) {
	signals := make(chan os.Signal, 2)
	exits := make(chan int, 1)
	const interval = 400 * time.Millisecond
	ctx, stop := contextFromSignals(context.Background(), signals, interval, func(code int) { exits <- code })
	defer stop()

	signals <- os.Interrupt
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("first signal did not cancel context")
	}
	// The same request delivered again, inside the interval, is no force.
	signals <- syscall.SIGTERM
	signals <- os.Interrupt
	select {
	case code := <-exits:
		t.Fatalf("a signal repeated at once forced exit %d", code)
	case <-time.After(100 * time.Millisecond):
	}

	time.Sleep(interval)
	signals <- syscall.SIGTERM
	select {
	case code := <-exits:
		if code != 130 {
			t.Fatalf("force exit code = %d, want 130", code)
		}
	case <-time.After(time.Second):
		t.Fatal("a signal repeated after the interval did not force exit")
	}
}

// What a forced quit must still do is done before the exit, in the order it
// was registered, and a withdrawn hook is not run.
func TestAForcedQuitRunsWhatWasRegisteredForIt(t *testing.T) {
	var ran []string
	first := OnForcedQuit(func(context.Context) { ran = append(ran, "first") })
	withdrawn := OnForcedQuit(func(context.Context) { ran = append(ran, "withdrawn") })
	last := OnForcedQuit(func(ctx context.Context) {
		if _, bounded := ctx.Deadline(); bounded {
			ran = append(ran, "last")
		}
	})
	defer first()
	defer last()
	withdrawn()

	runForcedQuitHooks()
	if len(ran) != 2 || ran[0] != "first" || ran[1] != "last" {
		t.Fatalf("a forced quit ran %q, want first then last, each bounded", ran)
	}
}

func TestContextFromSignalsStopIsIdempotent(t *testing.T) {
	ctx, stop := contextFromSignals(context.Background(), make(chan os.Signal), time.Second, func(int) {
		t.Fatal("unexpected force exit")
	})
	stop()
	stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel context")
	}
}

// SignalHandled says whether a SignalContext is listening, and stops saying so
// once every one has stopped, however many times each stop is called.
func TestSignalHandledFollowsTheListeningContexts(t *testing.T) {
	if SignalHandled() {
		t.Fatal("no SignalContext is listening yet")
	}
	_, stopFirst := SignalContext(context.Background())
	_, stopSecond := SignalContext(context.Background())
	stopFirst()
	stopFirst()
	if !SignalHandled() {
		t.Fatal("the second SignalContext is still listening")
	}
	stopSecond()
	if SignalHandled() {
		t.Fatal("every SignalContext has stopped")
	}
}

// A run ends in order on each signal that asks a process to end: Ctrl-C, a
// kill, and the hangup a closed terminal or a stopped parent shell sends. A
// hangup used to be ignored, so the stack outlived whatever started it.
func TestSignalContextStopsOnEverySignalThatEndsAProcess(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			ctx, stop := SignalContext(context.Background())
			defer stop()
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				t.Fatalf("%s did not stop the command", sig)
			}
		})
	}
}

// The second signal is a person insisting. A hangup is never that: a terminal
// that closes while the run is already stopping must not cut the stop short.
func TestAHangupNeverForcesTheExit(t *testing.T) {
	signals := make(chan os.Signal, 2)
	exits := make(chan int, 1)
	ctx, stop := contextFromSignals(context.Background(), signals, 100*time.Millisecond, func(code int) { exits <- code })
	defer stop()

	signals <- syscall.SIGTERM
	<-ctx.Done()
	signals <- syscall.SIGHUP
	select {
	case code := <-exits:
		t.Fatalf("a hangup during the stop forced exit %d", code)
	case <-time.After(200 * time.Millisecond):
	}
	signals <- os.Interrupt
	select {
	case <-exits:
	case <-time.After(time.Second):
		t.Fatal("a second Ctrl-C after a hangup did not force the exit")
	}
}

// lostOutputEnv makes this test binary stand in for a command whose output is
// a pipe nobody reads any more; it names the file the command writes once it
// has gone on past that.
const lostOutputEnv = "SIGNAL_TEST_LOST_OUTPUT"

// The process a run was started for read the run's output. When it is killed
// the run has to stop in order, and the first thing a stop does is say so: on
// a pipe nobody reads. That write must fail, not end the command.
func TestOutliveOutputGoesOnPastAReaderThatIsGone(t *testing.T) {
	if survived := os.Getenv(lostOutputEnv); survived != "" {
		defer OutliveOutput()()
		// The reader closes this process's stdin once it has stopped reading.
		_, _ = os.Stdin.Read(make([]byte, 1))
		for range 3 {
			_, _ = os.Stdout.WriteString("stopping the run\n")
		}
		if err := os.WriteFile(survived, []byte("stopped in order"), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}

	survived := t.TempDir() + "/survived"
	command := exec.Command(os.Args[0], "-test.run=^TestOutliveOutputGoesOnPastAReaderThatIsGone$")
	command.Env = append(os.Environ(), lostOutputEnv+"="+survived)
	output, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stdout = write
	cue, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	_ = output.Close()
	_ = cue.Close()

	if err := command.Wait(); err != nil {
		t.Fatalf("the command did not outlive the reader of its output: %v", err)
	}
	if _, err := os.Stat(survived); err != nil {
		t.Fatalf("the command never got past the write nobody read: %v", err)
	}
}
