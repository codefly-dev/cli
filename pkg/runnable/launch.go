package runnable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"time"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/runners/base"
	"golang.org/x/sys/unix"
)

const (
	// InvocationFile and ResultFile are the two framing documents of one
	// invocation, written into the caller's invocation directory. They are
	// kept after the process ends: they are the evidence of what was
	// dispatched and what came back.
	InvocationFile = "invocation.json"
	ResultFile     = "result.json"
	// terminationGrace is how long the invocation's process group gets to
	// honour SIGTERM before the launcher escalates to SIGKILL.
	terminationGrace = 5 * time.Second
)

// NativeLauncher supervises invocations of one installed native binding. It is
// the physical process boundary: it receives an approved execution
// specification and bounded invocation material, runs exactly the invocation it
// is handed, and reports what it observed. It never decides that an invocation
// happens, or happens again — a lost completion stays uncertain here and is
// resolved by whoever owns the package's recovery policy.
type NativeLauncher struct {
	pkg        *basev0.RunnablePackage
	root       string
	executable string
	arguments  []string
	budget     time.Duration
}

// NewNativeLauncher accepts an installed native binding this host can execute,
// where root is the directory the binding's artifact was unpacked into.
// Everything it checks is an installation fact rather than a property of one
// invocation, so a launcher that exists can dispatch: the binding installs this
// package, it is bound to the native facility, its artifact is built for this
// platform, and its launch command is an executable inside the installed root.
func NewNativeLauncher(pkg *basev0.RunnablePackage, binding *basev0.RunnableBinding, root string) (*NativeLauncher, error) {
	if err := corerunnable.VerifyBinding(binding, pkg); err != nil {
		return nil, fmt.Errorf("verify installed binding: %w", err)
	}
	// core ties an artifact kind to its facility, so refusing anything but the
	// native facility is also what keeps an image out of the local launcher.
	if facility := binding.GetFacility().GetKind(); facility != basev0.RunnableFacility_NATIVE {
		return nil, fmt.Errorf("the local launcher executes %s bindings, not %s", basev0.RunnableFacility_NATIVE, facility)
	}
	artifact := binding.GetArtifact()
	if host := runtime.GOOS + "/" + runtime.GOARCH; artifact.GetPlatform() != host {
		return nil, fmt.Errorf("installed artifact is built for %s, not this host's %s", artifact.GetPlatform(), host)
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("installed root %s must be absolute", root)
	}
	argv := artifact.GetCommand()
	if len(argv) == 0 {
		return nil, fmt.Errorf("installed native artifact carries no launch command")
	}
	if !filepath.IsLocal(argv[0]) {
		return nil, fmt.Errorf("launch command %q must be a path inside the installed package", argv[0])
	}
	executable := filepath.Join(root, argv[0])
	info, err := os.Stat(executable)
	if err != nil {
		return nil, fmt.Errorf("installed launch command: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil, fmt.Errorf("installed launch command %s is not an executable file", argv[0])
	}
	return &NativeLauncher{
		pkg: pkg, root: root, executable: executable, arguments: argv[1:],
		budget: pkg.GetExecution().GetTimeout().AsDuration(),
	}, nil
}

// Run is the material for one invocation of an installed binding.
type Run struct {
	// Invocation is the recorded invocation to execute.
	Invocation *basev0.RunnableInvocation
	// Directory is where the launcher writes this invocation's two framing
	// documents. One directory holds one invocation: a directory that already
	// holds either document belongs to another run, and reading its result as
	// this one's would report an outcome this run never produced.
	Directory string
	// Environment is what the facility resolved for the package's declared
	// dependencies and configurations. With the framing it is the whole
	// environment the process gets.
	Environment map[string]string
	// Stdout and Stderr receive up to the package's max_log_bytes of each
	// stream. They are diagnostics, never completion data, and a nil writer
	// discards them while the completion still records what the stream
	// produced.
	Stdout, Stderr io.Writer
}

// Invoke supervises one invocation and returns the launcher's typed completion.
//
// An error means the invocation was not dispatched at all. Every way a
// dispatched process can end is a completion rather than an error, including
// the ones that leave the operation's effect unproven: classifying them is
// core's job, so a timeout, a crash and a harness that never wrote its result
// mean the same thing in every launcher.
func (l *NativeLauncher) Invoke(ctx context.Context, run Run) (*basev0.RunnableCompletion, error) {
	invocation, err := corerunnable.PrepareInvocation(run.Invocation, l.pkg)
	if err != nil {
		return nil, err
	}
	budget := invocation.GetDeadline().AsTime().Sub(invocation.GetIssuedAt().AsTime())
	if budget > l.budget {
		return nil, fmt.Errorf("invocation budget %s is longer than the package's declared timeout %s", budget, l.budget)
	}
	if !filepath.IsAbs(run.Directory) {
		return nil, fmt.Errorf("invocation directory %s must be absolute", run.Directory)
	}
	if err = os.MkdirAll(run.Directory, 0700); err != nil {
		return nil, err
	}
	invocationPath := filepath.Join(run.Directory, InvocationFile)
	resultPath := filepath.Join(run.Directory, ResultFile)
	document, err := corerunnable.EncodeInvocation(invocation)
	if err != nil {
		return nil, err
	}
	if err = writeOnce(invocationPath, document); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(resultPath); err == nil {
		return nil, fmt.Errorf("invocation directory %s already holds a result document", run.Directory)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	environment, err := l.environment(invocation, invocationPath, resultPath, run.Environment)
	if err != nil {
		return nil, err
	}
	bound := l.pkg.GetExecution().GetMaxLogBytes()
	stdout, stderr := &boundedWriter{to: run.Stdout, allowed: bound}, &boundedWriter{to: run.Stderr, allowed: bound}
	// #nosec G204 -- the command is the installed artifact's own launch command,
	// tied to a digest-pinned package by core's VerifyBinding, confined to the
	// installed root, and executed directly rather than through a shell.
	command := exec.Command(l.executable, l.arguments...)
	command.Dir = l.root
	command.Env = environment
	command.Stdout, command.Stderr = stdout, stderr
	interruptible := l.pkg.GetExecution().GetCancellation() == basev0.RunnableExecution_CANCELLATION_SIGNAL
	observed, err := supervise(ctx, command, invocation.GetDeadline().AsTime(), interruptible)
	if err != nil {
		return nil, err
	}
	observed.Stdout, observed.Stderr = stdout.stream(), stderr.stream()
	// The harness renames its document onto the result path, so one that exists
	// is complete; one that is absent is no result at all rather than a partial
	// one, and core classifies the difference.
	if observed.Result, err = os.ReadFile(resultPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		observed.Result = nil
	}
	return corerunnable.Complete(invocation, l.pkg, observed)
}

// supervise runs command as its own process group and reports raw process
// facts. The group, not the leader, is the unit of both supervision and
// cleanup: a descendant that outlives the process the launcher started is still
// a resource this invocation acquired.
func supervise(ctx context.Context, command *exec.Cmd, deadline time.Time, interruptible bool) (corerunnable.Observation, error) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		// Nothing is dispatched. An invocation whose deadline has already
		// passed has no work left to admit, and starting a process only to kill
		// it would spend the operation's one attempt and make a timeout
		// indistinguishable from a crash.
		now := time.Now()
		return corerunnable.Observation{StartedAt: now, EndedAt: now, Ended: corerunnable.EndedOnDeadline}, nil
	}
	startedAt := time.Now()
	group, err := base.StartOwnedProcessGroup(command)
	if err != nil {
		return corerunnable.Observation{}, fmt.Errorf("start invocation: %w", err)
	}
	var waitErr error
	var endedAt time.Time
	waited := make(chan struct{})
	go func() {
		waitErr = command.Wait()
		endedAt = time.Now()
		close(waited)
	}()
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	// A package that does not declare signal cancellation cannot be interrupted:
	// it runs to its deadline even once the caller has stopped waiting, because
	// advertising a cancellation the harness does not implement would report
	// work as abandoned that is still running.
	var interrupted <-chan struct{}
	if interruptible {
		interrupted = ctx.Done()
	}
	ended := corerunnable.EndedOnItsOwn
	select {
	case <-waited:
	case <-timer.C:
		ended = corerunnable.EndedOnDeadline
	case <-interrupted:
		ended = corerunnable.EndedOnCancel
	}
	// Terminating is both how the launcher ends an invocation it stopped waiting
	// for and how it releases one that ended on its own. The context is detached
	// because a cancelled invocation must still be cleaned up.
	terminateErr := group.Terminate(context.WithoutCancel(ctx), terminationGrace)
	<-waited
	if terminateErr != nil {
		return corerunnable.Observation{}, fmt.Errorf("end the invocation's process group: %w", terminateErr)
	}
	// A non-zero exit is an observation, not a supervision failure; anything
	// else means the launcher never saw the process end.
	var exited *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exited) {
		return corerunnable.Observation{}, fmt.Errorf("supervise invocation: %w", waitErr)
	}
	observation := corerunnable.Observation{StartedAt: startedAt, EndedAt: endedAt, Ended: ended}
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		observation.Signal = unix.SignalName(status.Signal())
	} else {
		// #nosec G115 -- an exit status is -1 or 0..255, never a truncating value.
		observation.ExitCode = int32(command.ProcessState.ExitCode())
	}
	return observation, nil
}

