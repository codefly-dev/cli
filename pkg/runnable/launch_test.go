package runnable_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	runnableops "github.com/codefly-dev/cli/pkg/runnable"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The launcher is a process boundary, so these tests supervise real processes:
// the installed artifact is a script that plays the part of a generated
// harness, and the framing between the two is the real one. The real Python
// agent and its real harness are qualified in lifecycle_integration_test.go.

const artifactDigest = "sha256:a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"

func testPackage(t *testing.T, command []string, adjust func(*basev0.RunnableExecution)) *basev0.RunnablePackage {
	t.Helper()
	execution := &basev0.RunnableExecution{
		Facilities:     []*basev0.RunnableFacility{{Kind: basev0.RunnableFacility_NATIVE}},
		Timeout:        durationpb.New(2 * time.Minute),
		Cancellation:   basev0.RunnableExecution_CANCELLATION_SIGNAL,
		Recovery:       basev0.RunnableExecution_RECOVERY_RECOMPUTE,
		MaxInputBytes:  4096,
		MaxOutputBytes: 4096,
		MaxLogBytes:    4096,
	}
	if adjust != nil {
		adjust(execution)
	}
	pkg, err := corerunnable.PreparePackage(&basev0.RunnablePackage{
		Schema:   corerunnable.PackageSchemaV1,
		Identity: &basev0.RunnableIdentity{Name: "word-count", Module: "operations", Workspace: "qualification", Version: "0.1.0"},
		Agent:    &basev0.Agent{Kind: basev0.Agent_RUNNABLE, Name: "python", Publisher: "codefly.dev", Version: "0.0.1"},
		Contract: &basev0.RunnableContract{
			Protocol: corerunnable.ProtocolV1,
			Input:    &basev0.RunnableSchema{Fields: []*basev0.RunnableField{{Name: "text", Type: basev0.RunnableField_STRING}}},
			Output:   &basev0.RunnableSchema{Fields: []*basev0.RunnableField{{Name: "count", Type: basev0.RunnableField_INTEGER}}},
		},
		Execution: execution,
		Build: &basev0.RunnableBuild{
			Handler:             &basev0.RunnableInputDigest{Path: "handler.py", Digest: artifactDigest},
			HarnessDigest:       artifactDigest,
			Toolchain:           "python-3.12.4",
			ConfigurationDigest: artifactDigest,
		},
		Artifacts: []*basev0.RunnableArtifact{{
			Kind: basev0.RunnableArtifact_NATIVE, Platform: runtime.GOOS + "/" + runtime.GOARCH,
			Reference: "word-count-0.1.0.tar.gz", Digest: artifactDigest, Command: command,
		}},
	})
	require.NoError(t, err)
	return pkg
}

func testBinding(t *testing.T, pkg *basev0.RunnablePackage) *basev0.RunnableBinding {
	t.Helper()
	binding, err := corerunnable.PrepareBinding(&basev0.RunnableBinding{
		Schema: corerunnable.BindingSchemaV1, Identity: pkg.GetIdentity(), PackageDigest: pkg.GetDigest(),
		Facility: &basev0.RunnableFacility{Kind: basev0.RunnableFacility_NATIVE}, Artifact: pkg.GetArtifacts()[0],
	}, pkg)
	require.NoError(t, err)
	return binding
}

// installedHarness writes body as the installed artifact's launch command and
// returns the root it was installed into.
func installedHarness(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "harness"), []byte("#!/bin/sh\n"+body), 0700))
	return root
}

// resultOf is the document a harness writes for a successful invocation.
func resultOf(t *testing.T, invocation string, output string) string {
	t.Helper()
	return encodeResult(t, &basev0.RunnableResult{
		Protocol: corerunnable.ProtocolV1, InvocationId: invocation,
		Status: basev0.RunnableResult_SUCCEEDED, Output: []byte(output),
	})
}

func encodeResult(t *testing.T, result *basev0.RunnableResult) string {
	t.Helper()
	encoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "'", "the document is embedded in a single-quoted shell string")
	return string(encoded)
}

