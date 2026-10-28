package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codefly-dev/core/runners/base"
	"github.com/stretchr/testify/require"
)

func processExitTestIdentity() base.ProcessIdentity {
	return base.ProcessIdentity{PID: 1234567, PGID: 1234567, BootID: "boot", StartID: 42, Executable: "fixture"}
}

func processExitTestReceipt(t *testing.T, identity base.ProcessIdentity) (string, []byte) {
	t.Helper()
	raw, err := encodeProcessExitReceipt(processExitReceipt{1, identity})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path, raw
}

func TestProcessExitReceiptNativeShape(t *testing.T) {
	// An upstream owner field addition/type/tag change requires explicit v1
	// contract review, not silent propagation through the embedded native type.
	typeOf := reflect.TypeFor[base.ProcessIdentity]()
	fields := []struct{ name, kind string }{{"PID", "int"}, {"PGID", "int"}, {"BootID", "string"}, {"StartID", "uint64"}, {"Executable", "string"}}
	require.Equal(t, len(fields), typeOf.NumField())
	for i, field := range fields {
		require.Equal(t, field.name, typeOf.Field(i).Name)
		require.Equal(t, field.kind, typeOf.Field(i).Type.String())
		require.Empty(t, typeOf.Field(i).Tag)
	}
	_, raw := processExitTestReceipt(t, processExitTestIdentity())
	require.Equal(t, "{\"schema_version\":1,\"process\":{\"PID\":1234567,\"PGID\":1234567,\"BootID\":\"boot\",\"StartID\":42,\"Executable\":\"fixture\"}}\n", string(raw))
	result, err := json.Marshal(processExitResult{SchemaVersion: 1, PID: 1234567, Observation: "unknown"})
	require.NoError(t, err)
	require.JSONEq(t, `{"schema_version":1,"pid":1234567,"signal_attempted":false,"signal_accepted":false,"observation":"unknown","expected_birth_gone":false}`, string(result))
}

func TestProcessExitReceiptRejectsAlteredEvidence(t *testing.T) {
	_, raw := processExitTestReceipt(t, processExitTestIdentity())
	valid := string(raw)
	cases := map[string]string{
		"null": "null\n", "missing process": "{\"schema_version\":1}\n", "null process": "{\"schema_version\":1,\"process\":null}\n",
		"unknown":                strings.Replace(valid, `"process":`, `"extra":1,"process":`, 1),
		"duplicate":              strings.Replace(valid, `"PID":1234567,`, `"PID":9,"PID":1234567,`, 1),
		"alias":                  strings.Replace(valid, `"PID"`, `"pid"`, 1),
		"escaped duplicate":      strings.Replace(valid, `"PID":1234567,`, `"\u0050ID":9,"PID":1234567,`, 1),
		"null identity field":    strings.Replace(valid, `"PGID":1234567`, `"PGID":null`, 1),
		"missing identity field": strings.Replace(valid, `"Executable":"fixture"`, `"Executable":""`, 1),
		"unknown version":        strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1),
		"trailing":               valid + "{}", "whitespace": " " + valid, "missing newline": strings.TrimSuffix(valid, "\n"),
		"oversize":      strings.Repeat(" ", processExitReceiptLimit+1),
		"missing birth": strings.Replace(valid, `"StartID":42`, `"StartID":0`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt.json")
			require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
			_, err := readProcessExitReceipt(t.Context(), path)
			require.Error(t, err)
		})
	}
}

func TestProcessExitPreviewReceiptBoundary(t *testing.T) {
	identity := processExitTestIdentity()
	_, raw := processExitTestReceipt(t, identity)
	identity.Executable += strings.Repeat("x", processExitReceiptLimit-len(raw))
	var output bytes.Buffer
	command := newRecoverProcessCommand(func(int) (base.ProcessIdentity, error) { return identity, nil }, func(context.Context, base.ProcessIdentity, syscall.Signal) error {
		t.Fatal("preview signaled")
		return nil
	})
	command.SetOut(&output)
	command.SetArgs([]string{strconv.Itoa(identity.PID), "--preview"})
	require.NoError(t, command.Execute())
	require.Len(t, output.Bytes(), processExitReceiptLimit)
	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, output.Bytes(), 0o600))
	receipt, err := readProcessExitReceipt(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, identity, receipt.Process)
	identity.Executable += "x"
	_, err = encodeProcessExitReceipt(processExitReceipt{1, identity})
	require.Error(t, err)
}

