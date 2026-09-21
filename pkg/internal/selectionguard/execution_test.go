package selectionguard

import (
	"os"
	"path/filepath"
	"testing"

	core "github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

func TestLegacyExecutionCannotIgnoreSelectionOrNestedDescriptor(t *testing.T) {
	for _, scenario := range []string{"selection", "nested descriptor", "replacements", "malformed descriptor"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			data := "kind: composed-module\nmodules:\n  include: [nested]\n"
			switch scenario {
			case "selection":
				require.NoError(t, os.WriteFile(filepath.Join(root, SelectionFile), []byte("{}"), 0o600))
			case "replacements":
				data = "kind: composed-module\nreplacements:\n  - target: modules/nested\n"
			case "malformed descriptor":
				data = "[malformed"
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, core.DescriptorFileName), []byte(data), 0o600))
			err := RejectUnboundExecution(root)
			if scenario == "malformed descriptor" {
				require.Error(t, err)
			} else {
				require.ErrorIs(t, err, ErrUnboundExecution)
			}
		})
	}
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, core.DescriptorFileName), []byte("name: existing-module\nservices: []\n"), 0o600))
	require.NoError(t, RejectUnboundExecution(root), "ordinary module deployment has no new selection to ignore")
}
