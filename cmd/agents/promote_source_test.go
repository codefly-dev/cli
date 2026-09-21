package agents

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourcePromotionNoLongerMaintainsCompatibilityPins(t *testing.T) {
	err := PromoteSourceCmd.RunE(PromoteSourceCmd, []string{"example.test/unknown:9.0.0"})
	require.ErrorContains(t, err, "no longer edits a CLI compatibility roster")
	require.ErrorContains(t, err, "test source --agent")
}
