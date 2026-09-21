// Command renderexecutor is a test-only independent, selection-bound renderer.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/artifactexecution"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"google.golang.org/grpc"
)

type renderer struct {
	builderv0.UnimplementedBuilderServer
	digest   string
	contract string
}

func (r *renderer) BuildCapabilities(context.Context, *builderv0.BuildCapabilitiesRequest) (*builderv0.BuildCapabilitiesResponse, error) {
	return &builderv0.BuildCapabilitiesResponse{ExecutionContracts: []string{r.contract}}, nil
}

func (r *renderer) Deploy(_ context.Context, request *builderv0.DeploymentRequest) (*builderv0.DeploymentResponse, error) {
	if err := artifactexecution.Check(request.Execution, artifactexecution.BuilderRender, r.digest, []string{r.contract}); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request.Execution)
	if err != nil {
		return nil, err
	}
	receipt := &basev0.ArtifactExecutionReceipt{Identity: request.Execution.Identity}
	for _, output := range request.Execution.Outputs {
		path := output.Name + ".json"
		if err := os.WriteFile(filepath.Join(request.OutputDirectory, path), data, 0o600); err != nil {
			return nil, err
		}
		receipt.Outputs = append(receipt.Outputs, &basev0.ArtifactExecutionOutput{
			Name: output.Name, MediaType: output.MediaType, Path: path, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)),
		})
	}
	return &builderv0.DeploymentResponse{State: &builderv0.DeploymentStatus{State: builderv0.DeploymentStatus_SUCCESS}, Execution: receipt}, nil
}

func main() {
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := grpc.NewServer()
	builderv0.RegisterBuilderServer(server, &renderer{digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), contract: *contract})
	if err := os.WriteFile(*address, []byte(listener.Addr().String()), 0o600); err != nil {
		panic(err)
	}
	if err := server.Serve(listener); err != nil {
		panic(err)
	}
}
