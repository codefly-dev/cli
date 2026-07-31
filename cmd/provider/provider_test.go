package provider

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandTree(t *testing.T) {
	require.Equal(t, CommandName, Cmd.Name())

	var names []string
	for _, sub := range Cmd.Commands() {
		names = append(names, sub.Name())
	}
	sort.Strings(names)
	require.Equal(t, []string{
		"apply", "destroy", "disconnect", "doctor", "import", "list", "plan", "setup",
	}, names, "the eight provider subcommands must all be registered")
}

func TestEnvAndJSONFlagsPresent(t *testing.T) {
	for _, sub := range Cmd.Commands() {
		if sub.Name() == "apply" {
			continue // apply takes --plan, not --env
		}
		require.NotNilf(t, sub.Flags().Lookup("env"), "%s must accept --env", sub.Name())
		require.NotNilf(t, sub.Flags().Lookup("json"), "%s must accept --json", sub.Name())
	}
}

func TestPlanRejectsConflictingModes(t *testing.T) {
	require.Error(t, errMutuallyExclusive("--validate-only", "--refresh-only"))
}

// Each subcommand owns its own env/json flags. With the earlier shared package
// globals, parsing one command's --json would have been observed by every other
// command in the same process; here it must not leak.
func TestFlagsAreScopedPerCommand(t *testing.T) {
	require.NoError(t, listCmd.ParseFlags([]string{"--schema", "--json", "--env", "staging"}))
	require.True(t, jsonOf(listCmd))
	require.Equal(t, "staging", envOf(listCmd))

	require.False(t, jsonOf(doctorCmd), "doctor must not observe list's --json")
	require.Equal(t, "local", envOf(doctorCmd), "doctor must keep its own --env default")
	require.False(t, jsonOf(setupCmd), "setup must not observe list's --json")
}
