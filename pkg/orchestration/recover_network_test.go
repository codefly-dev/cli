package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoverUnusedNetworkRequiresAnImmutableID(t *testing.T) {
	for _, ref := range []string{"bridge", strings.Repeat("a", 12), strings.Repeat("F", 64), strings.Repeat("z", 64)} {
		for _, remove := range []bool{false, true} {
			ok, err := RecoverUnusedNetwork(context.Background(), ref, remove)
			require.ErrorContains(t, err, "full lowercase 64-hex")
			require.False(t, ok)
		}
	}
}
