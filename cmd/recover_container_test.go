package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/runners/dockerrun"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/require"
)

func containerStopReceipt(t *testing.T) (dockerrun.ContainerStopExpectation, []byte) {
	t.Helper()
	expected := dockerrun.ContainerStopExpectation{
		ID: strings.Repeat("a", 64), Created: "2026-10-28T00:38:43.921733968Z", Name: "codefly-fixture",
		Labels: map[string]string{dockerrun.LabelCodeflyOwner: "true", dockerrun.LabelCodeflyName: "codefly-fixture"},
		Mounts: []container.MountPoint{},
	}
	var encoded bytes.Buffer
	require.NoError(t, json.NewEncoder(&encoded).Encode(expected))
	return expected, encoded.Bytes()
}

func TestContainerRecoveryPreviewReceiptRoundTrip(t *testing.T) {
	for _, atLimit := range []bool{false, true} {
		expected, data := containerStopReceipt(t)
		if atLimit {
			expected.Labels["padding"] = ""
			initial, err := json.Marshal(expected)
			require.NoError(t, err)
			expected.Labels["padding"] = strings.Repeat("x", dockerrun.MaxContainerStopExpectationBytes-len(initial)-1)
		}
		var out bytes.Buffer
		preview := newRecoverContainerCommand(func(_ context.Context, id string) (*dockerrun.ContainerStopExpectation, error) {
			require.Equal(t, expected.ID, id)
			return &expected, nil
		}, func(context.Context, dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error) {
			t.Fatal("preview invoked effects")
			return nil, nil
		})
		preview.SetOut(&out)
		preview.SetArgs([]string{expected.ID, "--preview"})
		require.NoError(t, preview.Execute())
		if atLimit {
			require.Equal(t, dockerrun.MaxContainerStopExpectationBytes, out.Len())
		} else {
			require.Equal(t, string(data), out.String())
			require.Contains(t, out.String(), `"mounts":[]`)
		}
		parsed, err := dockerrun.ParseContainerStopExpectation(out.Bytes())
		require.NoError(t, err)
		require.Equal(t, expected, parsed)
		file := filepath.Join(t.TempDir(), "expected.json")
		require.NoError(t, os.WriteFile(file, out.Bytes(), 0o600))
		calls := 0
		command := newRecoverContainerCommand(nil, func(_ context.Context, actual dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error) {
			calls++
			require.Equal(t, expected, actual)
			return &dockerrun.ContainerStopResult{Stopped: true, Retained: true, MountsUnchanged: true}, nil
		})
		command.SetOut(&bytes.Buffer{})
		command.SetArgs([]string{expected.ID, "--expect", file})
		require.NoError(t, command.Execute())
		require.Equal(t, 1, calls)
	}
}

func TestContainerRecoveryRefusesBeforeEffects(t *testing.T) {
	expected, valid := containerStopReceipt(t)
	for _, tc := range []struct {
		name string
		args []string
		data string
	}{
		{"no ID", []string{"--preview"}, ""},
		{"extra ID", []string{expected.ID, expected.ID, "--preview"}, ""},
		{"no mode", []string{expected.ID}, ""},
		{"both modes", []string{expected.ID, "--preview", "--expect", "RECEIPT"}, string(valid)},
		{"empty path", []string{expected.ID, "--preview", "--expect="}, ""},
		{"missing file", []string{expected.ID, "--expect", "MISSING"}, ""},
		{"wrong target", []string{strings.Repeat("b", 64), "--expect", "RECEIPT"}, string(valid)},
		{"short target", []string{expected.ID[:12], "--expect", "RECEIPT"}, string(valid)},
		{"empty", nil, ""},
		{"malformed", nil, "{"},
		{"incomplete", nil, "{}"},
		{"null", nil, "null"},
		{"trailing", nil, string(valid) + " {}"},
		{"unknown", nil, strings.Replace(string(valid), "{", `{"unknown":true,`, 1)},
		{"duplicate", nil, strings.Replace(string(valid), "{", `{"name":"codefly-fixture",`, 1)},
		{"null mounts", nil, strings.Replace(string(valid), `"mounts":[]`, `"mounts":null`, 1)},
		{"oversize", nil, strings.Repeat(" ", dockerrun.MaxContainerStopExpectationBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "receipt.json")
			require.NoError(t, os.WriteFile(file, []byte(tc.data), 0o600))
			args := tc.args
			if args == nil {
				args = []string{expected.ID, "--expect", "RECEIPT"}
			}
			for i := range args {
				if args[i] == "RECEIPT" {
					args[i] = file
				} else if args[i] == "MISSING" {
					args[i] = filepath.Join(t.TempDir(), "missing")
				}
			}
			command := newRecoverContainerCommand(func(context.Context, string) (*dockerrun.ContainerStopExpectation, error) {
				t.Fatal("invalid CLI input reached preview")
				return nil, nil
			}, func(context.Context, dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error) {
				t.Fatal("invalid input reached stop")
				return nil, nil
			})
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(args)
			require.Error(t, command.Execute())
		})
	}
}

