package gitops

import (
	"context"
	"fmt"

	"github.com/codefly-dev/authority/libraries/authority-core/go/declare"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
)

// ModuleBindings is one composed module's contract bindings as a render for an
// environment resolves them: the ones its delivered manifest carries, and the
// optional ones the composition fills no slot of, which it leaves out.
type ModuleBindings struct {
	Module    string
	Delivered []string
	Omitted   []modulecontract.OmittedBinding
	// Refused is the reason the render would refuse the module's request in
	// this environment, when it would.
	Refused error
}

// ResolveModuleBindings resolves every composed module's request for
// authority against env's configuration, through the one render every
// producer calls (declare.Module over resolveContract, as moduleAuthority
// does), and reports which bindings are delivered and which are omitted. A
// module that asks for no authority is not listed. Two environments compared
// through it disagree exactly where their renders would.
func ResolveModuleBindings(ctx context.Context, workspace *resources.Workspace, env *environments.Environment) ([]ModuleBindings, error) {
	provided, err := configurations.ReadWorkspaceConfigurations(ctx, workspace, env.Runtime(), orchestration.ModuleArtifactResolverFor(workspace))
	if err != nil {
		return nil, fmt.Errorf("read the workspace configurations of environment %s: %w", env.Name, err)
	}
	var out []ModuleBindings
	for _, ref := range workspace.Modules {
		module, err := workspace.LoadModuleFromName(ctx, ref.Name)
		if err != nil {
			out = append(out, ModuleBindings{Module: ref.Name, Refused: err})
			continue
		}
		request, err := authorityRequestOf(module)
		if err != nil {
			out = append(out, ModuleBindings{Module: ref.Name, Refused: err})
			continue
		}
		if request.undeclared != "" {
			continue
		}
		_, resolved, err := declare.Module(module.Dir(), module.Name, request.contract, func(contract *modulecontract.Contract) (*modulecontract.Resolved, error) {
			return resolveContract(ctx, workspace, env, provided, contract)
		})
		switch {
		case err != nil:
			out = append(out, ModuleBindings{Module: ref.Name, Refused: err})
		case resolved != nil:
			bindings := ModuleBindings{Module: ref.Name, Omitted: resolved.Omitted}
			for _, binding := range resolved.Bindings {
				bindings.Delivered = append(bindings.Delivered, binding.ID)
			}
			out = append(out, bindings)
		}
	}
	return out, nil
}
