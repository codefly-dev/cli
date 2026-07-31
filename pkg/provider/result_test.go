package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStatusExitCodesAreStable(t *testing.T) {
	want := map[Status]int{
		StatusSuccess:          0,
		StatusInvalid:          1,
		StatusDiff:             2,
		StatusPolicyDenied:     3,
		StatusApprovalRequired: 4,
		StatusPartial:          5,
		StatusUncertain:        6,
		StatusStale:            7,
	}
	for status, code := range want {
		require.Equalf(t, code, status.ExitCode(), "exit code for %s must never change", status)
	}
	require.Equal(t, 1, Status("anything-unknown").ExitCode(), "unknown status maps to the unclassified failure code")
}

func TestResultFailSetsInvalidAndKeepsStrongerStatus(t *testing.T) {
	result := NewResult("plan", "local")
	result.Status = StatusStale
	result.Fail(CodeBindingNotFound, "gone")
	require.Equal(t, StatusStale, result.Status, "Fail must not downgrade a stronger status")

	result = NewResult("plan", "local")
	result.Fail(CodeBindingNotFound, "gone")
	require.Equal(t, StatusInvalid, result.Status)
}

func TestCommandErrorCarriesExitCodeAndMachineFlag(t *testing.T) {
	err := &commandError{status: StatusPolicyDenied, emitted: true}
	require.Equal(t, 3, err.ExitCode())
	require.True(t, err.MachineReadable())

	human := &commandError{status: StatusInvalid}
	require.Equal(t, 1, human.ExitCode())
	require.False(t, human.MachineReadable())
}

// A binding whose input holds a raw-looking secret must never surface that
// value anywhere in the JSON envelope — only its key.
func TestResultJSONNeverLeaksInputValues(t *testing.T) {
	secret := "client_secret=totally-fake-test-value"
	binding := &Binding{
		Name:     "billing",
		Provider: "acme/stripe:1.2.3",
		Mode:     ModeManaged,
		Input:    map[string]string{"token": secret},
	}
	result := NewResult("list", "local")
	result.Bindings = append(result.Bindings, binding.View(true))

	payload, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(payload), secret, "input values must never appear in the JSON envelope")
	require.Contains(t, string(payload), "token", "input keys are safe to surface")
}

func TestResultJSONIsDeterministic(t *testing.T) {
	build := func() []byte {
		result := NewResult("list", "local")
		result.Diagnostics = append(result.Diagnostics,
			Diagnostic{Code: CodeInvalidMode, Binding: "b", Message: "x"})
		payload, err := json.MarshalIndent(result, "", "  ")
		require.NoError(t, err)
		return payload
	}
	require.Equal(t, string(build()), string(build()))
	require.True(t, strings.Contains(string(build()), `"schema_version": 1`))
}
