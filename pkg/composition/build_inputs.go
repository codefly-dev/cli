package composition

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/codefly-dev/core/artifactexecution"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"google.golang.org/protobuf/encoding/protojson"
)

// BuildInput supplies only the existing Builder RPC payload. Core supplies the
// selected executor, source slots, output declarations and execution identity.
type BuildInput struct {
	Target  string          `json:"target"`
	Service string          `json:"service"`
	Request json.RawMessage `json:"request"`
}

func decodeBuildInputs(inputs []BuildInput) ([]renderInput, error) {
	if len(inputs) == 0 {
		return nil, errors.New("at least one explicit build request is required")
	}
	var result []renderInput
	seen := make(map[string]bool)
	for _, input := range inputs {
		key := input.Target + "\x00" + input.Service
		if input.Service == "" || strings.ContainsRune(input.Target, '\x00') || strings.ContainsRune(input.Service, '\x00') || seen[key] {
			return nil, errors.New("build request has an empty, invalid or duplicate service identity")
		}
		seen[key] = true
		request := &builderv0.BuildRequest{}
		if err := protojson.Unmarshal(input.Request, request); err != nil {
			return nil, errors.New("invalid build request protobuf JSON; payload values withheld")
		}
		if request.Execution != nil || request.OutputDirectory != "" {
			return nil, errors.New("build execution and staging directory are host-owned")
		}
		result = append(result, renderInput{input.Target, input.Service, artifactexecution.BuilderBuild, request})
	}
	return result, nil
}
