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

// The roots a reload withdraws are the ones the run starts that the
// composition no longer names, the pinned ones excepted: a service or
// solution run's command-line roots are never withdrawn, a workspace run pins
// none.
func TestGoneRootsAreTheDroppedOnesTheRunDidNotPin(t *testing.T) {
	running := []string{"crm/api", "wiki/backend", "docs/documents"}
	require.Equal(t, []string{"wiki/backend", "docs/documents"}, goneRoots(map[string]bool{}, running, []string{"crm/api"}))
	require.Equal(t, []string{"wiki/backend"}, goneRoots(map[string]bool{"docs/documents": true}, running, []string{"crm/api"}), "a pinned root stays")
	require.Nil(t, goneRoots(map[string]bool{}, running, running), "nothing dropped withdraws nothing")
}
