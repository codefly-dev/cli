package orchestration

import (
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
