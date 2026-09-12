package orchestration

import (
	"slices"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

// A derived value must arrive on the carrier a service reads by contract:
// resources.ConfigurationAsEnvironmentVariables turns a workspace-origin
// configuration into CODEFLY__WORKSPACE_CONFIGURATION__<GROUP>__<KEY>, which is
// what the SDK's WorkspaceValue resolves. Delivering it any other way leaves the
// service depending on an incidental raw-environment fallback.
func TestApplyWorkspaceConfigurationValuesEmitsTheDeclaredCarrier(t *testing.T) {
	world := &World{workspaceConfigurationValues: map[string]map[string]string{
		"federation": {"MODULE_REGISTRATION_SECRETS": "documents:deadbeef"},
	}}

	got := world.applyWorkspaceConfigurationValues(nil, []string{"federation"})
	if len(got) != 1 {
		t.Fatalf("expected one configuration, got %d", len(got))
	}
	envs := resources.EnvironmentVariableAsStrings(
		resources.ConfigurationAsEnvironmentVariables(got[0], false))
	want := "CODEFLY__WORKSPACE_CONFIGURATION__FEDERATION__MODULE_REGISTRATION_SECRETS=documents:deadbeef"
	if len(envs) != 1 || envs[0] != want {
		t.Errorf("emitted %v, want [%s]", envs, want)
	}
}

// A composition that ships the group with an empty placeholder (the shape
// module-saas-starter's federation.env has) must receive the derived value in
// place of it, not alongside it: two entries for one key leave which wins to
// map iteration order.
func TestApplyWorkspaceConfigurationValuesReplacesADeclaredKey(t *testing.T) {
	declared := &basev0.Configuration{
		Origin: resources.ConfigurationWorkspace,
		Infos: []*basev0.ConfigurationInformation{{
			Name:                "federation",
			ConfigurationValues: []*basev0.ConfigurationValue{{Key: "MODULE_REGISTRATION_SECRETS", Value: ""}},
		}},
	}
	world := &World{workspaceConfigurationValues: map[string]map[string]string{
		"federation": {"MODULE_REGISTRATION_SECRETS": "documents:deadbeef"},
	}}

	got := world.applyWorkspaceConfigurationValues([]*basev0.Configuration{declared}, []string{"federation"})
	if len(got) != 1 {
		t.Fatalf("expected the declared configuration to be reused, got %d", len(got))
	}
	values := got[0].Infos[0].ConfigurationValues
	if len(values) != 1 || values[0].Value != "documents:deadbeef" {
		t.Errorf("declared key not replaced in place: %v", values)
	}
}

// A run derives more than one value for a group — the federation group carries a
// registration digest and an identity digest — and both must arrive whether the
// composition declared a placeholder for them or not. A host declaring both keys
// takes the replace path twice; one still carrying only the older key takes
// replace then append. Both compositions are live, so both shapes are pinned
// here, and a key the run derives nothing for must survive either way.
func TestApplyWorkspaceConfigurationValuesEmitsEveryDerivedKeyInAGroup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared []*basev0.ConfigurationValue
	}{
		{
			name: "composition declares both placeholders",
			declared: []*basev0.ConfigurationValue{
				{Key: "MODULE_REGISTRATION_SECRETS", Value: ""},
				{Key: "MODULE_IDENTITY_SECRETS", Value: ""},
				{Key: "SOLUTION_REGISTRATION_SECRETS", Value: "solution:cafe"},
			},
		},
		{
			name: "composition predates the identity key",
			declared: []*basev0.ConfigurationValue{
				{Key: "MODULE_REGISTRATION_SECRETS", Value: ""},
				{Key: "SOLUTION_REGISTRATION_SECRETS", Value: "solution:cafe"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declared := &basev0.Configuration{
				Origin: resources.ConfigurationWorkspace,
				Infos: []*basev0.ConfigurationInformation{{
					Name:                "federation",
					ConfigurationValues: tc.declared,
				}},
			}
			world := &World{workspaceConfigurationValues: map[string]map[string]string{
				"federation": {
					"MODULE_REGISTRATION_SECRETS": "documents:aaaa",
					"MODULE_IDENTITY_SECRETS":     "documents:bbbb",
				},
			}}

			got := world.applyWorkspaceConfigurationValues([]*basev0.Configuration{declared}, []string{"federation"})
			if len(got) != 1 {
				t.Fatalf("expected the declared configuration to be reused, got %d", len(got))
			}
			envs := resources.EnvironmentVariableAsStrings(
				resources.ConfigurationAsEnvironmentVariables(got[0], false))
			for _, want := range []string{
				"CODEFLY__WORKSPACE_CONFIGURATION__FEDERATION__MODULE_REGISTRATION_SECRETS=documents:aaaa",
				"CODEFLY__WORKSPACE_CONFIGURATION__FEDERATION__MODULE_IDENTITY_SECRETS=documents:bbbb",
				"CODEFLY__WORKSPACE_CONFIGURATION__FEDERATION__SOLUTION_REGISTRATION_SECRETS=solution:cafe",
			} {
				if !slices.Contains(envs, want) {
					t.Errorf("emitted %v, missing %s", envs, want)
				}
			}
			// A replaced key must not also be appended: two entries for one key
			// leave which wins to map iteration order.
			if len(envs) != 3 {
				t.Errorf("emitted %d values, want exactly 3: %v", len(envs), envs)
			}
		})
	}
}

// The value reaches only services that declare the group. A service that does
// not depend on federation must not receive the composition's registration
// policy just because the run derived one.
func TestApplyWorkspaceConfigurationValuesSkipsUndeclaredGroups(t *testing.T) {
	world := &World{workspaceConfigurationValues: map[string]map[string]string{
		"federation": {"MODULE_REGISTRATION_SECRETS": "documents:deadbeef"},
	}}

	if got := world.applyWorkspaceConfigurationValues(nil, []string{"identity"}); len(got) != 0 {
		t.Errorf("value leaked to a service declaring only %q: %+v", "identity", got)
	}
	if got := world.applyWorkspaceConfigurationValues(nil, nil); len(got) != 0 {
		t.Errorf("value leaked to a service declaring nothing: %+v", got)
	}
}

// A run that derives nothing must leave the resolved set byte-identical.
func TestApplyWorkspaceConfigurationValuesNoOpsWithoutValues(t *testing.T) {
	declared := []*basev0.Configuration{{Origin: resources.ConfigurationWorkspace}}
	world := &World{}
	got := world.applyWorkspaceConfigurationValues(declared, []string{"federation"})
	if len(got) != 1 || got[0] != declared[0] {
		t.Errorf("no-op run altered the resolved configurations: %+v", got)
	}
}
