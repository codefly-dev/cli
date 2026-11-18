package run

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/sdk/session"
	"github.com/stretchr/testify/require"
)

// A test's dependency stack is started by the SDK, which generates the run's
// naming scope from its own session and passes it with --naming-scope. That
// run is as throwaway as one whose scope this CLI generated, and it is the
// only run a test harness ever starts: unless it is declared disposable, its
// agents keep its state for good and refuse it what only a disposable run is
// given.
func TestRunUnderTheScopeItsSDKSessionGeneratedIsDisposable(t *testing.T) {
	useRunEnvironment(t, orchestration.LocalEnvironmentName)
	workspace := loadWorkspaceFixture(t, declaredLocalWorkspace)
	owner, err := session.New()
	require.NoError(t, err)
	previousScope, previousExplicit := namingScope, namingScopeExplicit
	t.Cleanup(func() { namingScope, namingScopeExplicit = previousScope, previousExplicit })

	for _, tc := range []struct {
		name       string
		scope      string
		explicit   bool
		session    *session.Session
		disposable bool
	}{
		{"the session's scope behind the caller's label", "suite-" + owner.Scope(), true, owner, true},
		{"the session's scope alone", owner.Scope(), true, owner, true},
		// A reusable session (sdk.WithKeepRunning) runs under the developer's
		// label: a warm stack keeps the state it had.
		{"a label the caller named, inside a session", "suite", true, owner, false},
		{"another session's scope", "suite-s0123456789ab", true, owner, false},
		{"the same text with no session", "suite-" + owner.Scope(), true, nil, false},
		{"a session identity too short to be the SDK's", "suite-sabc", true, &session.Session{ID: "abc", Secret: "secret"}, false},
		{"a declared scope, inside a session", "", false, owner, false},
		{"an explicitly empty scope, inside a session", "", true, owner, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(session.IDEnvironment, "")
			t.Setenv(session.SecretEnvironment, "")
			if tc.session != nil {
				t.Setenv(session.IDEnvironment, tc.session.ID)
				t.Setenv(session.SecretEnvironment, tc.session.Secret)
			}
			namingScope, namingScopeExplicit = tc.scope, tc.explicit

			env, err := runEnvironment(workspace)
			require.NoError(t, err)
			require.Equal(t, tc.disposable, env.Disposable)
			require.False(t, workspace.FindEnvironment("local").Disposable, "the declaration is never marked")
		})
	}
}
