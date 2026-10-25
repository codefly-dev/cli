package gitops

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/codefly-dev/core/wool"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	delivery "github.com/codefly-dev/authority/libraries/delivery/go"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/configurations"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/codefly-dev/core/resources"
)

// ValidateMethodPolicies validates the exact published inputs used by delivery.
// Structure is checked wherever a policy is read: a method declaring both
// requirements and any checked caller, a requirement with no action, a kind or
// action outside the vocabulary. requireDeclaration also refuses a method that
// declares nothing, by name; it is set only where the policies are consumed
// (methodPoliciesRequired), since elsewhere such a method is passed through for
// the hop to refuse per call. An uncovered business role is not malformed
// policy.
func ValidateMethodPolicies(methods []*compositionv1.MethodPolicyV1, requireDeclaration bool) error {
	var problems []error
	for _, m := range methods {
		requires := map[string][]string{}
		for _, sc := range m.GetRequires() {
			requires[sc.GetResourceKind()] = append(requires[sc.GetResourceKind()], sc.GetActions()...)
		}
		if err := delivery.ValidateMethodPolicy(m.GetMethod(), m.GetAnyCheckedCaller(), requires, requireDeclaration); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// CompositionInterface is the Codefly interface a host declares when it
// receives manifests through ApplyManifest, method policies with them.
const CompositionInterface = "codefly.dev/composition"

// HostImplements reports whether the host's delivery endpoint — the endpoint
// host.delivery names — declares, in its module's interface endpoints, that it
// implements the named Codefly interface at any version. The endpoint is the
// one manifests are sent to, so another endpoint of the same service that
// implements the interface does not change how they are delivered: a host
// whose delivery is its signed REST route keeps receiving them there. A host
// module the workspace does not compose declares nothing here.
func HostImplements(ctx context.Context, workspace *resources.Workspace, hostModule, hostService, hostEndpoint, name string) bool {
	module, err := workspace.LoadModuleFromName(ctx, hostModule)
	if err != nil || module.Interface == nil {
		return false
	}
	for _, endpoint := range module.Interface.Endpoints {
		if endpoint.Service != hostService || endpoint.Endpoint != hostEndpoint {
			continue
		}
		for _, implemented := range endpoint.Implements {
			if implemented == name || strings.HasPrefix(implemented, name+"@") {
				return true
			}
		}
	}
	return false
}

// methodPoliciesRequired answers whether every method the module publishes
// must declare a policy before anything is rendered, and why. A policy is
// required where it is consumed: by a host implementing codefly.dev/composition,
// which receives the policies with each manifest, or wherever the module's own
// contract opts in (require_method_policies). Toward any other host an unmarked
// method passes, as MethodPolicies lists it: the hop refuses it per call.
//
// The host's own module — the module of the host.delivery service — is not
// behind the hop: its methods are the host's, never fronted by the policies it
// receives, so the host clause does not hold it (its own
// require_method_policies still does). Every other module in the composition
// is held. host.component names the host's instance, not a module, so it is
// never read here.
func methodPoliciesRequired(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, moduleName string, request authorityRequest) (string, bool) {
	if request.contract != nil && request.contract.RequireMethodPolicies {
		return "its " + modulecontract.FileName + " sets require_method_policies", true
	}
	if env != nil && env.Host != nil {
		hostModule, hostService, hostEndpoint := env.Host.DeliveryEndpoint()
		if hostModule != moduleName && HostImplements(ctx, workspace, hostModule, hostService, hostEndpoint, CompositionInterface) {
			return fmt.Sprintf("its policies are delivered to host %s/%s, which implements %s", hostModule, hostService, CompositionInterface), true
		}
	}
	return "", false
}

// ValidateModulePermissions runs before render/build/publication effects. Hop
// warning mode never changes structural validity. Contract.Resolve remains the
// owner of binding/callee checks; its Validate owns the roles a contract declares.
func ValidateModulePermissions(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment) error {
	return validateModulePermissions(ctx, workspace, module, env, nil)
}

// selectedSource is an explicit development deployment input, already resolved
// through loadDevService, which must be checked even atop a published package.
func validateModulePermissions(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment, selectedSource *resources.Service) error {
	methods, _, err := MethodPolicies(module.Dir())
	if err != nil {
		return err
	}
	request, err := authorityRequestOf(module)
	if err != nil {
		return err
	}
	reason, required := methodPoliciesRequired(ctx, workspace, env, module.Name, request)
	if err = ValidateMethodPolicies(methods, required); err != nil {
		if required {
			return fmt.Errorf("module %s permissions (every published method declares a policy: %s): %w", module.Name, reason, err)
		}
		return fmt.Errorf("module %s permissions: %w", module.Name, err)
	}
	if err = validatePermissionSource(ctx, workspace, module, selectedSource); err != nil {
		return err
	}
	services, err := loadModuleServices(ctx, module, env)
	if err != nil {
		return err
	}
	_, err = moduleAuthority(ctx, workspace, module, services, env, request)
	return err
}

// CheckCompositionPermissions collects all selected-module diagnostics. Local
// run adoption warns; deployment rejects the same diagnostics before effects.
func CheckCompositionPermissions(ctx context.Context, workspace *resources.Workspace, env *environments.Environment, strict bool, seeds ...string) error {
	var selected *resources.ModuleClosure
	if len(seeds) > 0 {
		provided, _, err := orchestration.WorkspaceConfigurationsForChecking(ctx, workspace, env)
		if err != nil {
			return err
		}
		var references map[string][]string
		if provided != nil {
			references = configurations.EndpointProducers(provided.Infos)
		}
		selected, err = workspace.ResolveModuleClosure(ctx, resources.StageRun, seeds, resources.WithModuleConfigurationReferences(references))
		if err != nil {
			return err
		}
	}
	var problems []error
	for _, ref := range workspace.Modules {
		if selected != nil && !selected.Contains(ref.Name) {
			continue
		}
		module, err := workspace.LoadModuleFromName(ctx, ref.Name)
		if err == nil {
			err = ValidateModulePermissions(ctx, workspace, module, env)
		}
		if err != nil {
			problems = append(problems, err)
		}
	}
	err := errors.Join(problems...)
	if err != nil && !strict {
		wool.Get(ctx).Warn("permission preflight: " + err.Error())
		return nil
	}
	return err
}

// PermissionPreflight wires composition validation without making the generic
// orchestration engine depend on the deployment implementation.
func PermissionPreflight(strict bool) func(context.Context, *resources.Workspace, *environments.Environment) error {
	return func(ctx context.Context, w *resources.Workspace, e *environments.Environment) error {
		return CheckCompositionPermissions(ctx, w, e, strict)
	}
}
