package runnables

import (
	"testing"

	basev1 "github.com/codefly-dev/core/generated/go/codefly/base/v1"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

func TestInstallationSelectionFailsClosedAndDoesNotMutateCatalog(t *testing.T) {
	service := &resources.Service{WorkspaceConfigurationDependencies: []string{RuntimeExecutionGroup, RunnableBindingsGroup}}
	service.WithModule("runtime")
	info := &basev1.ConfigurationInformation{Name: RuntimeExecutionGroup, ConfigurationValues: []*basev1.ConfigurationValue{{Key: "INSTALLATION_JSON"}}}
	bindings := &basev1.ConfigurationInformation{Name: RunnableBindingsGroup, ConfigurationValues: []*basev1.ConfigurationValue{{Key: "READ"}, {Key: "ADMIN"}}}
	confs := []*basev1.Configuration{{Infos: []*basev1.ConfigurationInformation{info, bindings}}}
	for _, raw := range []string{`null`, `{`, `{"contract":"unknown"}`, `{"contract":"runtime.installation/v1","bindings":"all"}`, `[{"contract":"runtime.installation/v1"},null]`} {
		info.ConfigurationValues[0].Value = raw
		_, err := PreparedSelectionFor(service, confs)
		require.Error(t, err, raw)
	}
	info.ConfigurationValues[0].Value = `{"contract":"runtime.installation/v1","name":"release","bindings":[{"prepared":"READ"},{"call":{}}]}`
	keys, err := PreparedSelectionFor(service, confs)
	require.NoError(t, err)
	selected := SelectPreparedConfigurations(confs, keys)
	require.Len(t, selected[0].Infos[1].ConfigurationValues, 1)
	require.Len(t, bindings.ConfigurationValues, 2, "another consumer still receives the original catalog")
	info.ConfigurationValues[0].Value = `[]`
	keys, err = PreparedSelectionFor(service, confs)
	require.NoError(t, err)
	require.NotNil(t, keys)
	require.Empty(t, keys)
}
