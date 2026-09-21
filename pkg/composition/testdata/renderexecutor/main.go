// Command renderexecutor is a test-only independent, selection-bound renderer.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/agents/contract"
	"github.com/codefly-dev/core/artifactexecution"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	agentv0 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	solutionv0 "github.com/codefly-dev/core/generated/go/codefly/services/solution/v0"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type renderer struct {
	digest   string
	contract string
}

type agentRenderer struct {
	agentv0.UnimplementedAgentServer
	*renderer
}
type builderRenderer struct {
	builderv0.UnimplementedBuilderServer
	*renderer
}
type solutionRenderer struct {
	solutionv0.UnimplementedSolutionServer
	*renderer
}

func (r *agentRenderer) GetAgentInformation(context.Context, *agentv0.AgentInformationRequest) (*agentv0.AgentInformation, error) {
	declaration := contract.Current()
	if os.Getenv("RENDER_TEST_MODE") == "missing-protocol" {
		declaration = &agentv0.AgentContract{}
	}
	return &agentv0.AgentInformation{Contract: declaration}, nil
}

func (r *solutionRenderer) GetSolutionInformation(_ context.Context, request *solutionv0.GetSolutionInformationRequest) (*solutionv0.GetSolutionInformationResponse, error) {
	if !proto.Equal(request.GetArtifact(), r.solutionIdentity()) && (request.GetArtifact().GetPublisher() != "" || request.GetArtifact().GetName() != "" || request.GetArtifact().GetVersion() != "") {
		return nil, errors.New("host invented executor coordinates")
	}
	return &solutionv0.GetSolutionInformationResponse{Artifact: r.solutionIdentity(), Capabilities: &solutionv0.SolutionCapabilities{SupportsRender: true, ExecutionContracts: []string{r.contract}}}, nil
}

func (r *renderer) solutionIdentity() *solutionv0.SolutionArtifact {
	return &solutionv0.SolutionArtifact{Publisher: "test-owner", Name: "declared-renderer", Version: "test-version", ArtifactDigest: r.digest}
}

func (r *solutionRenderer) Render(ctx context.Context, request *solutionv0.RenderRequest) (*solutionv0.RenderResponse, error) {
	if !proto.Equal(request.GetContext().GetArtifact(), r.solutionIdentity()) {
		return nil, errors.New("returned identity was not carried into render")
	}
	receipt, err := r.emit(ctx, request.Execution, artifactexecution.SolutionRender, request.Destination)
	return &solutionv0.RenderResponse{Execution: receipt}, err
}

func (r *builderRenderer) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{ExecutionContracts: []string{r.contract}}, nil
}

func (r *builderRenderer) Build(ctx context.Context, request *builderv0.BuildRequest) (*builderv0.BuildResponse, error) {
	receipt, err := r.emit(ctx, request.Execution, artifactexecution.BuilderBuild, request.OutputDirectory)
	state := builderv0.BuildStatus_SUCCESS
	if os.Getenv("RENDER_TEST_MODE") == "failed" {
		state = builderv0.BuildStatus_ERROR
	}
	return &builderv0.BuildResponse{State: &builderv0.BuildStatus{State: state}, Execution: receipt}, err
}

func (r *builderRenderer) Deploy(ctx context.Context, request *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	receipt, err := r.emit(ctx, request.Execution, artifactexecution.BuilderRender, request.OutputDirectory)
	state := builderv0.DeploymentStatus_SUCCESS
	if os.Getenv("RENDER_TEST_MODE") == "failed" {
		state = builderv0.DeploymentStatus_ERROR
	}
	return &builderv0.DeploymentResponse{State: &builderv0.DeploymentStatus{State: state}, Execution: receipt}, err
}

