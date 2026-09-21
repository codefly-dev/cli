package composition

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/codefly-dev/core/artifactexecution"
	core "github.com/codefly-dev/core/composition"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	solutionv0 "github.com/codefly-dev/core/generated/go/codefly/services/solution/v0"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// RenderInput carries the existing RPC payload, not another selection model.
// Execution, destination and verified executor identity remain host-owned.
type RenderInput struct {
	Target   string          `json:"target"`
	Service  string          `json:"service"`
	Protocol string          `json:"protocol"`
	Request  json.RawMessage `json:"request"`
}

type renderInput struct {
	target, service, protocol string
	request                   proto.Message
}

func decodeRenderInputs(inputs []RenderInput) ([]renderInput, error) {
	if len(inputs) == 0 {
		return nil, errors.New("at least one explicit render request is required")
	}
	var result []renderInput
	seen := map[string]bool{}
	for _, input := range inputs {
		key := input.Target + "\x00" + input.Service
		if input.Service == "" || strings.ContainsRune(input.Target, '\x00') || strings.ContainsRune(input.Service, '\x00') || seen[key] {
			return nil, errors.New("render request has an empty, invalid or duplicate service identity")
		}
		seen[key] = true
		var request proto.Message
		switch input.Protocol {
		case artifactexecution.BuilderRender:
			request = &builderv0.DeploymentRequest{}
		case artifactexecution.SolutionRender:
			request = &solutionv0.RenderRequest{}
		default:
			return nil, errors.New("render request protocol is unsupported")
		}
		if err := protojson.Unmarshal(input.Request, request); err != nil {
			// Parser errors can include configuration values.
			return nil, errors.New("invalid render request protobuf JSON")
		}
		switch value := request.(type) {
		case *builderv0.DeploymentRequest:
			if value.Execution != nil || value.OutputDirectory != "" {
				return nil, errors.New("render execution and staging directory are host-owned")
			}
		case *solutionv0.RenderRequest:
			if value.Execution != nil || value.Destination != "" || value.ArtifactReference != "" || value.Context.GetArtifact() != nil {
				return nil, errors.New("solution execution, artifact identity and destination are host-owned")
			}
		}
		result = append(result, renderInput{input.Target, input.Service, input.Protocol, request})
	}
	slices.SortFunc(result, func(a, b renderInput) int {
		return strings.Compare(a.target+"\x00"+a.service, b.target+"\x00"+b.service)
	})
	return result, nil
}

// RenderConfigurationIdentity binds the exact wire configuration for all calls.
// Sorting and deterministic protobuf encoding ignore JSON spelling/key order.
func RenderConfigurationIdentity(key []byte, inputs []RenderInput) (string, error) {
	decoded, err := decodeRenderInputs(inputs)
	if err != nil {
		return "", err
	}
	values := make(map[string]string, len(decoded))
	for _, input := range decoded {
		data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(input.request)
		if err != nil {
			return "", err
		}
		identity, err := json.Marshal([]string{input.target, input.service, input.protocol})
		if err != nil {
			return "", fmt.Errorf("render request identity: %w", err)
		}
		values[string(identity)] = base64.StdEncoding.EncodeToString(data)
	}
	return core.ConfigurationIdentity(key, values)
}
