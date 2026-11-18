package cmd

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAgentUpdatesOnlyGateWorkspaceExecution(t *testing.T) {
	for _, tc := range []struct {
		group, leaf   string
		check, deploy bool
	}{
		{"run", "service", true, false}, {"run", "workspace", true, false},
		{"build", "module", true, false}, {"deploy", "service", true, true},
		{"run", "cluster", false, false}, {"update", "workspace", false, false},
	} {
		root := &cobra.Command{Use: "codefly"}
		group := &cobra.Command{Use: tc.group}
		leaf := &cobra.Command{Use: tc.leaf}
		root.AddCommand(group)
		group.AddCommand(leaf)
		check, deploy := agentUpdateCommand(leaf)
		require.Equal(t, tc.check, check)
		require.Equal(t, tc.deploy, deploy)
	}
}
