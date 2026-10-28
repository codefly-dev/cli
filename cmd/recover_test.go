package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/runners/base"
)

func recoveryReceipt(t *testing.T) (base.ProcessGroupRecoveryExpectation, []byte) {
	t.Helper()
	member := base.ProcessIdentity{PID: 12345, PGID: 12345, BootID: "test-boot", StartID: 101, Executable: "/fixture/service"}
	expected := base.ProcessGroupRecoveryExpectation{
		Namespace: "authenticated-v2", RecordSHA256: strings.Repeat("a", 64), PGID: member.PGID,
		Leader: member, Owner: base.ProcessIdentity{PID: 12344, BootID: "test-boot", StartID: 100, Executable: "/fixture/owner"},
		Members: []base.ProcessIdentity{member},
	}
	data, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	return expected, data
}

func TestRecoveryPreviewOnlyEmitsOwnerReceipt(t *testing.T) {
	expected, _ := recoveryReceipt(t)
	var out bytes.Buffer
	command := newRecoverCommand(func(_ context.Context, pgid int) (*base.ProcessGroupRecoveryExpectation, error) {
		if pgid != expected.PGID {
			t.Fatalf("preview selected %d", pgid)
		}
		return &expected, nil
	}, func(context.Context, base.ProcessGroupRecoveryExpectation) (*base.ProcessGroupRecoveryResult, error) {
		t.Fatal("preview invoked effects")
		return nil, nil
	})
	command.SetOut(&out)
	command.SetArgs([]string{"process-group", "12345", "--preview"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	actual, err := base.ParseProcessGroupRecoveryExpectation(out.Bytes())
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("receipt changed: %+v, %v", actual, err)
	}
}

func TestRecoveryRefusesBeforeOwnerEffects(t *testing.T) {
	_, valid := recoveryReceipt(t)
	for _, tc := range []struct {
		name string
		args []string
		data string
	}{
		{"no selection", nil, ""},
		{"extra selection", []string{"12345", "12346", "--preview"}, ""},
		{"invalid number", []string{"invalid", "--preview"}, ""},
		{"reserved number", []string{"1", "--preview"}, ""},
		{"no mode", []string{"12345"}, ""},
		{"both modes", []string{"12345", "--preview", "--expect", "RECEIPT"}, string(valid)},
		{"empty receipt path", []string{"12345", "--preview", "--expect="}, ""},
		{"missing file", []string{"12345", "--expect", "MISSING"}, ""},
		{"wrong target", []string{"12346", "--expect", "RECEIPT"}, string(valid)},
		{"empty", nil, ""},
		{"malformed", nil, "{"},
		{"incomplete", nil, "{}"},
		{"null", nil, "null"},
		{"trailing", nil, string(valid) + " {}"},
		{"unknown field", nil, strings.Replace(string(valid), "{", `{"unknown":true,`, 1)},
		{"duplicate field", nil, strings.Replace(string(valid), "{", `{"pgid":12345,`, 1)},
		{"oversized", nil, strings.Repeat(" ", 1024*1024+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "receipt.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			args := tc.args
			if args == nil && tc.name != "no selection" {
				args = []string{"12345", "--expect", "RECEIPT"}
			}
			for i := range args {
				if args[i] == "RECEIPT" {
					args[i] = path
				} else if args[i] == "MISSING" {
					args[i] = filepath.Join(t.TempDir(), "missing")
				}
			}
			command := newRecoverCommand(func(context.Context, int) (*base.ProcessGroupRecoveryExpectation, error) {
				t.Fatal("invalid selection reached preview")
				return nil, nil
			}, func(context.Context, base.ProcessGroupRecoveryExpectation) (*base.ProcessGroupRecoveryResult, error) {
				t.Fatal("invalid selection reached recovery")
				return nil, nil
			})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(append([]string{"process-group"}, args...))
			if err := command.Execute(); err == nil {
				t.Fatal("invalid selection succeeded")
			}
		})
	}
}

func TestRecoveryReportsOwnerPostconditionsAndErrors(t *testing.T) {
	expected, data := recoveryReceipt(t)
	path := filepath.Join(t.TempDir(), "receipt.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ownerErr := errors.New("owner refused or could not complete recovery")
	for _, tc := range []struct {
		name        string
		result      *base.ProcessGroupRecoveryResult
		err         error
		wantSuccess bool
	}{
		{"complete", &base.ProcessGroupRecoveryResult{GroupDead: true, RecordRemoved: true}, nil, true},
		{"dead record retained", &base.ProcessGroupRecoveryResult{GroupDead: true}, nil, false},
		{"live record removed", &base.ProcessGroupRecoveryResult{RecordRemoved: true}, nil, false},
		{"no proof", &base.ProcessGroupRecoveryResult{}, nil, false},
		{"nil result", nil, nil, false},
		{"refused", &base.ProcessGroupRecoveryResult{}, ownerErr, false},
		{"partial failure", &base.ProcessGroupRecoveryResult{GroupDead: true}, ownerErr, false},
		{"postcondition error", &base.ProcessGroupRecoveryResult{GroupDead: true, RecordRemoved: true}, ownerErr, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			command := newRecoverCommand(func(context.Context, int) (*base.ProcessGroupRecoveryExpectation, error) {
				t.Fatal("execution must not replace the reviewed receipt with a fresh preview")
				return nil, nil
			}, func(_ context.Context, actual base.ProcessGroupRecoveryExpectation) (*base.ProcessGroupRecoveryResult, error) {
				calls++
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("owner received changed evidence: %+v", actual)
				}
				return tc.result, tc.err
			})
			command.SetOut(&out)
			command.SetErr(&bytes.Buffer{})
			command.SilenceErrors, command.SilenceUsage = true, true
			command.SetArgs([]string{"process-group", "12345", "--expect", path})
			err := command.Execute()
			if calls != 1 || (err == nil) != tc.wantSuccess || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if tc.result != nil {
				var actual base.ProcessGroupRecoveryResult
				if err := json.Unmarshal(out.Bytes(), &actual); err != nil || actual != *tc.result {
					t.Fatalf("lost partial result: %q, %v", out.String(), err)
				}
			}
		})
	}
}

func TestRecoveryCommandRegisteredAndHelp(t *testing.T) {
	command, _, err := RootCmd.Find([]string{"recover", "process-group"})
	if err != nil || command.Name() != "process-group" {
		t.Fatalf("command missing: %v", err)
	}
	var out bytes.Buffer
	command.SetOut(&out)
	t.Cleanup(func() { command.SetOut(nil) })
	if err := command.Help(); err != nil || !strings.Contains(out.String(), "--expect") {
		t.Fatalf("help missing receipt workflow: %v, %s", err, out.String())
	}
}
