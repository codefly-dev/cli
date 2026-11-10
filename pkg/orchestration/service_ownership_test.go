package orchestration

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckOwnedServices(t *testing.T) {
	flow := &Flow{}
	require.NoError(t, flow.checkOwnedServices([]string{"any/service"}))
	flow.WithOwnedServices([]string{"app/subject"})
	require.NoError(t, flow.checkOwnedServices([]string{"app/subject"}))
	require.ErrorContains(t, flow.checkOwnedServices([]string{"app/subject", "app/other"}), "app/other outside its scheduled ownership")
}
