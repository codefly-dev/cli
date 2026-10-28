package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/runners/base"
)

// This exercises receipt transport, not authentication of a large live group.
// Core owns producer admission; the CLI must preserve its accepted byte range.
func TestRecoveryPreviewReceiptByteBoundary(t *testing.T) {
	for name, size := range map[string]int{
		"above registry storage limit": 20 * 1024,
		"inclusive receipt limit":      base.MaxProcessGroupRecoveryExpectationBytes,
		"one byte above limit":         base.MaxProcessGroupRecoveryExpectationBytes + 1,
	} {
		t.Run(name, func(t *testing.T) {
			expected, raw := recoveryReceipt(t)
			member := expected.Leader
			member.PID = 100000
			memberJSON, err := json.Marshal(member)
			if err != nil {
				t.Fatal(err)
			}
			// Each extra member adds one comma plus its JSON; keep PID widths
			// constant, then pad one executable by less than a member's size.
			count := (size - len(raw) - 1) / (len(memberJSON) + 1)
			for i := 0; i < count; i++ {
				member.PID = 100000 + i
				expected.Members = append(expected.Members, member)
			}
			raw, err = json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			expected.Members[len(expected.Members)-1].Executable += strings.Repeat("x", size-len(raw)-1)
			var out bytes.Buffer
			calls := 0
			command := newRecoverCommand(func(context.Context, int) (*base.ProcessGroupRecoveryExpectation, error) {
				return &expected, nil
			}, func(_ context.Context, got base.ProcessGroupRecoveryExpectation) (*base.ProcessGroupRecoveryResult, error) {
				calls++
				if !reflect.DeepEqual(got, expected) {
					t.Fatal("receipt transport changed the owner expectation")
				}
				return &base.ProcessGroupRecoveryResult{GroupDead: true, RecordRemoved: true}, nil
			})
			command.SetOut(&out)
			command.SetErr(&bytes.Buffer{})
			command.SilenceErrors, command.SilenceUsage = true, true
			command.SetArgs([]string{"process-group", "12345", "--preview"})
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			if out.Len() != size || out.Bytes()[size-1] != '\n' || calls != 0 {
				t.Fatalf("preview bytes=%d, want %d including newline; effects=%d", out.Len(), size, calls)
			}
			oversized := size > base.MaxProcessGroupRecoveryExpectationBytes
			parsed, err := base.ParseProcessGroupRecoveryExpectation(out.Bytes())
			if oversized && err == nil {
				t.Fatal("Core parser accepted oversized receipt")
			}
			if !oversized && (err != nil || !reflect.DeepEqual(parsed, expected)) {
				t.Fatalf("actual preview output does not round-trip through Core: %v", err)
			}
			path := filepath.Join(t.TempDir(), "receipt.json")
			if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			command.SetArgs([]string{"process-group", "12345", "--preview=false", "--expect", path})
			if oversized {
				// Injected oversized preview tests the consumer's bound; Core's
				// real producer refuses this expectation before returning it.
				if err := command.Execute(); err == nil || calls != 0 {
					t.Fatalf("oversized receipt reached effects: %v, calls=%d", err, calls)
				}
				return
			}
			if err := command.Execute(); err != nil || calls != 1 {
				t.Fatalf("accepted preview cannot be consumed: %v, calls=%d", err, calls)
			}
			t.Logf("%d-byte command preview, including newline, consumed unchanged", size)
		})
	}
}