// writeResult is the shell that puts document at the launcher's result path.
func writeResult(document string) string {
	return "printf '%s' '" + document + "' > \"$CODEFLY__RUNNABLE_RESULT\"\n"
}

func invocationFor(pkg *basev0.RunnablePackage, id string, budget time.Duration) *basev0.RunnableInvocation {
	issued := time.Now()
	return &basev0.RunnableInvocation{
		Protocol: corerunnable.ProtocolV1, Runnable: pkg.GetIdentity(), InvocationId: id, IntentId: "intent-1",
		IssuedAt: timestamppb.New(issued), Deadline: timestamppb.New(issued.Add(budget)), Input: []byte(`{"text":"one two three"}`),
	}
}

// invoke runs one invocation of an installed harness and returns its completion.
func invoke(t *testing.T, ctx context.Context, pkg *basev0.RunnablePackage, root string, run runnableops.Run) *basev0.RunnableCompletion {
	t.Helper()
	launcher, err := runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), root)
	require.NoError(t, err)
	if run.Directory == "" {
		run.Directory = filepath.Join(t.TempDir(), "invocation")
	}
	completion, err := launcher.Invoke(ctx, run)
	require.NoError(t, err)
	return completion
}

func TestInvokeReportsTheHandlerOutputTheHarnessProved(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	invocation := invocationFor(pkg, "inv-1", 30*time.Second)
	root := installedHarness(t, writeResult(resultOf(t, "inv-1", `{"count":3}`)))
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocation})

	require.Equal(t, basev0.RunnableCompletion_SUCCEEDED, completion.GetOutcome())
	require.JSONEq(t, `{"count":3}`, string(completion.GetResult().GetOutput()))
	require.True(t, corerunnable.OutcomeIsCertain(completion.GetOutcome()))
	require.Equal(t, invocation.GetInvocationId(), completion.GetInvocationId())
}

func TestInvokeReportsATypedHandlerFailureAsACompletion(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	failed := encodeResult(t, &basev0.RunnableResult{
		Protocol: corerunnable.ProtocolV1, InvocationId: "inv-1", Status: basev0.RunnableResult_FAILED,
		Error: &basev0.RunnableError{Code: "insufficient_funds", Message: "the card was declined"},
	})
	root := installedHarness(t, writeResult(failed)+"exit 1\n")
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second)})

	// The operation ran and reported why it failed, so the outcome is certain
	// even though the process exited non-zero.
	require.Equal(t, basev0.RunnableCompletion_FAILED, completion.GetOutcome())
	require.Equal(t, "insufficient_funds", completion.GetResult().GetError().GetCode())
	require.True(t, corerunnable.OutcomeIsCertain(completion.GetOutcome()))
}

func TestInvokeDoesNotAcceptAnExitStatusAsACompletion(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		outcome basev0.RunnableCompletion_Outcome
	}{
		{"a process that wrote no result", "exit 0\n", basev0.RunnableCompletion_MISSING_OUTPUT},
		{"a process that crashed", "exit 7\n", basev0.RunnableCompletion_CRASHED},
		{"another invocation's result", writeResult(`{"protocol":"codefly.runnable/v1","invocation_id":"inv-2","status":"SUCCEEDED","output":"e30="}`), basev0.RunnableCompletion_INVALID_OUTPUT},
		{"a malformed result", writeResult("not a document"), basev0.RunnableCompletion_INVALID_OUTPUT},
		{"a result with no payload", writeResult(`{"protocol":"codefly.runnable/v1","invocation_id":"inv-1","status":"SUCCEEDED"}`), basev0.RunnableCompletion_INVALID_OUTPUT},
	} {
		t.Run(test.name, func(t *testing.T) {
			pkg := testPackage(t, []string{"harness"}, nil)
			root := installedHarness(t, test.body)
			completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second)})

			require.Equal(t, test.outcome, completion.GetOutcome())
			require.Nil(t, completion.GetResult())
			require.False(t, corerunnable.OutcomeIsCertain(completion.GetOutcome()))
			require.NotEmpty(t, completion.GetMessage())
		})
	}
}