func TestProcessExitCommandRefusesBeforeEffects(t *testing.T) {
	identity := processExitTestIdentity()
	path, _ := processExitTestReceipt(t, identity)
	pid := strconv.Itoa(identity.PID)
	cases := [][]string{
		nil, {"0", "--preview"}, {"1", "--preview"}, {"-2", "--preview"}, {"+2", "--preview"}, {"02", "--preview"},
		{"9999999999999999999999999", "--preview"}, {strconv.Itoa(os.Getpid()), "--preview"}, {pid},
		{pid, "--expect", path}, {pid, "--force"}, {pid, "--preview", "--force"}, {pid, "--preview", "--expect", path},
		{pid, "--expect=", "--force"}, {pid, "--expect", path + "-missing", "--force"},
		{"1234568", "--expect", path, "--force"}, {pid, "another", "--preview"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			command := newRecoverProcessCommand(func(int) (base.ProcessIdentity, error) {
				t.Fatal("invalid selection reached inspection")
				return identity, nil
			}, func(context.Context, base.ProcessIdentity, syscall.Signal) error {
				t.Fatal("invalid selection signaled")
				return nil
			})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(args)
			require.Error(t, command.Execute())
		})
	}
}

func TestProcessExitOwnerOutcomes(t *testing.T) {
	expected := processExitTestIdentity()
	changedBirth, changedGroup, changedExecutable, changedBoot := expected, expected, expected, expected
	changedBirth.StartID++
	changedBoot.BootID = "other-boot"
	changedGroup.PGID++
	changedExecutable.Executable = "execed"
	inspectionErr, deliveryErr := errors.New("owner inspection incomplete"), errors.New("delivery uncertain")
	type observation struct {
		identity base.ProcessIdentity
		err      error
	}
	for _, tc := range []struct {
		name       string
		reads      []observation
		delivery   error
		cancelSend bool
		want       processExitResult
		wantErr    bool
	}{
		{"absent initially", []observation{{err: base.ErrProcessNotFound}}, nil, false, processExitResult{Observation: "absent", ExpectedBirthGone: true}, false},
		{"ordinary admission error", []observation{{err: inspectionErr}}, nil, false, processExitResult{Observation: "unknown"}, true},
		{"live replacement", []observation{{identity: changedBirth}}, nil, false, processExitResult{Observation: "changed_birth", ExpectedBirthGone: true}, true},
		{"changed boot", []observation{{identity: changedBoot}}, nil, false, processExitResult{Observation: "changed_birth", ExpectedBirthGone: true}, true},
		{"incomplete owner observation", []observation{{}}, nil, false, processExitResult{Observation: "unknown"}, true},
		{"changed PGID admission", []observation{{identity: changedGroup}}, nil, false, processExitResult{Observation: "same_birth"}, true},
		{"changed executable admission", []observation{{identity: changedExecutable}}, nil, false, processExitResult{Observation: "same_birth"}, true},
		{"exit observed", []observation{{identity: expected}, {err: base.ErrProcessNotFound}}, nil, false, processExitResult{SignalAttempted: true, SignalAccepted: true, Observation: "absent", ExpectedBirthGone: true}, false},
		{"ordinary readback error", []observation{{identity: expected}, {err: inspectionErr}}, nil, false, processExitResult{SignalAttempted: true, SignalAccepted: true, Observation: "unknown"}, true},
		{"error despite absence", []observation{{identity: expected}, {err: base.ErrProcessNotFound}}, deliveryErr, false, processExitResult{SignalAttempted: true, Observation: "absent", ExpectedBirthGone: true}, true},
		{"owner notfound is not absence", []observation{{identity: expected}, {identity: changedBirth}}, base.ErrProcessNotFound, false, processExitResult{SignalAttempted: true, Observation: "changed_birth", ExpectedBirthGone: true}, true},
		{"replacement after accepted send", []observation{{identity: expected}, {identity: changedBirth}}, nil, false, processExitResult{SignalAttempted: true, SignalAccepted: true, Observation: "changed_birth", ExpectedBirthGone: true}, true},
		{"cancel during accepted owner call", []observation{{identity: expected}}, nil, true, processExitResult{SignalAttempted: true, SignalAccepted: true, Observation: "unknown"}, true},
		{"cancel during uncertain owner call", []observation{{identity: expected}}, deliveryErr, true, processExitResult{SignalAttempted: true, Observation: "unknown"}, true},
		{"same birth exec is not exit", []observation{{identity: expected}, {identity: changedExecutable}, {identity: changedGroup}, {err: base.ErrProcessNotFound}}, nil, false, processExitResult{SignalAttempted: true, SignalAccepted: true, Observation: "absent", ExpectedBirthGone: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reads, calls := 0, 0
			result, err := forceProcessExit(ctx, expected, func(pid int) (base.ProcessIdentity, error) {
				require.Equal(t, expected.PID, pid)
				require.Less(t, reads, len(tc.reads))
				r := tc.reads[reads]
				reads++
				return r.identity, r.err
			}, func(_ context.Context, identity base.ProcessIdentity, sig syscall.Signal) error {
				calls++
				require.Equal(t, expected, identity)
				require.Positive(t, identity.PID)
				require.Equal(t, syscall.SIGKILL, sig)
				if tc.cancelSend {
					cancel()
				}
				return tc.delivery
			})
			require.Equal(t, tc.wantErr, err != nil, "%v", err)
			if tc.delivery != nil {
				require.ErrorIs(t, err, tc.delivery)
			}
			tc.want.SchemaVersion, tc.want.PID = 1, expected.PID
			require.Equal(t, tc.want, result)
			require.Equal(t, len(tc.reads), reads)
			if result.SignalAttempted {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
		})
	}
}

func TestProcessExitCancellationAndPollingBudget(t *testing.T) {
	expected := processExitTestIdentity()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := forceProcessExit(ctx, expected, func(int) (base.ProcessIdentity, error) { t.Fatal("canceled inspection"); return expected, nil }, func(context.Context, base.ProcessIdentity, syscall.Signal) error {
		t.Fatal("canceled signal")
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, result.SignalAttempted)
	reads := 0
	result, err = forceProcessExit(t.Context(), expected, func(int) (base.ProcessIdentity, error) { reads++; return expected, nil }, func(context.Context, base.ProcessIdentity, syscall.Signal) error { return nil })
	require.ErrorContains(t, err, "polling budget")
	require.Greater(t, reads, 1)
	require.Equal(t, "same_birth", result.Observation)
	require.False(t, result.ExpectedBirthGone)
	require.True(t, result.SignalAccepted)
}

type processExitFailWriter struct{ err error }

func (writer processExitFailWriter) Write([]byte) (int, error) { return 0, writer.err }

func TestProcessExitCommandReportingAndWiring(t *testing.T) {
	expected := processExitTestIdentity()
	path, _ := processExitTestReceipt(t, expected)
	for _, failOutput := range []bool{false, true} {
		reads := 0
		var output bytes.Buffer
		deliveryErr, outputErr := errors.New("delivery uncertainty"), errors.New("output lost")
		command := newRecoverProcessCommand(func(int) (base.ProcessIdentity, error) {
			reads++
			if reads == 1 {
				return expected, nil
			}
			return base.ProcessIdentity{}, base.ErrProcessNotFound
		}, func(context.Context, base.ProcessIdentity, syscall.Signal) error { return deliveryErr })
		command.SetOut(&output)
		if failOutput {
			command.SetOut(processExitFailWriter{outputErr})
		}
		command.SetErr(&bytes.Buffer{})
		command.SilenceErrors, command.SilenceUsage = true, true
		command.SetArgs([]string{strconv.Itoa(expected.PID), "--expect", path, "--force"})
		err := command.Execute()
		require.ErrorIs(t, err, deliveryErr)
		if failOutput {
			require.ErrorIs(t, err, outputErr)
		} else {
			var result processExitResult
			require.NoError(t, json.Unmarshal(output.Bytes(), &result))
			require.True(t, result.SignalAttempted)
			require.Equal(t, "absent", result.Observation)
		}
	}
	command, _, err := RootCmd.Find([]string{"recover", "process"})
	require.NoError(t, err)
	require.Equal(t, "process", command.Name())
	var help bytes.Buffer
	command.SetOut(&help)
	t.Cleanup(func() { command.SetOut(nil) })
	require.NoError(t, command.Help())
	for _, phrase := range []string{"SIGKILL", "irreversible", "same-birth exec", "Cancellation", "--force"} {
		require.Contains(t, help.String(), phrase)
	}
}

func TestProcessExitReceiptNonRegularRefuses(t *testing.T) {
	for _, path := range []string{t.TempDir(), os.DevNull} {
		_, err := readProcessExitReceipt(t.Context(), path)
		require.Error(t, err)
	}
	// Nonblocking open must not wait for a FIFO writer before type validation.
	path := filepath.Join(t.TempDir(), "fifo")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	done := make(chan error, 1)
	go func() { _, err := readProcessExitReceipt(context.Background(), path); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatalf("receipt open waited for FIFO peer: %s", path)
	}
}
