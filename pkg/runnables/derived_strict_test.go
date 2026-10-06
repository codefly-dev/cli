package runnables_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/runnables"
	"github.com/codefly-dev/cli/pkg/runnables/runnablestest"
	"github.com/stretchr/testify/require"
)

func TestDerivedReadersRefuseUnrecognizedAuthority(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		index bool
		edit  func(string) string
		want  string
	}{
		{"index field", true, func(s string) string { return strings.Replace(s, "{", `{"required_reader_feature":"future",`, 1) }, "required_reader_feature"},
		{"operation field", false, func(s string) string {
			return strings.Replace(s, "{", `{"future_required_authority":[{"name":"downstream"}],`, 1)
		}, "future_required_authority"},
		{"nested scope field", false, func(s string) string {
			return strings.Replace(s, `"resource_kind":`, `"required_constraint":"future","resource_kind":`, 1)
		}, "required_constraint"},
		{"index trailing value", true, func(s string) string { return s + ` {}` }, ""},
		{"operation trailing value", false, func(s string) string { return s + ` {}` }, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			runnablestest.Write(t, dir, "test-workspace", "documents", "runtime-worker-grpc-apply-text")
			index, err := runnables.LoadIndex(dir)
			require.NoError(t, err)
			require.Len(t, index.Operations, 1)
			file := filepath.Join(dir, index.Operations[0].Path, runnables.OperationFileName)
			if scenario.index {
				file = filepath.Join(dir, runnables.DerivedDir, runnables.IndexFileName)
			}
			original, err := os.ReadFile(file)
			require.NoError(t, err)
			changed := scenario.edit(string(original))
			require.NotEqual(t, string(original), changed)
			require.NoError(t, os.WriteFile(file, []byte(changed), 0o600))
			_, err = runnables.LoadDerivedOperations(dir)
			require.Error(t, err)
			if scenario.want != "" {
				require.ErrorContains(t, err, scenario.want)
			}
		})
	}
}
