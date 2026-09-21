package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/shared"
	"google.golang.org/protobuf/encoding/protojson"
)

// StagedBuild contains invocation evidence only. It is not an authorized derived
// output, a published URI, functional qualification or deployment approval.
type StagedBuild struct {
	Identity          string           `json:"identity"`
	SelectionIdentity string           `json:"selectionIdentity"`
	Directory         string           `json:"directory"`
	EvidenceFile      string           `json:"evidenceFile"`
	Executions        []ExecutionFiles `json:"executions"`
}

// StageBuild uses only Core's computed source-build operations. Render payloads
// remain in the configuration identity used for subsequent render/admission.
func (session *SelectionSession) StageBuild(ctx context.Context, options *StageOptions) (*StagedBuild, error) {
	if options == nil || options.LoadOptions == nil {
		return nil, errors.New("explicit build staging options, sandbox and principal policy are required")
	}
	identity, err := ExecutionConfigurationIdentity(options.IdentityKey, options.Requests, options.BuildRequests)
	if err != nil {
		return nil, err
	}
	if identity != session.ConfigurationIdentity {
		return nil, errors.New("build/render configuration differs from the effective selection")
	}
	requests, err := decodeBuildInputs(options.BuildRequests)
	if err != nil {
		return nil, err
	}
	parent, err := isolatedExecutionDirectory(options.OutputParent, nil)
	if err != nil {
		return nil, err
	}
	var result *StagedBuild
	err = session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) (runErr error) {
		if parentErr := session.checkStagingParent(parent, snapshot.local); parentErr != nil {
			return parentErr
		}
		prepared, prepareErr := session.Engine.PrepareArtifactExecutions(ctx, resolved, "build", core.DeploymentInputs{})
		if prepareErr != nil {
			return prepareErr
		}
		if len(prepared) == 0 {
			return errors.New("effective selection has no source-build requirements")
		}
		batch, acquireErr := session.acquireExecutionBatch(ctx, resolved, prepared, requests, options.HTTPClient, "build")
		if acquireErr != nil {
			return acquireErr
		}
		directory, directoryErr := os.MkdirTemp(parent, "codefly-build-")
		if directoryErr != nil {
			return directoryErr
		}
		complete := false
		defer func() {
			if !complete {
				runErr = errors.Join(runErr, os.RemoveAll(directory))
			}
		}()
		if err = validateStagingDirectory(directory); err != nil {
			return err
		}
		staged := &StagedBuild{SelectionIdentity: resolved.Identity(), Directory: directory, EvidenceFile: filepath.Join(directory, "build.json")}
		for i, call := range batch {
			output := filepath.Join(directory, fmt.Sprintf("%04d", i))
			if err = os.Mkdir(output, 0o700); err != nil {
				return err
			}
			loadOptions, optionErr := options.LoadOptions(output)
			if optionErr != nil {
				return optionErr
			}
			receipt, invokeErr := invokeArtifactExecution(ctx, call.path, call.execution.Request(), call.payload, output, loadOptions)
			if invokeErr != nil {
				return invokeErr
			}
			if _, err = core.VerifyArtifactExecutionDirectory(ctx, call.execution, receipt, output); err != nil {
				return err
			}
			encoded, encodeErr := protojson.Marshal(receipt)
			if encodeErr != nil {
				return encodeErr
			}
			request := call.execution.Request()
			staged.Executions = append(staged.Executions, ExecutionFiles{Target: request.Target, Service: request.Service, Directory: output, Receipt: encoded})
		}
		// Rehash earlier outputs after later executors have stopped; one executor
		// finishing is not proof that the rest of a batch left its files alone.
		for i, execution := range staged.Executions {
			var receipt basev0.ArtifactExecutionReceipt
			if err = protojson.Unmarshal(execution.Receipt, &receipt); err != nil {
				return err
			}
			if _, err = core.VerifyArtifactExecutionDirectory(ctx, prepared[i], &receipt, execution.Directory); err != nil {
				return err
			}
		}
		if err = errors.Join(ctx.Err(), resolved.CheckLocalInputs(), session.unchanged(snapshot)); err != nil {
			return err
		}
		staged.Identity, err = buildEvidenceIdentity(staged)
		if err != nil {
			return err
		}
		data, encodeErr := json.MarshalIndent(staged, "", "  ")
		if encodeErr != nil {
			return encodeErr
		}
		if err = shared.WriteFileAtomic(ctx, staged.EvidenceFile, data, 0o600); err != nil {
			return err
		}
		complete, result = true, staged
		return nil
	})
	return result, err
}

// The independently retained invocation digest binds measured output receipts,
// not their mutable filesystem locations. Core still rechecks all actual bytes.
func buildEvidenceIdentity(staged *StagedBuild) (string, error) {
	value := *staged
	value.Identity, value.Directory, value.EvidenceFile = "", "", ""
	value.Executions = append([]ExecutionFiles(nil), staged.Executions...)
	for i := range value.Executions {
		value.Executions[i].Directory = ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return contentDigest(data), nil
}
