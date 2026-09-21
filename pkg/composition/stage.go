package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/agents/services"
	"github.com/codefly-dev/core/artifactexecution"
	core "github.com/codefly-dev/core/composition"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	solutionv0 "github.com/codefly-dev/core/generated/go/codefly/services/solution/v0"
	"github.com/codefly-dev/core/shared"
	"github.com/codefly-dev/core/solution"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type StageOptions struct {
	OutputParent  string
	Requests      []RenderInput
	BuildRequests []BuildInput
	IdentityKey   []byte
	HTTPClient    *http.Client
	// Every call needs its own explicit sandbox/principal choice.
	LoadOptions func(directory string) ([]manager.LoadOption, error)
}

type StagedRender struct {
	Directory  string                 `json:"directory"`
	InputsFile string                 `json:"inputsFile"`
	Record     *core.DeploymentRecord `json:"record"`
}

// StageRender invokes selected executors, requires registered-group shutdown,
// and verifies output bytes. It never applies, publishes or grants deployment approval.
func (session *SelectionSession) StageRender(ctx context.Context, files *DeploymentFiles, options *StageOptions) (*StagedRender, error) {
	if options == nil {
		return nil, errors.New("explicit staging options are required")
	}
	identity, err := ExecutionConfigurationIdentity(options.IdentityKey, options.Requests, options.BuildRequests)
	if err != nil {
		return nil, err
	}
	if identity != session.ConfigurationIdentity {
		return nil, errors.New("render configuration differs from the effective selection")
	}
	if options.LoadOptions == nil {
		return nil, errors.New("explicit executor sandbox and principal policy is required")
	}
	if files == nil || len(files.Executions) != 0 || len(files.Qualifications) != 0 {
		return nil, errors.New("staging requires runtime inputs without previous execution evidence or qualifications")
	}
	requests, err := decodeRenderInputs(options.Requests)
	if err != nil {
		return nil, err
	}
	parent, err := isolatedExecutionDirectory(options.OutputParent, nil)
	if err != nil {
		return nil, err
	}
	var result *StagedRender
	err = session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) (runErr error) {
		if parentErr := session.checkStagingParent(parent, snapshot.local); parentErr != nil {
			return parentErr
		}
		prepared, prepareErr := session.prepareRender(ctx, resolved, files)
		if prepareErr != nil {
			return prepareErr
		}
		batch, acquireErr := session.acquireRenderBatch(ctx, resolved, prepared, requests, options.HTTPClient)
		if acquireErr != nil {
			return acquireErr
		}
		directory, directoryErr := os.MkdirTemp(parent, "codefly-render-")
		if directoryErr != nil {
			return directoryErr
		}
		complete := false
		defer func() {
			if !complete {
				if cleanupErr := os.RemoveAll(directory); cleanupErr != nil {
					runErr = errors.Join(runErr, fmt.Errorf("remove incomplete staging %s: %w", directory, cleanupErr))
				}
			}
		}()
		if err = validateStagingDirectory(directory); err != nil {
			return err
		}
		data, encodeErr := json.Marshal(files)
		if encodeErr != nil {
			return encodeErr
		}
		var staged DeploymentFiles
		if err = json.Unmarshal(data, &staged); err != nil {
			return err
		}
		for i := range staged.Runtime {
			staged.Runtime[i].Path, err = filepath.Abs(staged.Runtime[i].Path)
			if err != nil {
				return err
			}
		}
		for i, call := range batch {
			output := filepath.Join(directory, fmt.Sprintf("%04d", i))
			if err = os.Mkdir(output, 0o700); err != nil {
				return err
			}
			loadOptions, optionErr := options.LoadOptions(output)
			if optionErr != nil {
				return optionErr
			}
			receipt, invokeErr := invokeRender(ctx, call.path, call.execution.Request(), call.payload, output, loadOptions)
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
		result, err = session.completeRender(ctx, snapshot, resolved, &staged, directory)
		if err != nil {
			return err
		}
		complete = true
		return nil
	})
	return result, err
}

func validateStagingDirectory(path string) error {
	directory, err := openProtectedAuthorityDirectory(path)
	if err != nil {
		return err
	}
	return errors.Join(validatePrivateInputDirectory(directory), directory.Close())
}

func (session *SelectionSession) checkStagingParent(parent string, local map[string]string) error {
	roots := make([]string, 0, 1+len(local))
	roots = append(roots, session.Root)
	for _, path := range local {
		roots = append(roots, path)
	}
	for _, root := range roots {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(canonical, parent)
		if err != nil {
			return err
		}
		if filepath.IsLocal(relative) {
			return errors.New("staging parent must be outside the product and local checkouts")
		}
	}
	return nil
}

type renderCall struct {
	execution *core.PreparedArtifactExecution
	payload   proto.Message
	path      string
}

func (session *SelectionSession) acquireRenderBatch(ctx context.Context, resolved *core.ResolvedComposition, prepared []*core.PreparedArtifactExecution, requests []renderInput, client *http.Client) ([]renderCall, error) {
	return session.acquireExecutionBatch(ctx, resolved, prepared, requests, client, "render")
}

