package orchestration

import (
	"testing"

	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/stretchr/testify/require"
)

func TestRestartActionType(t *testing.T) {
	// A watch-triggered restart (START) must re-enter at configure (Init) so a
	// fresh compiler error gets re-attempted rather than relaunching the stale
	// binary.
	require.Equal(t, RuntimeInit, restartActionType(runtimev0.DesiredState_START))
	require.Equal(t, RuntimeInit, restartActionType(runtimev0.DesiredState_INIT))
	require.Equal(t, RuntimeLoad, restartActionType(runtimev0.DesiredState_LOAD))
}
