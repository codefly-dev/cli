package common

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

func TestRepeatedContextsKeepOneInvocationLogAndForwardOneCopy(t *testing.T) {
	if os.Getenv("CODEFLY_CONTEXT_LOG_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRepeatedContextsKeepOneInvocationLogAndForwardOneCopy$")
		cmd.Env = append(os.Environ(), "CODEFLY_HOME="+t.TempDir(), "CODEFLY_CONTEXT_LOG_TEST=1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		return
	}
	cli.SuppressOutput()
	_, done := NewContext()
	first := cli.LogFilePath()
	cli.GetFileLogger().Process(&wool.Log{Level: wool.INFO, Message: "preparation-retained"})
	done() // workspace/solution preparation ends before service delegation.
	_, done = NewContext()
	defer done()
	require.Equal(t, first, cli.LogFilePath())
	data, err := json.Marshal(agents.LogMessage{Log: &wool.Log{Level: wool.INFO, Message: "forwarded-once"}})
	require.NoError(t, err)
	agents.GetLogHandler().ForwardLogs(strings.NewReader(string(data) + "\n"))
	actual, err := os.ReadFile(first)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(actual), "preparation-retained"))
	require.Equal(t, 1, strings.Count(string(actual), "forwarded-once"), "nested contexts must not register duplicate agent processors")
}
