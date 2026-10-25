package gitops

import (
	"context"
	"errors"
	"fmt"

	"github.com/codefly-dev/core/wool"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
	delivery "github.com/codefly-dev/authority/libraries/delivery/go"
	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/resources"
)

// ValidateMethodPolicies validates the exact published inputs used by delivery.
// An uncovered business role is not malformed policy.
func ValidateMethodPolicies(methods []*compositionv1.MethodPolicyV1) error {
	var problems []error
	for _, m := range methods {
		requires := map[string][]string{}
		for _, sc := range m.GetRequires() {
			requires[sc.GetResourceKind()] = append(requires[sc.GetResourceKind()], sc.GetActions()...)
		}
		if err := delivery.ValidateMethodPolicy(m.GetMethod(), m.GetAnyCheckedCaller(), requires, true); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// ValidateModulePermissions runs before render/build/publication effects. Hop
// warning mode never changes structural validity. Contract.Resolve remains the
// owner of binding/callee checks; SolutionRoles owns role declaration checks.
func ValidateModulePermissions(ctx context.Context, workspace *resources.Workspace, module *resources.Module, env *environments.Environment) error {
	methods, _, err := MethodPolicies(module.Dir())
	if err != nil {
		return err
	}
	if err = ValidateMethodPolicies(methods); err != nil {
		return fmt.Errorf("module %s permissions: %w", module.Name, err)
	}
	request, err := authorityRequestOf(module)
	if err != nil {
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
		var err error
		selected, err = workspace.ResolveModuleClosure(ctx, resources.StageRun, seeds)
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
