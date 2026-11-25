package deployments

import (
	"fmt"
	"slices"

	"github.com/codefly-dev/core/agents/contract"
	agentv1 "github.com/codefly-dev/core/generated/go/codefly/services/agent/v1"
	builderv1 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v1"
	"github.com/codefly-dev/core/resources"
)

// CompositionProvenance projects the loaded composition's membership, including
// imported names, roles and owners. It never discovers a workspace from a service
// directory or infers membership from the subset being deployed.
func CompositionProvenance(workspace *resources.Workspace) (*builderv1.CompositionProvenance, error) {
	if workspace == nil {
		return nil, fmt.Errorf("%w: deployment requires the loaded composition", resources.ErrUnjudgedProvenance)
	}
	provenance := &builderv1.CompositionProvenance{}
	for _, member := range workspace.Members() {
		var role builderv1.CompositionMember_Role
		switch member.Role {
		case resources.MemberRoleModule:
			role = builderv1.CompositionMember_ROLE_MODULE
		case resources.MemberRoleSolution:
			role = builderv1.CompositionMember_ROLE_SOLUTION
		default:
			return nil, fmt.Errorf("%w: composition member %q has unknown role %q", resources.ErrUnjudgedProvenance, member.Name, member.Role)
		}
		provenance.Members = append(provenance.Members, &builderv1.CompositionMember{Name: member.Name, Role: role, Workspace: member.Workspace})
	}
	return provenance, nil
}

// RequireCompositionProvenance checks the running peer's adoption, not the Core
// version linked into the CLI. Older agents must not silently ignore membership.
func RequireCompositionProvenance(info *agentv1.AgentInformation) error {
	if !slices.Contains(info.GetContract().GetCapabilities(), contract.DeploymentCompositionProvenance) {
		return fmt.Errorf("deployment requires agent capability %s; upgrade the builder agent to one that judges request composition provenance", contract.DeploymentCompositionProvenance)
	}
	return nil
}
