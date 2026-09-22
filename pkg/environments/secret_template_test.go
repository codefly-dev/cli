package environments

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecretTemplateAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*EnvironmentServiceSecretMapping)
	}{
		{"unsupported engine", func(m *EnvironmentServiceSecretMapping) { m.Template.EngineVersion = "v1" }},
		{"replace drops other keys", func(m *EnvironmentServiceSecretMapping) { m.Template.MergePolicy = "Replace" }},
		{"missing source", func(m *EnvironmentServiceSecretMapping) { m.RemoteKeys = nil }},
		{"empty expression", func(m *EnvironmentServiceSecretMapping) { m.Template.Data["TOKEN"] = "" }},
		{"missing data", func(m *EnvironmentServiceSecretMapping) { m.Template.Data = nil }},
		{"invalid interval", func(m *EnvironmentServiceSecretMapping) { m.RefreshInterval = "soon" }},
		{"disabled rotation", func(m *EnvironmentServiceSecretMapping) { m.RefreshInterval = "0s" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mapping := EnvironmentServiceSecretMapping{
				RemoteKeys:      map[string]EnvironmentSecretRemoteRef{"TOKEN": {Key: "app/token"}},
				Template:        &EnvironmentSecretTemplate{EngineVersion: "v2", MergePolicy: "Merge", Data: map[string]string{"TOKEN": "{{ .TOKEN | sha256sum }}"}},
				RefreshInterval: "1m",
			}
			secrets := &EnvironmentServiceSecrets{
				SecretStore: EnvironmentSecretStoreReference{Name: "app", Kind: "SecretStore"},
				Services:    map[string]EnvironmentServiceSecretMapping{"api": mapping},
			}
			require.NoError(t, secrets.Validate())
			test.modify(&mapping)
			secrets.Services["api"] = mapping
			require.Error(t, secrets.Validate())
		})
	}
}
