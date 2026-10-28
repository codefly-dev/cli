package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/codefly-dev/core/runners/base"
	"github.com/spf13/cobra"
)

const (
	processExitReceiptLimit   = 16 << 10
	processObservationUnknown = "unknown"
	processObservationAbsent  = "absent"
	processExitModeGraceful   = "graceful"
	processExitModeForce      = "force"
)

// Version 1 is a fixed local native-identity exception, not a registry format.
// Its exact owner type shape is frozen by TestProcessExitReceiptNativeShape.
type processExitReceipt struct {
	SchemaVersion int                  `json:"schema_version"`
	Process       base.ProcessIdentity `json:"process"`
}

// processExitResult is version 2: version 1 named no mode, because SIGKILL was
// the only one. The --preview receipt stays version 1 — its shape did not
// change, and bumping it would invalidate receipts already saved for review.
type processExitResult struct {
	SchemaVersion int `json:"schema_version"`
	PID           int `json:"pid"`
	// Mode and Signal name what was REQUESTED, so a reader of the result never
	// has to infer which of the two irreversibility contracts applied.
	Mode              string `json:"mode"`
	Signal            string `json:"signal"`
	SignalAttempted   bool   `json:"signal_attempted"`
	SignalAccepted    bool   `json:"signal_accepted"`
	Observation       string `json:"observation"`
	ExpectedBirthGone bool   `json:"expected_birth_gone"`
}

func init() {
	RecoverCmd.AddCommand(newRecoverProcessCommand(base.InspectProcess, base.SignalProcess))
}

func newRecoverProcessCommand(inspect func(int) (base.ProcessIdentity, error), signal func(context.Context, base.ProcessIdentity, syscall.Signal) error) *cobra.Command {
	var preview, graceful, force bool
	var path string
	command := &cobra.Command{
		Use: "process PID", Short: "Preview or explicitly force one independently selected process to exit",
		Long: `Select one process by PID, independently of workspace or group ownership.
Save --preview verbatim, review its full identity, then use --expect FILE with
exactly one of --graceful or --force. Execution compares the full current
identity and requests ONE signal through Core for that process birth only:
--graceful requests SIGTERM, --force requests SIGKILL. SIGKILL is irreversible
and discards whatever the process had not yet written; --graceful lets the
process run its own shutdown, which is what a store holding authoritative state
needs. Neither escalates: a --graceful request is never followed by SIGKILL, and
an unconfirmed exit stays unconfirmed rather than falling back to force. No
escalation, group operation, registry removal or orphan recovery is performed.
Executable equality is checked at admission only: a same-birth exec can still
receive the signal. Children may react to lost connections or parent death;
this command does not prove that their cleanup or stored data remain untouched.
Success requires observed absence without errors; delivery alone is not exit.
Cancellation cannot revoke in-flight delivery. The five-second polling budget
does not interrupt synchronous inspection or output. An error may follow an
effect: inspect the disposition before any retry. Use the same reviewed binary
on the same host/boot; changing implementation requires a fresh preview.`,
		Example: "  codefly recover process 12345 --preview > process-exit.json\n  codefly recover process 12345 --expect process-exit.json --graceful\n  codefly recover process 12345 --expect process-exit.json --force",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, parseErr := strconv.Atoi(args[0])
			if parseErr != nil || pid <= 1 || pid == os.Getpid() || strconv.Itoa(pid) != args[0] {
				return errors.New("PID must be a canonical decimal integer greater than 1, excluding this CLI")
			}
			// Exactly one mode, and the receipt belongs to the two that signal.
			modes := 0
			for _, selected := range []bool{preview, graceful, force} {
				if selected {
					modes++
				}
			}
			if modes != 1 || preview == (path != "") || (cmd.Flags().Changed("expect") && path == "") {
				return errors.New("select --preview alone, or --expect FILE with exactly one of --graceful or --force")
			}
			ctx, done := common.NewContext()
			defer done()
			ctx, stop := common.SignalContext(ctx)
			defer stop()
			unregister := context.AfterFunc(cmd.Context(), stop)
			defer unregister()
			if err := cmd.Context().Err(); err != nil {
				return err
			}
			if preview {
				identity, err := inspect(pid)
				if err != nil {
					return err
				}
				if identity.PID != pid {
					return errors.New("core returned a different process identity")
				}
				raw, err := encodeProcessExitReceipt(processExitReceipt{1, identity})
				if err = errors.Join(err, ctx.Err()); err != nil {
					return err
				}
				_, err = cmd.OutOrStdout().Write(raw)
				return err
			}
			expected, err := readProcessExitReceipt(ctx, path)
			if err != nil {
				return err
			}
			if expected.Process.PID != pid {
				return errors.New("receipt PID does not match the explicitly selected PID")
			}
			requested, mode := syscall.SIGKILL, processExitModeForce
			if graceful {
				requested, mode = syscall.SIGTERM, processExitModeGraceful
			}
			result, effectErr := requestProcessExit(ctx, expected.Process, requested, mode, inspect, signal)
			return errors.Join(effectErr, json.NewEncoder(cmd.OutOrStdout()).Encode(result))
		},
	}
	command.Flags().BoolVar(&preview, "preview", false, "Print the current process identity without signaling")
	command.Flags().StringVar(&path, "expect", "", "Use the complete verbatim version-1 preview receipt")
	command.Flags().BoolVar(&graceful, "graceful", false, "Request one SIGTERM for the selected process birth, letting it shut down itself; never escalates to SIGKILL")
	command.Flags().BoolVar(&force, "force", false, "Explicitly request irreversible SIGKILL for the selected process birth")
	return command
}