// environment is the whole environment the invocation's process gets: the
// framing the harness reads its two documents through, plus exactly what the
// facility resolved for the package's declared dependencies and configurations.
// Nothing of the launcher's own environment is inherited, so an ambient
// credential that happens to sit in it never reaches a handler.
func (l *NativeLauncher) environment(invocation *basev0.RunnableInvocation, invocationPath, resultPath string, resolved map[string]string) ([]string, error) {
	framing := corerunnable.InvocationEnvironment(invocation, invocationPath, resultPath)
	environment := make([]string, 0, len(framing)+len(resolved))
	for name, value := range resolved {
		if _, reserved := framing[name]; reserved {
			return nil, fmt.Errorf("resolved environment must not set %s: the launcher owns the framing", name)
		}
		environment = append(environment, name+"="+value)
	}
	for name, value := range framing {
		environment = append(environment, name+"="+value)
	}
	slices.Sort(environment)
	return environment, nil
}

func writeOnce(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	return errors.Join(writeErr, file.Close())
}

// boundedWriter forwards at most allowed bytes to one log stream's writer and
// counts everything the stream produced. It never reports an error to the
// process: exceeding the log bound truncates diagnostics, and unlike the output
// bound it must not change the outcome of the operation.
type boundedWriter struct {
	to       io.Writer
	allowed  uint64
	produced uint64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if room := w.allowed - min(w.produced, w.allowed); w.to != nil && room > 0 {
		//nolint:errcheck // A caller's log sink failing is not this invocation's outcome.
		w.to.Write(p[:min(room, uint64(len(p)))])
	}
	w.produced += uint64(len(p))
	return len(p), nil
}

func (w *boundedWriter) stream() corerunnable.LogStream {
	return corerunnable.LogStream{Bytes: min(w.produced, w.allowed), Truncated: w.produced > w.allowed}
}
