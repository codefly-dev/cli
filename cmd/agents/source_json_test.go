package agents

import (
	"errors"
	"os/exec"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestSourceJSONSeparatesRealProcessDiagnostics(t *testing.T) {
	for _, test := range []struct {
		operation string
		payload   string
		response  proto.Message
	}{
		{"Builder.Package", `{"state":{"state":"SUCCESS"}}`, &builderv0.PackageResponse{}},
		{"Builder.Audit", `{"state":{"state":"CLEAN"}}`, &builderv0.AuditResponse{}},
	} {
		t.Run(test.operation, func(t *testing.T) {
			// Exercise the process pipes, including diagnostics before and after
			// stdout. The end-to-end agent qualification covers the actual RPC.
			command := exec.Command("sh", "-c", `printf 'downloading 50%%\n' >&2; printf '%s\n' "$1"; printf 'download complete\n' >&2`, "source-json", test.payload)
			require.NoError(t, runAgentSourceJSON(command, test.operation, test.response))
			require.True(t, test.response.ProtoReflect().Has(test.response.ProtoReflect().Descriptor().Fields().ByName("state")))
		})
	}
}

func TestSourceJSONPreservesFailureAndStrictResponseValidation(t *testing.T) {
	t.Run("nonzero exit with valid response", func(t *testing.T) {
		command := exec.Command("sh", "-c", `printf '{"state":{"state":"SUCCESS"}}\n'; printf 'download failed\n' >&2; exit 9`)
		err := runAgentSourceJSON(command, "Builder.Package", &builderv0.PackageResponse{})
		var exitError *exec.ExitError
		require.True(t, errors.As(err, &exitError))
		require.Equal(t, 9, exitError.ExitCode())
		require.ErrorContains(t, err, "download failed")
	})
	for _, payload := range []string{
		`progress {"state":{"state":"SUCCESS"}}`,
		`{"unexpected":true}`,
		`{"state":{"state":"SUCCESS"}} {"state":{"state":"SUCCESS"}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			command := exec.Command("sh", "-c", `printf '%s\n' "$1"; printf 'retained diagnostic\n' >&2`, "source-json", payload)
			err := runAgentSourceJSON(command, "Builder.Package", &builderv0.PackageResponse{})
			require.ErrorContains(t, err, "decode Builder.Package response")
			require.ErrorContains(t, err, "retained diagnostic")
		})
	}
}
