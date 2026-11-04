package orchestration

import (
	"fmt"
	"strings"

	"github.com/codefly-dev/core/services"
)

// An agent loads the service it serves by directory. Before core v0.19.7 it
// bound the service's interface dependencies itself, against the workspace at
// the identity's WorkspacePath — which it cannot compose when that workspace
// imports another by source and version: only the host registers a workspace
// resolver (cmd/common/context.go), and the agent is another process. Since
// core v0.19.7 the host hands the agent the binding its composition made
// (ServiceIdentity.interface_bindings, filled by core's services.Load from
// Service.InterfaceBindingsProto) and the agent never loads the workspace.
//
// An agent built on an older core ignores the field, loads the workspace, and
// fails with core's "requires host resolution" — a sentence about the host,
// from a process that is not the host, which has sent every operator to an
// uncommitted `path:` override on the import. The override does not fix the
// agent; it hides which one is behind. This names it.
const hostResolutionRefusal = "requires host resolution"

// explainHostResolutionRefusal returns err as the agent reported it unless it
// is the refusal above for a service whose binding this host supplied, in
// which case the returned error names the agent, what it did instead of
// reading the binding, and the release that reads it. The agent's own error
// stays wrapped underneath.
func explainHostResolutionRefusal(err error, instance *services.Instance) error {
	if err == nil || instance == nil || instance.Service == nil {
		return err
	}
	if !strings.Contains(err.Error(), hostResolutionRefusal) {
		return err
	}
	if instance.Service.InterfaceBindingsProto() == nil {
		// Nothing bound this service — it was loaded outside its composition —
		// so the agent was right to bind it against the workspace on disk.
		return err
	}
	agent := "its agent"
	switch {
	case instance.Agent != nil && instance.Agent.Agent != nil:
		agent = "agent " + instance.Agent.Agent.Identifier()
	case instance.Service.Agent != nil:
		agent = "agent " + instance.Service.Agent.Identifier()
	}
	return fmt.Errorf("%s binds %s's interface dependencies against the workspace on disk instead of reading the binding this host supplied (ServiceIdentity.interface_bindings), so it cannot compose a workspace imported by source and version; the agent was built on a core older than v0.19.7, which reads the binding. Upgrade the agent to a release built on core v0.19.7 or newer; a path: override on the import hides this without fixing it: %w", agent, instance.Unique(), err)
}
