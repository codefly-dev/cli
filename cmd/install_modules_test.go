package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallModulesRegisteredHelp(t *testing.T) {
	command, _, err := RootCmd.Find([]string{"install", "modules"})
	require.NoError(t, err)
	require.Equal(t, "modules", command.Name())
	var output bytes.Buffer
	previous := command.OutOrStdout()
	command.SetOut(&output)
	defer command.SetOut(previous)
	require.NoError(t, command.Help())
	require.Contains(t, output.String(), "Does not start agents or services")
}
