package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	core "github.com/codefly-dev/core/composition"
)

// RecordAdmission persists Core's admission result, not mutation authority or
// an observation of deployment. It never stores runtime paths or configuration.
func (session *SelectionSession) RecordAdmission(ctx context.Context, files *DeploymentFiles, policy core.DeploymentPolicy, now time.Time, destination string) (*AdmissionInspection, error) {
	if !filepath.IsAbs(destination) {
		return nil, errors.New("admission record destination must be absolute")
	}
	var inspection *AdmissionInspection
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		if err := session.checkStagingParent(filepath.Dir(destination), snapshot.local); err != nil {
			return err
		}
		var err error
		inspection, err = session.admitResolved(ctx, resolved, files, policy, now)
		if err != nil {
			return err
		}
		data, err := json.MarshalIndent(inspection, "", "  ")
		if err != nil {
			return err
		}
		if err = session.unchanged(ctx, snapshot); err != nil {
			return err
		}
		return publishAdmissionRecord(ctx, destination, append(data, '\n'))
	})
	if err != nil {
		return nil, err
	}
	return inspection, nil
}

type AdmissionRecheck struct {
	RecordedIdentity string              `json:"recordedIdentity"`
	Current          AdmissionInspection `json:"current"`
}

// RecheckAdmission never trusts the record itself as authority. expected must
// come from the caller's retained approval, not the supplied record. Current
// policy and actual bytes are checked again, including qualification expiration.
// No returned value grants permission to mutate or proves bytes stayed unchanged
// after this call; effect owners still need isolation and effect-time admission.
func (session *SelectionSession) RecheckAdmission(ctx context.Context, files *DeploymentFiles, policy core.DeploymentPolicy, recorded *AdmissionInspection, expected string, now time.Time) (*AdmissionRecheck, error) {
	var result *AdmissionRecheck
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		var err error
		result, err = session.recheckResolved(ctx, resolved, files, policy, recorded, expected, now)
		if err != nil {
			return err
		}
		return session.unchanged(ctx, snapshot)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (session *SelectionSession) recheckResolved(ctx context.Context, resolved *core.ResolvedComposition, files *DeploymentFiles, policy core.DeploymentPolicy, recorded *AdmissionInspection, expected string, now time.Time) (*AdmissionRecheck, error) {
	if recorded == nil || expected == "" || recorded.Identity != expected {
		return nil, errors.New("retained admission identity must match the supplied record")
	}
	if now.IsZero() || recorded.Record.ApprovedAt.IsZero() || recorded.Record.ApprovedAt.After(now) {
		return nil, errors.New("recorded admission time must not be zero or in the future")
	}
	var result *AdmissionRecheck
	err := func() error {
		// Core identities include ApprovedAt. Reproduce the original at that
		// instant, then independently admit at the current time using fresh files.
		original, err := session.admitResolved(ctx, resolved, files, policy, recorded.Record.ApprovedAt)
		if err != nil {
			return err
		}
		if original.Identity != expected {
			return errors.New("effective inputs or qualifications differ from the recorded admission")
		}
		want, err := json.Marshal(original)
		if err != nil {
			return err
		}
		got, err := json.Marshal(recorded)
		if err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			return errors.New("admission record content differs from Core's authenticated record")
		}
		current, err := session.admitResolved(ctx, resolved, files, policy, now)
		if err != nil {
			return err
		}
		comparison := *current
		comparison.Identity = original.Identity
		comparison.Record.ApprovedAt = original.Record.ApprovedAt
		got, err = json.Marshal(comparison)
		if err != nil {
			return err
		}
		if !bytes.Equal(want, got) {
			return errors.New("deployment inputs changed during admission recheck")
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		result = &AdmissionRecheck{RecordedIdentity: expected, Current: *current}
		return nil
	}()
	return result, err
}

// Link publishes a complete, synced file without replacing an existing record,
// even if an independent process wins the destination concurrently.
func publishAdmissionRecord(ctx context.Context, destination string, data []byte) (resultErr error) {
	parent, err := isolatedExecutionDirectory(filepath.Dir(destination), nil)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	// The private directory keeps temporary names out of the caller's namespace.
	temp, err := os.MkdirTemp(parent, ".codefly-admission-")
	if err != nil {
		return err
	}
	tempName := filepath.Base(temp)
	defer func() { resultErr = errors.Join(resultErr, root.RemoveAll(tempName)) }()
	file, err := root.OpenFile(filepath.Join(tempName, "record"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = root.Link(filepath.Join(tempName, "record"), filepath.Base(destination)); err != nil {
		return fmt.Errorf("publish admission without replacing existing records: %w", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
