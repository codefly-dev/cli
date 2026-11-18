package orchestration

import (
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/sdk/session"
)

// sessionScopeIdentity is how much of a session's identity its scope carries
// (session.Session.Scope).
const sessionScopeIdentity = 12

// DeclareSessionScope declares env's state disposable when its naming scope
// was generated for this run by the SDK session that started this process, and
// reports whether it did.
//
// A test's dependency stack never gets a scope from IsolateEnvironment: the
// SDK generates one from its session's random identity, keeps the caller's
// label in front of it, and passes it as an explicit --naming-scope. That
// scope is as unrepeatable as a generated invocation identity, and its
// generator is the session, so the session is what proves it: the scope ends
// with the identity of the session this process was started under. Nothing
// else is evidence. A reusable session (sdk.WithKeepRunning) runs under the
// developer's label alone, and a scope a person typed is theirs; both stay a
// developer's state, which is never disposable.
func DeclareSessionScope(env *environments.Environment, owner *session.Session) bool {
	if env == nil || owner == nil || env.NamingScope == "" {
		return false
	}
	// An identity too short to carry a scope was not minted by the SDK.
	if len(owner.ID) < sessionScopeIdentity {
		return false
	}
	generated := owner.Scope()
	if env.NamingScope != generated && !strings.HasSuffix(env.NamingScope, "-"+generated) {
		return false
	}
	env.Disposable = true
	return true
}
