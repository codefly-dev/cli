package agents

import (
	"bytes"
	"fmt"
	"os/exec"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Source commands emit their typed response on stdout. Agent downloads and
// compiler diagnostics use stderr, including on successful cold-cache runs.
// Keep those streams separate; a warm plugin cache must not govern whether
// a valid Builder response can be decoded.
func runAgentSourceJSON(command *exec.Cmd, operation string, response proto.Message) error {
	var diagnostics bytes.Buffer
	command.Stderr = &diagnostics
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("plugin-owned %s: %w\nstdout: %s\nstderr: %s", operation, err,
			boundedAgentCIOutput(output), boundedAgentCIOutput(diagnostics.Bytes()))
	}
	if err := protojson.Unmarshal(bytes.TrimSpace(output), response); err != nil {
		return fmt.Errorf("decode %s response: %w\nstdout: %s\nstderr: %s", operation, err,
			boundedAgentCIOutput(output), boundedAgentCIOutput(diagnostics.Bytes()))
	}
	return nil
}
