package solutionrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// frontendFixture composes one solution module whose entry service's
// directory holds the declared file when body is not empty.
func frontendFixture(t *testing.T, body string) (*resources.Module, []*resources.Service) {
	// The services are loaded so the fixture is the module a render holds;
	// the file is located through the module, never through them.
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		resources.WorkspaceConfigurationName:                                  "name: example\nlayout: modules\nmodules:\n  - name: wiki\n",
		"modules/wiki/" + resources.ModuleConfigurationName:                   "kind: module\nname: wiki\nservice-entry: backend\nservices:\n  - name: backend\n  - name: worker\n",
		"modules/wiki/services/backend/" + resources.ServiceConfigurationName: "kind: service\nname: backend\nversion: 0.0.0\nmodule: wiki\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n",
		"modules/wiki/services/worker/" + resources.ServiceConfigurationName:  "kind: service\nname: worker\nversion: 0.0.0\nmodule: wiki\nagent:\n  kind: runtime::service\n  name: go\n  version: 0.0.1\n  publisher: codefly.dev\n",
	}
	if body != "" {
		files["modules/wiki/services/backend/"+FrontendFileName] = body
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	workspace, err := resources.LoadWorkspaceFromDir(t.Context(), root)
	require.NoError(t, err)
	module, err := workspace.LoadModuleFromName(t.Context(), "wiki")
	require.NoError(t, err)
	var services []*resources.Service
	for _, reference := range module.ServiceReferences {
		service, err := module.LoadServiceFromName(t.Context(), reference.Name)
		require.NoError(t, err)
		services = append(services, service)
	}
	return module, services
}

// The declared file is read beside the entry service, field for field into
// core's block, nothing defaulted, with a dashboard whose whole numbers stay
// whole, as core's canonical encoding requires of them.
func TestDeclaredFrontendIsReadBesideTheEntryService(t *testing.T) {
	module, _ := frontendFixture(t, `title: Wiki
order: 10
exposed_module: ./Page
host_contract: 1
dashboard:
  rows: 3
  panels:
    - metric: pages
surfaces:
  - id: footnote
    client: word
    title: Footnote
    module: /surfaces/word/footnote.js
    contract: 1
    applies: always
    events:
      - document.opened
`)
	frontend, err := DeclaredFrontend(t.Context(), module)
	require.NoError(t, err)
	require.Equal(t, &solutionhost.Frontend{
		Title: "Wiki", Order: 10, ExposedModule: "./Page", HostContract: 1,
		Dashboard: map[string]any{"rows": 3, "panels": []any{map[string]any{"metric": "pages"}}},
		Surfaces:  []solutionhost.FrontendSurface{{ID: "footnote", Client: "word", Title: "Footnote", Module: "/surfaces/word/footnote.js", Contract: 1, Applies: "always", Events: []string{"document.opened"}}},
	}, frontend)
}

// What the file cannot be is refused naming the file: absent, empty, carrying
// a key core's block does not have, or two documents. A worker's directory
// is never where the file is looked for.
func TestDeclaredFrontendRefusesWhatTheRuntimeRefuses(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"absent", "", "declares no frontend"},
		{"empty", "\n", "is empty"},
		{"unknown key", "title: Wiki\norder: 1\nhost_contract: 1\ncontract: wiki\n", "field contract not found"},
		{"two documents", "title: Wiki\norder: 1\nhost_contract: 1\n---\ntitle: Other\n", "more than one YAML document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			module, _ := frontendFixture(t, tc.body)
			_, err := DeclaredFrontend(t.Context(), module)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, FrontendFileName)
		})
	}
	t.Run("beside the worker is not beside the entry", func(t *testing.T) {
		module, _ := frontendFixture(t, "")
		require.NoError(t, os.WriteFile(filepath.Join(module.ServicesDir(), "worker", FrontendFileName), []byte("title: Wiki\norder: 1\nhost_contract: 1\n"), 0o600))
		_, err := DeclaredFrontend(t.Context(), module)
		require.ErrorContains(t, err, "declares no frontend")
	})
	t.Run("no entry service", func(t *testing.T) {
		module, _ := frontendFixture(t, "title: Wiki\norder: 1\nhost_contract: 1\n")
		module.ServiceEntry = ""
		_, err := DeclaredFrontend(t.Context(), module)
		require.ErrorContains(t, err, "no service-entry")
	})
}
