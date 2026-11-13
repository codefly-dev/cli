package gitops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
)

// A render record says how the last render of a module for an environment
// ended, and what composition it rendered, so publish delivers a working
// render only while a render still vouches for it.
//
// A failed render leaves the previous tree in place — the new tree is staged
// and replaces the old one only on success — and nothing in that tree says a
// later render failed. On 2026-11-12 `codefly deploy gitops render saas --env
// blue` failed on a dependency image, and the `publish` that followed opened
// and merged a promotion of the stale tree. The record is written before a
// render starts (rendering), and again when it ends (rendered or failed), so a
// render that failed, or was interrupted, leaves a record publish refuses.

const (
	renderRecordRendering = "rendering"
	renderRecordRendered  = "rendered"
	renderRecordFailed    = "failed"

	// renderRecordErrorLimit bounds the failure a record keeps: enough to say
	// why, never an agent's whole log.
	renderRecordErrorLimit = 600
)

type renderRecord struct {
	Module      string    `json:"module"`
	Environment string    `json:"environment"`
	Outcome     string    `json:"outcome"`
	At          time.Time `json:"at"`
	Error       string    `json:"error,omitempty"`
	// Digest is the inventory digest a successful render installed.
	Digest string `json:"digest,omitempty"`
	// Pins is the composition the render read: every pinned module of the
	// workspace, by name, as source@version. A render resolves its
	// dependencies' images and contracts from these, so it is stale once any
	// of them moves.
	Pins map[string]string `json:"pins,omitempty"`
	// PinCurrency is how the readiness gate this render passed judged Pins
	// against their sources, at render time. Absent when no gate judged them
	// (a CLI that recorded none, or a render path with no gate).
	PinCurrency *PinCurrency `json:"pinCurrency,omitempty"`
}

// PinCurrency is a render's judgement of the composition's pins against their
// sources, made by the readiness gate the render passed. It is what a publish
// of that render is held to.
//
// A render ships what its pins name, and refuses a pin behind its source
// unless a hold states why (cli#1339). Publishing is a later act: a render
// takes minutes, and while it runs other merges move the sources' main, so a
// publish that judged the pins again would refuse, as "N commits behind
// main", exactly the commits the render was right to ship. Judging them once,
// when they are rendered, keeps the safety the check exists for — nothing is
// delivered that no render judged current or held — and publish refuses a
// tree once its pins move at all (holdRenderRecord), so what it delivers is
// always what was judged.
type PinCurrency struct {
	// Behind names every pin the gate found behind its source, or could not
	// compare with it, that no hold covers: empty when every pin was current
	// or held. A render records a non-empty list only under the operator's
	// override, and publish refuses it under the same override only.
	Behind []string `json:"behind,omitempty"`
	// HeldUntil is the earliest end date (YYYY-MM-DD) of the pin-holds the
	// judgement relied on: past it the judgement no longer stands, as the
	// hold no longer would (a hold applies through its date).
	HeldUntil string `json:"heldUntil,omitempty"`
}

// Expired reports a judgement that relied on a pin-hold which has ended.
func (judged *PinCurrency) Expired(now time.Time) bool {
	if judged == nil || judged.HeldUntil == "" {
		return false
	}
	until, err := time.Parse("2006-01-02", judged.HeldUntil)
	return err != nil || now.After(until.Add(24*time.Hour))
}

// RenderedPinCurrency is the pin judgement the last render of module for
// environment recorded, nil when its record carries none — no record, a
// render that did not finish, or one that no gate judged — so the caller
// judges the pins against their sources itself, as before.
func RenderedPinCurrency(workspaceDir, environment, module string) *PinCurrency {
	data, err := os.ReadFile(workingrender.Record(workspaceDir, environment, module))
	if err != nil {
		return nil
	}
	var record renderRecord
	if json.Unmarshal(data, &record) != nil || record.Outcome != renderRecordRendered {
		return nil
	}
	return record.PinCurrency
}

// compositionPins is the composition a render reads, as a render record
// keeps it.
func compositionPins(workspace *resources.Workspace) map[string]string {
	if workspace == nil {
		return nil
	}
	pins := map[string]string{}
	for _, ref := range workspace.Modules {
		if pin := compositionPin(workspace, ref.Name); pin != nil {
			pins[ref.Name] = pin.Source + "@" + pin.Version
		}
	}
	return pins
}

