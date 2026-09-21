package composition

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPublishBuildCommandRequiresInspectedInputsAndPublicationDestination(t *testing.T) {
	command := NewCommand()
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetArgs([]string{"publish-build", "build.json", "inputs.json", "signers.json"})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "required flag(s)")
	command = NewCommand()
	command.SilenceErrors, command.SilenceUsage = true, true
	command.SetArgs([]string{"publish-build", "build.json", "inputs.json", "signers.json", "--expected-selection", "inspected", "--expected-build", "observed", "--repository", "registry.test/owner/builds", "--output", "/tmp/published.json"})
	require.ErrorContains(t, command.ExecuteContext(t.Context()), "--configuration and --identity-key")
	command = NewCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"publish-build", "--help"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	require.Contains(t, output.String(), "owner-authorized")
	require.Contains(t, output.String(), "--expected-selection")
}