func encodeProcessExitReceipt(receipt processExitReceipt) ([]byte, error) {
	p := receipt.Process
	if receipt.SchemaVersion != 1 || p.PID <= 1 || p.PID == os.Getpid() || p.PGID <= 0 || p.BootID == "" || p.StartID == 0 || p.Executable == "" {
		return nil, errors.New("version-1 receipt requires a complete non-self process identity")
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > processExitReceiptLimit {
		return nil, fmt.Errorf("process receipt exceeds %d bytes including newline", processExitReceiptLimit)
	}
	return raw, nil
}

func readProcessExitReceipt(ctx context.Context, path string) (processExitReceipt, error) {
	var receipt processExitReceipt
	file, err := composition.OpenInputFile(ctx, path)
	if err != nil {
		return receipt, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, processExitReceiptLimit+1))
	if err != nil {
		return receipt, err
	}
	if len(raw) > processExitReceiptLimit {
		return receipt, errors.New("process receipt exceeds the 16 KiB limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	canonical, err := encodeProcessExitReceipt(receipt)
	if err != nil {
		return receipt, err
	}
	if !bytes.Equal(raw, canonical) {
		return receipt, errors.New("process receipt must match the complete canonical preview bytes")
	}
	return receipt, ctx.Err()
}

func sameProcessBirth(first, second base.ProcessIdentity) bool {
	return first.PID == second.PID && first.BootID == second.BootID && first.StartID == second.StartID
}

func requestProcessExit(ctx context.Context, expected base.ProcessIdentity, requested syscall.Signal, mode string, inspect func(int) (base.ProcessIdentity, error), signal func(context.Context, base.ProcessIdentity, syscall.Signal) error) (processExitResult, error) {
	result := processExitResult{SchemaVersion: 2, PID: expected.PID, Mode: mode, Signal: requested.String(), Observation: processObservationUnknown}
	observe := func() (base.ProcessIdentity, error) {
		result.Observation, result.ExpectedBirthGone = processObservationUnknown, false
		if err := ctx.Err(); err != nil {
			return base.ProcessIdentity{}, err
		}
		current, err := inspect(expected.PID)
		switch {
		case errors.Is(err, base.ErrProcessNotFound):
			result.Observation, result.ExpectedBirthGone = processObservationAbsent, true
			err = nil
		case err != nil:
		case current.PID != expected.PID || current.PGID <= 0 || current.BootID == "" || current.StartID == 0 || current.Executable == "":
			err = errors.New("core returned an incomplete or mismatched process observation")
		case !sameProcessBirth(current, expected):
			result.Observation, result.ExpectedBirthGone = "changed_birth", true
			err = errors.New("PID now names a different birth; replacement was not selected")
		default:
			result.Observation = "same_birth"
		}
		return current, errors.Join(err, ctx.Err())
	}
	current, err := observe()
	if err != nil || result.Observation == processObservationAbsent {
		return result, err
	}
	if current != expected {
		return result, errors.New("process execution state changed since preview")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.SignalAttempted = true
	// Exactly one request, whichever mode: nothing here sends a second signal.
	deliveryErr := signal(ctx, expected, requested)
	result.SignalAccepted = deliveryErr == nil
	// This bounds polling, not synchronous owner inspection or in-flight delivery.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := observe()
		if err != nil || result.Observation == processObservationAbsent {
			return result, errors.Join(deliveryErr, err)
		}
		select {
		case <-ctx.Done():
			return result, errors.Join(deliveryErr, ctx.Err())
		case <-deadline.C:
			return result, errors.Join(deliveryErr, errors.New("process exit unconfirmed within polling budget"))
		case <-ticker.C:
		}
	}
}