// writeRenderRecord records a render of the working render at destination;
// a destination that is not a module's working render keeps no record.
func writeRenderRecord(destination string, record *renderRecord) error {
	workspaceDir, environment, module, ok := workingrender.RecordOf(destination)
	if !ok {
		return nil
	}
	return storeRenderRecord(workspaceDir, environment, module, record)
}

func storeRenderRecord(workspaceDir, environment, module string, record *renderRecord) error {
	// Prepare first: it is what makes the renders root ignore itself, and a
	// render can fail before anything else under it was written.
	if err := workingrender.Prepare(workspaceDir, environment); err != nil {
		return err
	}
	record.Module, record.Environment = module, environment
	record.At = time.Now().UTC()
	if len(record.Error) > renderRecordErrorLimit {
		record.Error = record.Error[:renderRecordErrorLimit] + "…"
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := workingrender.Record(workspaceDir, environment, module)
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("record the render of %s for %s: %w", module, environment, err)
	}
	staged := path + ".tmp"
	if err = os.WriteFile(staged, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("record the render of %s for %s: %w", module, environment, err)
	}
	if err = os.Rename(staged, path); err != nil {
		return fmt.Errorf("record the render of %s for %s: %w", module, environment, err)
	}
	return nil
}

// RecordRenderFailure records that the render of module for environment
// failed with cause before it could render the tree itself — a
// whole-composition render that stopped before reaching it — so publish
// refuses the earlier tree it left in place.
func RecordRenderFailure(workspaceDir, environment, module string, cause error) error {
	record := renderRecord{Outcome: renderRecordFailed}
	if cause != nil {
		record.Error = cause.Error()
	}
	return storeRenderRecord(workspaceDir, environment, module, &record)
}

// holdRenderRecord refuses to publish the working render of module for
// environment unless its last render succeeded, against the composition as it
// is pinned now.
func holdRenderRecord(workspace *resources.Workspace, module, environment string) error {
	renderAgain := fmt.Sprintf("run `codefly deploy gitops render %s --env %s` again", module, environment)
	data, err := os.ReadFile(workingrender.Record(workspace.Dir(), environment, module))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("module %s for environment %s has a working render but no record of the render that produced it (a CLI that kept none rendered it, or it was put there by hand), so nothing says it is the last render's: %s", module, environment, renderAgain)
	}
	if err != nil {
		return fmt.Errorf("read the render record of module %s for environment %s: %w", module, environment, err)
	}
	var record renderRecord
	if err = json.Unmarshal(data, &record); err != nil {
		return fmt.Errorf("the render record of module %s for environment %s is unreadable (%w): %s", module, environment, err, renderAgain)
	}
	when := record.At.Format(time.RFC3339)
	switch record.Outcome {
	case renderRecordRendered:
	case renderRecordFailed:
		return fmt.Errorf("the last render of module %s for environment %s failed at %s (%s), so its working render is an earlier render's and publishing it would deliver what that failure was replacing: %s once it succeeds", module, environment, when, record.Error, renderAgain)
	case renderRecordRendering:
		return fmt.Errorf("the last render of module %s for environment %s, started at %s, never finished (it was interrupted), so its working render is an earlier render's: %s", module, environment, when, renderAgain)
	default:
		return fmt.Errorf("the render record of module %s for environment %s has outcome %q, which this CLI does not know: %s", module, environment, record.Outcome, renderAgain)
	}
	if moved := movedPins(record.Pins, compositionPins(workspace)); len(moved) > 0 {
		return fmt.Errorf("the composition's pins moved since module %s was rendered for environment %s at %s (%s), so its working render is of another composition: %s", module, environment, when, strings.Join(moved, "; "), renderAgain)
	}
	return nil
}

// movedPins describes every pin that differs between what a render read and
// the composition now.
func movedPins(rendered, current map[string]string) []string {
	var moved []string
	for name, now := range current {
		was, ok := rendered[name]
		switch {
		case !ok:
			moved = append(moved, fmt.Sprintf("%s added at %s", name, now))
		case was != now:
			moved = append(moved, fmt.Sprintf("%s %s → %s", name, was, now))
		}
	}
	for name, was := range rendered {
		if _, ok := current[name]; !ok {
			moved = append(moved, fmt.Sprintf("%s removed (was %s)", name, was))
		}
	}
	sort.Strings(moved)
	return moved
}