func (r *renderer) emit(ctx context.Context, execution *basev0.ArtifactExecution, protocol, directory string) (*basev0.ArtifactExecutionReceipt, error) {
	if err := artifactexecution.Check(execution, protocol, r.digest, []string{r.contract}); err != nil {
		return nil, err
	}
	if os.Getenv("RENDER_TEST_MODE") == "wait" {
		if err := os.WriteFile(filepath.Join(directory, "waiting"), []byte("active"), 0o600); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	data, err := json.Marshal(execution)
	if err != nil {
		return nil, err
	}
	if protocol == artifactexecution.BuilderBuild {
		data, err = buildSelectedSource(ctx, execution)
		if err != nil {
			return nil, err
		}
	}
	receipt := &basev0.ArtifactExecutionReceipt{Identity: execution.Identity}
	for _, output := range execution.Outputs {
		path := output.Name + ".json"
		if err := os.WriteFile(filepath.Join(directory, path), data, 0o600); err != nil {
			return nil, err
		}
		receipt.Outputs = append(receipt.Outputs, &basev0.ArtifactExecutionOutput{
			Name: output.Name, MediaType: output.MediaType, Path: path, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
		})
	}
	if control := os.Getenv("RENDER_TEST_CONTROL"); control != "" {
		if err := controlledRender(ctx, control, filepath.Join(directory, execution.Outputs[0].Name+".json")); err != nil {
			return nil, err
		}
	}
	switch os.Getenv("RENDER_TEST_MODE") {
	case "missing-receipt":
		return nil, nil
	case "wrong-receipt":
		receipt.Identity = r.digest
	case "extra-file":
		if err := os.WriteFile(filepath.Join(directory, "extra"), data, 0o600); err != nil {
			return nil, err
		}
	}
	return receipt, nil
}

// The test builder performs real packaging of the declared source bytes, not
// an acknowledgement-only Build implementation.
func buildSelectedSource(ctx context.Context, execution *basev0.ArtifactExecution) ([]byte, error) {
	if len(execution.Inputs) != 1 || execution.Inputs[0].Name != "source" {
		return nil, errors.New("packager requires exactly one declared source")
	}
	input := execution.Inputs[0]
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, input.Uri, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("source acquisition failed")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != input.Digest {
		return nil, errors.New("source bytes differ from selected digest")
	}
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err = writer.Write(data); err != nil {
		return nil, err
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func main() {
	if output := os.Getenv("RENDER_TEST_CHILD_OUTPUT"); output != "" {
		if err := childWriter(output); err != nil {
			panic(err)
		}
		return
	}
	address := flag.String("address-file", "", "Listening address output file")
	contract := flag.String("contract", artifactexecution.Contract, "Explicit execution capability")
	flag.Parse()
	path, err := os.Executable()
	if err != nil {
		panic(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	r := &renderer{digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), contract: *contract}
	if os.Getenv("RENDER_TEST_MODE") == "unsupported" {
		r.contract = "artifact-execution/v2"
	}
	if *address == "" {
		agents.Serve(agents.PluginRegistration{Agent: &agentRenderer{renderer: r}, Builder: &builderRenderer{renderer: r}, Solution: &solutionRenderer{renderer: r}})
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, &builderRenderer{renderer: r})
	if err := os.WriteFile(*address, []byte(listener.Addr().String()), 0o600); err != nil {
		panic(err)
	}
	if err := server.Serve(listener); err != nil {
		panic(err)
	}
}

func controlledRender(ctx context.Context, control, output string) error {
	childPID := 0
	if os.Getenv("RENDER_TEST_MODE") == "child-writer" {
		path, err := os.Executable()
		if err != nil {
			return err
		}
		child := exec.Command(path)
		child.Env = append(os.Environ(), "RENDER_TEST_CHILD_OUTPUT="+output)
		if err = child.Start(); err != nil {
			return err
		}
		childPID = child.Process.Pid
		go func() { _ = child.Wait() }()
		if err = waitForFile(ctx, filepath.Join(control, "child-ready")); err != nil {
			return err
		}
	}
	data, err := json.Marshal(struct{ PGID, ChildPID int }{syscall.Getpgrp(), childPID})
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(control, "ready"), data, 0o600); err != nil {
		return err
	}
	return waitForFile(ctx, filepath.Join(control, "release"))
}

func childWriter(output string) error {
	// Stay in the inherited tracked group but outlive its leader's graceful exit.
	signal.Ignore(syscall.SIGTERM, os.Interrupt)
	control := os.Getenv("RENDER_TEST_CONTROL")
	if err := os.WriteFile(filepath.Join(control, "child-ready"), []byte("ready"), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := waitForFile(ctx, filepath.Join(control, "mutate")); err != nil {
		return err
	}
	if err := os.WriteFile(output, []byte("surviving child changed output"), 0o600); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func waitForFile(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
