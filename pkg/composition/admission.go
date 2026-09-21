package composition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	core "github.com/codefly-dev/core/composition"
)

type RuntimeFile struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	Path   string `json:"path"`
}

type DeploymentFiles struct {
	Runtime        []RuntimeFile              `json:"runtime"`
	Derived        []core.SignedDerivedOutput `json:"derived,omitempty"`
	Bindings       map[string]string          `json:"bindings"`
	Qualifications []core.SignedQualification `json:"qualifications"`
}

type AdmissionInspection struct {
	Identity string                `json:"identity"`
	Record   core.DeploymentRecord `json:"record"`
}

// CheckInputs returns the identities a qualification authority must attest.
// The result establishes neither functional readiness nor deployment approval.
func (session *SelectionSession) CheckInputs(ctx context.Context, files *DeploymentFiles) (*core.DeploymentRecord, error) {
	var record *core.DeploymentRecord
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		inputs, closeFiles, err := openDeploymentInputs(files)
		if err != nil {
			return err
		}
		defer closeFiles()
		record, err = session.Engine.CheckDeploymentInputs(ctx, resolved, inputs)
		if err != nil {
			return err
		}
		return session.unchanged(snapshot)
	})
	return record, err
}

func openDeploymentInputs(files *DeploymentFiles) (core.DeploymentInputs, func(), error) {
	if files == nil {
		return core.DeploymentInputs{}, nil, errors.New("deployment inputs are required")
	}
	inputs := core.DeploymentInputs{Derived: files.Derived, Bindings: files.Bindings, Qualifications: files.Qualifications}
	var opened []*os.File
	closeFiles := func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}
	for _, input := range files.Runtime {
		file, err := os.Open(input.Path)
		if err != nil {
			closeFiles()
			return inputs, nil, err
		}
		opened = append(opened, file)
		info, err := file.Stat()
		if err != nil {
			closeFiles()
			return inputs, nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > maxSelectionArtifactBytes {
			closeFiles()
			return inputs, nil, errors.New("runtime input must be a regular file within the artifact size limit")
		}
		inputs.Runtime = append(inputs.Runtime, core.RuntimeInput{Target: input.Target, Name: input.Name, Content: io.LimitReader(file, maxSelectionArtifactBytes+1)})
	}
	return inputs, closeFiles, nil
}

// Admit authenticates the supplied files and evidence with Core. This does not
// deploy anything or grant an effect owner permission to use different bytes.
func (session *SelectionSession) Admit(ctx context.Context, files *DeploymentFiles, policy core.DeploymentPolicy, now time.Time) (*AdmissionInspection, error) {
	var inspection *AdmissionInspection
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		inputs, closeFiles, err := openDeploymentInputs(files)
		if err != nil {
			return err
		}
		defer closeFiles()
		approved, err := session.Engine.AdmitDeployment(ctx, resolved, inputs, policy, now)
		if err != nil {
			return fmt.Errorf("deployment admission: %w", err)
		}
		if err := session.unchanged(snapshot); err != nil {
			return err
		}
		inspection = &AdmissionInspection{Identity: approved.Identity(), Record: approved.Record()}
		return nil
	})
	return inspection, err
}
