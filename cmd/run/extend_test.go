package run

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The roots a reload admits are the ones neither the run nor the composition
// it started from knew: a root composed after the start joins, in the
// composition's order; a root the composition already named when a service or
// solution run began stays outside that run's closure, as does one the run
// starts itself.
func TestFreshRootsAreTheOnesComposedAfterTheRunStarted(t *testing.T) {
	known := map[string]bool{"saas/host": true, "crm/api": true}
	require.Equal(t, []string{"wiki/backend", "docs/documents"}, freshRoots(known, []string{"saas/host", "wiki/backend", "crm/api", "docs/documents"}))
	require.Nil(t, freshRoots(known, []string{"crm/api", "saas/host"}), "nothing composed after the start admits nothing")
	require.Nil(t, freshRoots(known, nil))
}