func TestContainerRecoveryReportsAllPostconditions(t *testing.T) {
	expected, data := containerStopReceipt(t)
	file := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(file, data, 0o600))
	ownerErr := errors.New("owner could not verify stop")
	for bits := 0; bits < 8; bits++ {
		for _, failed := range []bool{false, true} {
			result := &dockerrun.ContainerStopResult{Stopped: bits&1 != 0, Retained: bits&2 != 0, MountsUnchanged: bits&4 != 0}
			var out bytes.Buffer
			command := newRecoverContainerCommand(nil, func(_ context.Context, actual dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error) {
				require.Equal(t, expected, actual)
				if failed {
					return result, ownerErr
				}
				return result, nil
			})
			command.SetOut(&out)
			command.SetErr(&bytes.Buffer{})
			command.SilenceErrors, command.SilenceUsage = true, true
			command.SetArgs([]string{expected.ID, "--expect", file})
			err := command.Execute()
			require.Equal(t, bits == 7 && !failed, err == nil)
			if failed {
				require.ErrorIs(t, err, ownerErr)
			}
			var reported dockerrun.ContainerStopResult
			require.NoError(t, json.Unmarshal(out.Bytes(), &reported))
			require.Equal(t, *result, reported)
		}
	}
	command := newRecoverContainerCommand(nil, func(context.Context, dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error) {
		return nil, nil
	})
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{expected.ID, "--expect", file})
	require.ErrorContains(t, command.Execute(), "incomplete")
}

func TestContainerRecoveryPreviewRefusalsAndRegistration(t *testing.T) {
	expected, _ := containerStopReceipt(t)
	for _, result := range []*dockerrun.ContainerStopExpectation{nil, {ID: strings.Repeat("b", 64)}} {
		command := newRecoverContainerCommand(func(context.Context, string) (*dockerrun.ContainerStopExpectation, error) { return result, nil }, nil)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{expected.ID, "--preview"})
		require.ErrorContains(t, command.Execute(), "no matching")
	}
	for _, badID := range []string{"fixture-name", expected.ID[:12], strings.ToUpper(expected.ID)} {
		// The owner validates positional syntax before opening Docker.
		command := newRecoverContainerCommand(dockerrun.PreviewContainerStop, nil)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{badID, "--preview"})
		require.ErrorContains(t, command.Execute(), "64-hex")
	}
	command, _, err := RootCmd.Find([]string{"recover", "container"})
	require.NoError(t, err)
	require.Equal(t, "container", command.Name())
	var help bytes.Buffer
	previous := command.OutOrStdout()
	command.SetOut(&help)
	t.Cleanup(func() { command.SetOut(previous) })
	require.NoError(t, command.Help())
	require.Contains(t, help.String(), "AutoRemove")
	require.Contains(t, help.String(), "--expect")
}

type containerStopReportWriter struct{ err error }

func (w containerStopReportWriter) Write([]byte) (int, error) { return 0, w.err }

func TestContainerRecoveryPreservesOwnerAndReportingErrors(t *testing.T) {
	expected, data := containerStopReceipt(t)
	ownerErr, outputErr := errors.New("inspection refused"), errors.New("output failed")
	preview := newRecoverContainerCommand(func(context.Context, string) (*dockerrun.ContainerStopExpectation, error) {
		return nil, ownerErr
	}, nil)
	preview.SetOut(&bytes.Buffer{})
	preview.SetErr(&bytes.Buffer{})
	preview.SetArgs([]string{expected.ID, "--preview"})
	require.ErrorIs(t, preview.Execute(), ownerErr)
	file := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(file, data, 0o600))
	for _, failed := range []bool{false, true} {
		command := newRecoverContainerCommand(nil, func(context.Context, dockerrun.ContainerStopExpectation) (*dockerrun.ContainerStopResult, error) {
			result := &dockerrun.ContainerStopResult{Stopped: true, Retained: true, MountsUnchanged: true}
			if failed {
				return result, ownerErr
			}
			return result, nil
		})
		command.SetOut(containerStopReportWriter{err: outputErr})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{expected.ID, "--expect", file})
		err := command.Execute()
		require.ErrorIs(t, err, outputErr)
		if failed {
			require.ErrorIs(t, err, ownerErr)
		}
	}
}