func TestInvokeRecordsTheExitStatusOfACrash(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	root := installedHarness(t, "exit 7\n")
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second)})

	require.Equal(t, basev0.RunnableCompletion_CRASHED, completion.GetOutcome())
	require.Equal(t, int32(7), completion.GetExitCode())
	require.Empty(t, completion.GetSignal())
}

func TestInvokeEndsAnInvocationThatOutlivesItsDeadline(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	root := installedHarness(t, "/bin/sleep 30\n")
	started := time.Now()
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 300*time.Millisecond)})

	require.Equal(t, basev0.RunnableCompletion_TIMED_OUT, completion.GetOutcome())
	require.Less(t, time.Since(started), 30*time.Second, "the launcher waited for the process instead of its deadline")
	require.NotEmpty(t, completion.GetSignal(), "the launcher ended the process, so it died on a signal")
	require.False(t, corerunnable.OutcomeIsCertain(completion.GetOutcome()))
}

func TestInvokeKeepsAnOutcomeTheHarnessProvedBeforeTheDeadline(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	// The harness proves its outcome and then fails to exit. Ending the process
	// must not discard a completion it already reported.
	root := installedHarness(t, writeResult(resultOf(t, "inv-1", `{"count":3}`))+"/bin/sleep 30\n")
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 3*time.Second)})

	require.Equal(t, basev0.RunnableCompletion_SUCCEEDED, completion.GetOutcome())
	require.JSONEq(t, `{"count":3}`, string(completion.GetResult().GetOutput()))
}

func TestInvokeInterruptsOnlyAPackageThatDeclaresSignalCancellation(t *testing.T) {
	t.Run("signal cancellation is honoured", func(t *testing.T) {
		pkg := testPackage(t, []string{"harness"}, nil)
		root := installedHarness(t, "/bin/sleep 30\n")
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			time.Sleep(200 * time.Millisecond)
			cancel()
		}()
		completion := invoke(t, ctx, pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second)})

		require.Equal(t, basev0.RunnableCompletion_CANCELED, completion.GetOutcome())
		require.False(t, corerunnable.OutcomeIsCertain(completion.GetOutcome()))
	})

	t.Run("a package that cannot be interrupted runs to its deadline", func(t *testing.T) {
		pkg := testPackage(t, []string{"harness"}, func(execution *basev0.RunnableExecution) {
			execution.Cancellation = basev0.RunnableExecution_CANCELLATION_NONE
		})
		root := installedHarness(t, "/bin/sleep 0.4\n"+writeResult(resultOf(t, "inv-1", `{"count":3}`)))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		completion := invoke(t, ctx, pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second)})

		// The caller stopped waiting, but the invocation is still running and
		// reports what it actually did. Advertising a cancellation the harness
		// does not implement would report live work as abandoned.
		require.Equal(t, basev0.RunnableCompletion_SUCCEEDED, completion.GetOutcome())
	})
}

func TestInvokeBoundsEachLogStreamWithoutChangingTheOutcome(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, func(execution *basev0.RunnableExecution) {
		execution.MaxLogBytes = 8
	})
	root := installedHarness(t, "printf '%s' 'ooooooooooooooooooooo'\nprintf '%s' 'eeeeeeeeeeee' >&2\n"+
		writeResult(resultOf(t, "inv-1", `{"count":3}`)))
	var stdout, stderr bytes.Buffer
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{
		Invocation: invocationFor(pkg, "inv-1", 30*time.Second), Stdout: &stdout, Stderr: &stderr,
	})

	require.Equal(t, basev0.RunnableCompletion_SUCCEEDED, completion.GetOutcome(), "a truncated log must not change the outcome")
	require.Equal(t, "oooooooo", stdout.String())
	require.Equal(t, "eeeeeeee", stderr.String())
	require.Equal(t, uint64(8), completion.GetLogs().GetStdout().GetBytes())
	require.True(t, completion.GetLogs().GetStdout().GetTruncated())
	require.True(t, completion.GetLogs().GetStderr().GetTruncated())
}

