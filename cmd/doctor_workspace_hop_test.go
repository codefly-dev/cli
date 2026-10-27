package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/composition"
	"github.com/stretchr/testify/require"
)

// Like a records composition, the routed module authors its authority contract
// and publishes an API catalog. Only the authority's hop consumes hop-routes;
// no workspace or module ships an authored route table.
func doctorHopWorkspace(t *testing.T) string {
	t.Helper()
	const doc = `{"openapi":"3.0.0","info":{"title":"records","version":"1.0.0"},"paths":{"/records":{"get":{"responses":{"200":{"description":"ok"}}}}}}`
	const contractPath = "contracts/api/records.json"
	digest := composition.APIContractDigest([]byte(doc))
	catalog, err := json.Marshal(composition.APIContractCatalog{
		Schema: composition.APIContractCatalogSchema, Package: "owner/records", Version: "1.0.0",
		Endpoints: []composition.APIContractEndpoint{{Service: "api", Endpoint: "web", API: "http", Kind: composition.APIContractKindOpenAPI, Package: "api", Path: contractPath, Digest: digest, Routes: []composition.APIContractRoute{{Method: "GET", Path: "/records"}}}},
	})
	require.NoError(t, err)
	return writeTestWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                                    "name: routes\nlayout: modules\nmodules:\n - name: records\n - name: authority\n",
		"configurations/local/hop.env":                              "MODULES=records\n",
		"modules/records/module.codefly.yaml":                       "name: records\nservices:\n - name: api\n",
		"modules/records/module.contract.codefly.yaml":              "schema: codefly/module-contract/v1\nprincipal: records\nnamespaces: []\nqueues: []\nscope_ceilings: []\nbindings: []\n",
		"modules/records/services/api/service.codefly.yaml":         testServiceYAML("api") + "endpoints:\n - name: web\n   api: http\n   visibility: public\n   exposure: none\n",
		"modules/records/" + contractPath:                           doc,
		"modules/records/" + composition.APIContractCatalogFileName: string(catalog),
		"modules/records/module.package.codefly.yaml":               "schema: codefly/module-package/v2\nkind: module-package\nid: owner/records\nversion: 1.0.0\nminimum-codefly-version: '>=0.3.32 <1.0.0'\nartifact-roots: [contracts, services]\ncontracts: {composition: '>=2.0.0 <3.0.0'}\nservices:\n - name: api\n   endpoints: [web]\n   api-contracts:\n    - endpoint: web\n      kind: openapi\n      package: api\n      path: " + contractPath + "\n      digest: " + digest + "\n",
		"modules/authority/module.codefly.yaml":                     "name: authority\nservices:\n - name: hop\n",
		"modules/authority/services/hop/service.codefly.yaml":       testServiceYAML("hop", "hop-routes"),
	})
}

func TestDoctorWorkspaceRecognizesGeneratedHopRoutes(t *testing.T) {
	for _, source := range []string{"workspace", "composed module"} {
		t.Run(source, func(t *testing.T) {
			dir := doctorHopWorkspace(t)
			if source == "composed module" {
				moduleConfigs := filepath.Join(dir, "modules/authority/configurations/local")
				require.NoError(t, os.MkdirAll(moduleConfigs, 0700))
				require.NoError(t, os.Rename(filepath.Join(dir, "configurations/local/hop.env"), filepath.Join(moduleConfigs, "hop.env")))
				require.NoError(t, os.Remove(filepath.Join(dir, "configurations/local")))
				// A composed module ships configurations from its own workspace.
				authority := filepath.Join(t.TempDir(), "authority")
				require.NoError(t, os.Rename(filepath.Join(dir, "modules/authority"), authority))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "workspace.codefly.yaml"), []byte("name: routes\nlayout: modules\nmodules:\n - name: records\n - name: authority\n   path: "+authority+"\n"), 0600))
			}
			before := snapshotTree(t, dir)
			for _, opts := range []workspaceReadinessOptions{
				{dir: dir}, {dir: dir, module: "records"}, {dir: dir, module: "authority"}, {dir: dir, service: "authority/hop"},
			} {
				report := runReadiness(t, opts)
				require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
				requireNoCode(t, report, codeConfigurationMissing)
				requireNoCode(t, report, codeConfigurationDirMissing)
				require.Equal(t, "ok", findCheck(report, "configuration references").Status)
			}
			require.Equal(t, before, snapshotTree(t, dir), "doctor must not materialize generated routes")
		})
	}
}

// A composition that names no module for the hop is READY: the declaring
// service receives hop-routes with an empty ROUTES, which the doctor reads
// from the same resolved inventory the run resolves (checkConfigurationSources
// takes generated groups from there, "never from a name exemption"), so
// nothing reports the group missing.
//
// This replaces a baselined refusal: `{"no selected modules", …,
// codeConfigurationMissing, "hop-routes"}` in the table below asserted that
// this composition fails the doctor. That refusal was the symptom of the
// delivery gap, not a rule — a host that stands a hop only on some
// compositions (saas's gateway on a cell) declares the group always and must
// start where it stands no hop — so the case is replaced by this one rather
// than kept green.
func TestDoctorWorkspaceAcceptsAHopThatSelectsNoModule(t *testing.T) {
	dir := doctorHopWorkspace(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "configurations/local/hop.env")))
	before := snapshotTree(t, dir)
	for _, opts := range []workspaceReadinessOptions{{dir: dir}, {dir: dir, service: "authority/hop"}} {
		report := runReadiness(t, opts)
		require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
		requireNoCode(t, report, codeConfigurationMissing)
		requireNoCode(t, report, codeConfigurationDirMissing)
	}
	require.Equal(t, before, snapshotTree(t, dir), "doctor must not materialize an empty route table")
}

func TestDoctorWorkspaceStillRefusesInvalidHopInputs(t *testing.T) {
	for _, tc := range []struct{ name, path, content, code, reason string }{
		{"missing catalog", "modules/records/" + composition.APIContractCatalogFileName, "", codeConfigurationInvalid, "not checked"},
		{"bad principal", "modules/records/module.contract.codefly.yaml", "schema: codefly/module-contract/v1\nprincipal: other\n", codeConfigurationInvalid, "principal"},
		{"authored routes", "configurations/local/hop-routes.env", "ROUTES={}\n", codeConfigurationInvalid, "remove its authored configuration"},
		{"private endpoint", "modules/records/services/api/service.codefly.yaml", testServiceYAML("api") + "endpoints:\n - name: web\n   api: http\n   visibility: private\n", codeConfigurationReference, "private"},
		{"missing ordinary group", "modules/authority/services/hop/service.codefly.yaml", testServiceYAML("hop", "hop-routes", "required-settings"), codeConfigurationMissing, "required-settings"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := doctorHopWorkspace(t)
			path := filepath.Join(dir, tc.path)
			if tc.content == "" {
				require.NoError(t, os.Remove(path))
			} else {
				require.NoError(t, os.WriteFile(path, []byte(tc.content), 0600))
			}
			report := runReadiness(t, workspaceReadinessOptions{dir: dir})
			require.Equal(t, readinessStatusNotReady, report.Status, reportJSON(t, report))
			diagnostic := requireCode(t, report, tc.code, "fail")
			require.True(t, strings.Contains(diagnostic.Message, tc.reason), diagnostic.Message)
		})
	}
}
