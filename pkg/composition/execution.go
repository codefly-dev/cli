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
	"google.golang.org/protobuf/encoding/protojson"
)

// ExecutionFiles is persisted transport, never trusted execution evidence.
// Every inspection prepares the current request and rehashes the staged files.
type ExecutionFiles struct {
	Target    string          `json:"target"`
	Service   string          `json:"service"`
	Directory string          `json:"directory"`
	Receipt   json.RawMessage `json:"receipt"`
}

// PrepareRender exposes Core's bound requests without invoking an executor.
// Protocol, representation and input slots come only from signed declarations.
func (session *SelectionSession) PrepareRender(ctx context.Context, files *DeploymentFiles) ([]json.RawMessage, error) {
	var requests []json.RawMessage
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		prepared, err := session.prepareRender(ctx, resolved, files)
		if err != nil {
			return err
		}
		for _, execution := range prepared {
			data, err := protojson.Marshal(execution.Request())
			if err != nil {
				return err
			}
			requests = append(requests, data)
		}
		return session.unchanged(snapshot)
	})
	return requests, err
}

func (session *SelectionSession) prepareRender(ctx context.Context, resolved *core.ResolvedComposition, files *DeploymentFiles) ([]*core.PreparedArtifactExecution, error) {
	inputs, closeFiles, err := openDeploymentInputs(files)
	if err != nil {
		return nil, err
	}
	defer closeFiles()
	// One batch consumes runtime streams once, not once per selected service.
	return session.Engine.PrepareArtifactExecutions(ctx, resolved, "render", inputs)
}

func (session *SelectionSession) verifiedDeploymentInputs(ctx context.Context, resolved *core.ResolvedComposition, files *DeploymentFiles) (core.DeploymentInputs, func(), error) {
	if files == nil || len(files.Executions) == 0 {
		return openDeploymentInputs(files)
	}
	prepared, err := session.prepareRender(ctx, resolved, files)
	if err != nil {
		return core.DeploymentInputs{}, nil, err
	}
	if len(files.Executions) != len(prepared) {
		return core.DeploymentInputs{}, nil, errors.New("staged render evidence is required for every selected service")
	}
	type serviceKey struct{ target, service string }
	byService := make(map[serviceKey]*core.PreparedArtifactExecution, len(prepared))
	for _, execution := range prepared {
		request := execution.Request()
		byService[serviceKey{request.Target, request.Service}] = execution
	}
	var verified []*core.VerifiedArtifactExecution
	var directories []string
	for _, input := range files.Executions {
		key := serviceKey{input.Target, input.Service}
		execution := byService[key]
		if execution == nil {
			return core.DeploymentInputs{}, nil, errors.New("unselected or duplicate render evidence")
		}
		delete(byService, key)
		var directory string
		directory, err = isolatedExecutionDirectory(input.Directory, directories)
		if err != nil {
			return core.DeploymentInputs{}, nil, err
		}
		directories = append(directories, directory)
		var receipt basev0.ArtifactExecutionReceipt
		if err = protojson.Unmarshal(input.Receipt, &receipt); err != nil {
			return core.DeploymentInputs{}, nil, fmt.Errorf("decode execution receipt: %w", err)
		}
		var sealed *core.VerifiedArtifactExecution
		sealed, err = core.VerifyArtifactExecutionDirectory(ctx, execution, &receipt, directory)
		if err != nil {
			return core.DeploymentInputs{}, nil, fmt.Errorf("verify staged render for %s/%s: %w", input.Target, input.Service, err)
		}
		verified = append(verified, sealed)
	}
	// Preparation consumed the first streams. Admission must read fresh handles,
	// so changes between preparation and admission cannot reuse stale hashes.
	inputs, closeFiles, err := openDeploymentInputs(files)
	if err != nil {
		return core.DeploymentInputs{}, nil, err
	}
	inputs.Executions = verified
	return inputs, closeFiles, nil
}

func isolatedExecutionDirectory(directory string, prior []string) (string, error) {
	if !filepath.IsAbs(directory) {
		return "", errors.New("execution directory must be absolute")
	}
	clean := filepath.Clean(directory)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}
	if resolved != clean {
		return "", errors.New("execution directory must not contain symlinks")
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("execution directory must be a directory")
	}
	for _, other := range prior {
		forward, err := filepath.Rel(other, clean)
		if err != nil {
			return "", err
		}
		backward, err := filepath.Rel(clean, other)
		if err != nil {
			return "", err
		}
		if filepath.IsLocal(forward) || filepath.IsLocal(backward) {
			return "", errors.New("execution directories must be disjoint")
		}
	}
	return clean, nil
}