func TestInvokeReportsLogVolumeWithoutASink(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, func(execution *basev0.RunnableExecution) {
		execution.MaxLogBytes = 64
	})
	root := installedHarness(t, "printf '%s' 'four'\n"+writeResult(resultOf(t, "inv-1", `{"count":3}`)))
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second)})

	require.Equal(t, uint64(4), completion.GetLogs().GetStdout().GetBytes())
	require.False(t, completion.GetLogs().GetStdout().GetTruncated())
}

func TestInvokePassesOnlyTheFramingAndTheResolvedEnvironment(t *testing.T) {
	t.Setenv("CODEFLY_TEST_AMBIENT_CREDENTIAL", "a credential the launcher happens to hold")
	pkg := testPackage(t, []string{"harness"}, nil)
	root := installedHarness(t, `printf '%s\n' "${CODEFLY_TEST_AMBIENT_CREDENTIAL-absent}" "${CODEFLY__STORE_ADDRESS-absent}" "$CODEFLY__RUNNABLE_PROTOCOL" > "$DUMP"
`+writeResult(resultOf(t, "inv-1", `{"count":3}`)))
	dump := filepath.Join(t.TempDir(), "environment")
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{
		Invocation:  invocationFor(pkg, "inv-1", 30*time.Second),
		Environment: map[string]string{"CODEFLY__STORE_ADDRESS": "localhost:5432", "DUMP": dump},
	})
	require.Equal(t, basev0.RunnableCompletion_SUCCEEDED, completion.GetOutcome())

	observed, err := os.ReadFile(dump)
	require.NoError(t, err)
	require.Equal(t, []string{"absent", "localhost:5432", corerunnable.ProtocolV1},
		strings.Split(strings.TrimSuffix(string(observed), "\n"), "\n"))
}

func TestInvokeRefusesAResolvedEnvironmentThatSetsTheFraming(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	launcher, err := runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), installedHarness(t, "exit 0\n"))
	require.NoError(t, err)

	_, err = launcher.Invoke(t.Context(), runnableops.Run{
		Invocation:  invocationFor(pkg, "inv-1", 30*time.Second),
		Directory:   filepath.Join(t.TempDir(), "invocation"),
		Environment: map[string]string{corerunnable.EnvResultPath: "/tmp/elsewhere"},
	})
	require.ErrorContains(t, err, "the launcher owns the framing")
}

func TestInvokeEndsTheWholeProcessTree(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	// A descendant that outlives the process the launcher started is still a
	// resource this invocation acquired.
	root := installedHarness(t, "/bin/sleep 30 &\nprintf '%s' \"$!\" > \"$CHILD\"\n/bin/sleep 30\n")
	child := filepath.Join(t.TempDir(), "child")
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{
		Invocation: invocationFor(pkg, "inv-1", 2*time.Second), Environment: map[string]string{"CHILD": child},
	})
	require.Equal(t, basev0.RunnableCompletion_TIMED_OUT, completion.GetOutcome())

	recorded, err := os.ReadFile(child)
	require.NoError(t, err)
	pid, err := strconv.Atoi(string(recorded))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return !base.IsProcessAlive(pid) }, 10*time.Second, 20*time.Millisecond,
		"descendant %d outlived the invocation that started it", pid)
}

func TestInvokeDoesNotDispatchAnInvocationWhoseDeadlineHasPassed(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	dispatched := filepath.Join(t.TempDir(), "dispatched")
	root := installedHarness(t, "printf '%s' ran > \""+dispatched+"\"\n")
	invocation := invocationFor(pkg, "inv-1", 30*time.Second)
	issued := time.Now().Add(-30 * time.Second)
	invocation.IssuedAt, invocation.Deadline = timestamppb.New(issued), timestamppb.New(issued.Add(time.Second))
	completion := invoke(t, t.Context(), pkg, root, runnableops.Run{Invocation: invocation})

	require.Equal(t, basev0.RunnableCompletion_TIMED_OUT, completion.GetOutcome())
	require.NoFileExists(t, dispatched, "an invocation with no time left must not spend an attempt")
}