func (session *SelectionSession) acquireExecutionBatch(ctx context.Context, resolved *core.ResolvedComposition, prepared []*core.PreparedArtifactExecution, requests []renderInput, client *http.Client, operationName string) ([]renderCall, error) {
	if len(prepared) != len(requests) {
		return nil, fmt.Errorf("explicit %s payload required for every selected service", operationName)
	}
	operations := resolved.Record().Operations
	batch := make([]renderCall, len(prepared))
	// Validate and acquire the whole batch before executing any member.
	for i, execution := range prepared {
		request := execution.Request()
		index := slices.IndexFunc(requests, func(input renderInput) bool {
			return input.target == request.Target && input.service == request.Service && input.protocol == request.Protocol
		})
		if index < 0 {
			return nil, fmt.Errorf("%s payload does not match selected service and protocol", operationName)
		}
		operation := slices.IndexFunc(operations, func(op core.ResolvedArtifactOperation) bool {
			return op.Target == request.Target && op.Service == request.Service && op.Operation == operationName
		})
		if operation < 0 {
			return nil, fmt.Errorf("selected %s operation is missing", operationName)
		}
		artifact := operations[operation].Executor.Artifact
		if artifact.MediaType != "application/octet-stream" {
			return nil, errors.New("staging supports only raw application/octet-stream native executors; no archive or media inference")
		}
		path, err := session.acquireArtifact(ctx, client, artifact)
		if err != nil {
			return nil, err
		}
		batch[i] = renderCall{execution: execution, payload: requests[index].request, path: path}
	}
	return batch, nil
}

func (session *SelectionSession) completeRender(ctx context.Context, snapshot *selectionSnapshot, resolved *core.ResolvedComposition, staged *DeploymentFiles, directory string) (*StagedRender, error) {
	inputs, closeFiles, err := session.verifiedDeploymentInputs(ctx, resolved, staged)
	if err != nil {
		return nil, err
	}
	defer closeFiles()
	record, err := session.Engine.CheckDeploymentInputs(ctx, resolved, inputs)
	if err != nil {
		return nil, err
	}
	if err = session.unchanged(ctx, snapshot); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(staged, "", "  ")
	if err != nil {
		return nil, err
	}
	inputsFile := filepath.Join(directory, "inputs.json")
	if err = shared.WriteFileAtomic(ctx, inputsFile, data, 0o600); err != nil {
		return nil, err
	}
	return &StagedRender{Directory: directory, InputsFile: inputsFile, Record: record}, nil
}

const renderShutdownTimeout = 10 * time.Second

func invokeRender(ctx context.Context, path string, execution *basev0.ArtifactExecution, payload proto.Message, directory string, opts []manager.LoadOption) (receipt *basev0.ArtifactExecutionReceipt, renderErr error) {
	if execution == nil || (execution.Protocol != artifactexecution.BuilderRender && execution.Protocol != artifactexecution.SolutionRender) {
		return nil, errors.New("unsupported render protocol")
	}
	return invokeArtifactExecution(ctx, path, execution, payload, directory, opts)
}

func invokeArtifactExecution(ctx context.Context, path string, execution *basev0.ArtifactExecution, payload proto.Message, directory string, opts []manager.LoadOption) (receipt *basev0.ArtifactExecutionReceipt, renderErr error) {
	conn, err := manager.LoadArtifact(ctx, path, execution, opts...)
	if err != nil {
		return nil, err
	}
	defer func() {
		// RPC cancellation must not cancel authenticated group shutdown. No
		// receipt can establish completion while a registered writer survives.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renderShutdownTimeout)
		defer cancel()
		if shutdownErr := conn.CloseAndWait(cleanupCtx); shutdownErr != nil {
			receipt = nil
			operation := "renderer"
			if execution.Protocol == artifactexecution.BuilderBuild {
				operation = "builder"
			}
			renderErr = errors.Join(renderErr, fmt.Errorf("shutdown selected %s: %w", operation, shutdownErr))
		}
	}()
	switch execution.Protocol {
	case artifactexecution.BuilderBuild:
		request, ok := proto.Clone(payload).(*builderv0.BuildRequest)
		if !ok {
			return nil, errors.New("builder build payload type mismatch")
		}
		request.Execution, request.OutputDirectory = execution, directory
		client := services.NewBuilderAgentClient(conn.GRPCConn())
		client.ProcessInfo = conn.ProcessInfo()
		response, callErr := client.Build(ctx, request)
		if callErr != nil {
			return nil, errors.New("selected builder build failed; no staged result was accepted")
		}
		return response.Execution, nil
	case artifactexecution.BuilderRender:
		request, ok := proto.Clone(payload).(*builderv0.DeploymentRequest)
		if !ok {
			return nil, errors.New("builder render payload type mismatch")
		}
		request.Execution, request.OutputDirectory = execution, directory
		client := services.NewBuilderAgentClient(conn.GRPCConn())
		client.ProcessInfo = conn.ProcessInfo()
		response, callErr := client.Deploy(ctx, request)
		if callErr != nil {
			return nil, errors.New("selected builder render failed; no staged result was accepted")
		}
		return response.Execution, nil
	case artifactexecution.SolutionRender:
		client := solution.NewClient(conn.GRPCConn())
		info, inspectErr := client.GetSolutionInformation(ctx, solution.CeilingInspect(), &solutionv0.GetSolutionInformationRequest{Artifact: &solutionv0.SolutionArtifact{ArtifactDigest: conn.ArtifactDigest()}})
		if inspectErr != nil {
			return nil, errors.New("selected solution identity inspection failed")
		}
		if info.GetArtifact().GetArtifactDigest() != conn.ArtifactDigest() {
			return nil, errors.New("solution returned a different executor identity")
		}
		request, ok := proto.Clone(payload).(*solutionv0.RenderRequest)
		if !ok {
			return nil, errors.New("solution render payload type mismatch")
		}
		if request.Context == nil {
			request.Context = &solutionv0.SolutionContext{}
		}
		request.Context.Artifact = info.Artifact
		request.Execution, request.Destination = execution, directory
		response, callErr := client.Render(ctx, solution.CeilingRender(), request)
		if callErr != nil {
			return nil, errors.New("selected solution render failed; no staged result was accepted")
		}
		return response.Execution, nil
	default:
		return nil, errors.New("unsupported render protocol")
	}
}
