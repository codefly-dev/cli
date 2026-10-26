package gitops

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// renderRecorder renders by name, answering the stale readers each module's
// render reports, and records the order modules were rendered in.
func renderRecorder(stale map[string]map[string][]string, rendered *[]string) func(context.Context, string) (RenderResult, error) {
	return func(_ context.Context, module string) (RenderResult, error) {
		*rendered = append(*rendered, module)
		return RenderResult{StaleGroupConsumers: stale[module]}, nil
	}
}

func TestRenderGroupReadersRendersTheReadersOfAChangedGroupAlone(t *testing.T) {
	var rendered []string
	announced := map[string][]string{}
	// tracing and robin read internal-auth; records reads nothing that changed.
	stale := map[string]map[string][]string{"saas": {"internal-auth": {"robin", "tracing"}}}
	results, err := RenderGroupReaders(t.Context(), "saas", renderRecorder(stale, &rendered), func(reader string, groups []string) { announced[reader] = groups })
	require.NoError(t, err)
	require.Equal(t, []string{"saas", "robin", "tracing"}, rendered)
	require.Len(t, results, 3)
	require.Equal(t, map[string][]string{"robin": {"internal-auth"}, "tracing": {"internal-auth"}}, announced)
	require.NotContains(t, rendered, "records")
}

func TestRenderGroupReadersRendersOnlyTheModuleWhenNoGroupChanged(t *testing.T) {
	var rendered []string
	_, err := RenderGroupReaders(t.Context(), "saas", renderRecorder(nil, &rendered), func(string, []string) { t.Fatal("no reader is re-rendered") })
	require.NoError(t, err)
	require.Equal(t, []string{"saas"}, rendered)
}

func TestRenderGroupReadersRendersAReaderOnceAcrossGroups(t *testing.T) {
	var rendered []string
	announced := map[string][]string{}
	stale := map[string]map[string][]string{
		"saas":  {"internal-auth": {"robin"}, "platform": {"robin", "saas"}},
		"robin": {"model": {"tracing"}},
	}
	_, err := RenderGroupReaders(t.Context(), "saas", renderRecorder(stale, &rendered), func(reader string, groups []string) { announced[reader] = groups })
	require.NoError(t, err)
	require.Equal(t, []string{"saas", "robin", "tracing"}, rendered)
	require.Equal(t, []string{"internal-auth", "platform"}, announced["robin"])
}