func TestInvokeRefusesMaterialItCannotDispatch(t *testing.T) {
	pkg := testPackage(t, []string{"harness"}, nil)
	root := installedHarness(t, "exit 0\n")
	launcher, err := runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), root)
	require.NoError(t, err)

	t.Run("a budget longer than the declared timeout", func(t *testing.T) {
		_, err := launcher.Invoke(t.Context(), runnableops.Run{
			Invocation: invocationFor(pkg, "inv-1", 3*time.Hour), Directory: filepath.Join(t.TempDir(), "invocation"),
		})
		require.ErrorContains(t, err, "longer than the package's declared timeout")
	})

	t.Run("an invocation of another release", func(t *testing.T) {
		invocation := invocationFor(pkg, "inv-1", 30*time.Second)
		invocation.Runnable = &basev0.RunnableIdentity{Name: "word-count", Module: "operations", Workspace: "qualification", Version: "0.2.0"}
		_, err := launcher.Invoke(t.Context(), runnableops.Run{Invocation: invocation, Directory: filepath.Join(t.TempDir(), "invocation")})
		require.ErrorContains(t, err, "names another release")
	})

	t.Run("a directory that already holds an invocation", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "invocation")
		_, err := launcher.Invoke(t.Context(), runnableops.Run{Invocation: invocationFor(pkg, "inv-1", 30*time.Second), Directory: directory})
		require.NoError(t, err)
		_, err = launcher.Invoke(t.Context(), runnableops.Run{Invocation: invocationFor(pkg, "inv-2", 30*time.Second), Directory: directory})
		require.Error(t, err, "a second invocation must not read the first one's documents as its own")
	})
}

func TestNewNativeLauncherRefusesAnInstallationItCannotExecute(t *testing.T) {
	t.Run("an artifact built for another platform", func(t *testing.T) {
		pkg := testPackage(t, []string{"harness"}, nil)
		pkg.GetArtifacts()[0].Platform = "plan9/386"
		pkg.Digest = ""
		pkg, err := corerunnable.PreparePackage(pkg)
		require.NoError(t, err)
		_, err = runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), installedHarness(t, "exit 0\n"))
		require.ErrorContains(t, err, "built for plan9/386")
	})

	t.Run("a binding that does not install the package", func(t *testing.T) {
		pkg := testPackage(t, []string{"harness"}, nil)
		binding := testBinding(t, pkg)
		binding.CredentialReferences = []string{"tampered-after-installation"}
		_, err := runnableops.NewNativeLauncher(pkg, binding, installedHarness(t, "exit 0\n"))
		require.ErrorContains(t, err, "verify installed binding")
	})

	t.Run("a launch command outside the installed package", func(t *testing.T) {
		pkg := testPackage(t, []string{"../escape"}, nil)
		_, err := runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), installedHarness(t, "exit 0\n"))
		require.ErrorContains(t, err, "inside the installed package")
	})

	t.Run("a launch command that is not executable", func(t *testing.T) {
		pkg := testPackage(t, []string{"handler.py"}, nil)
		root := installedHarness(t, "exit 0\n")
		require.NoError(t, os.WriteFile(filepath.Join(root, "handler.py"), []byte("print()\n"), 0600))
		_, err := runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), root)
		require.ErrorContains(t, err, "is not an executable file")
	})

	t.Run("a relative installed root", func(t *testing.T) {
		pkg := testPackage(t, []string{"harness"}, nil)
		_, err := runnableops.NewNativeLauncher(pkg, testBinding(t, pkg), "relative/root")
		require.ErrorContains(t, err, "must be absolute")
	})
}
