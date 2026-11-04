package cmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/fixturefragments"
	"github.com/stretchr/testify/require"
)

const fixtureFragmentHead = "schema: codefly.dev/authority/fixture-composition/v1\n"

// twoFragmentWorkspace composes two modules from OUTSIDE the workspace tree —
// as a pinned module materializes, which is what platform-core does — each
// shipping a fixture-composition fragment, which is what every module composed
// since cli#1197 does, plus a service that declares the group as the local
// authority does. An in-repo module's configurations belong to the consuming
// workspace root, so only a foreign module can offer a group at all.
func twoFragmentWorkspace(t *testing.T, extra map[string]string) string {
	t.Helper()
	authority, documents := filepath.Join(t.TempDir(), "authority"), filepath.Join(t.TempDir(), "documents")
	files := map[string]string{
		"workspace.codefly.yaml": "name: two-fragments\nlayout: modules\nmodules:\n    - name: authority\n      path: " + authority +
			"\n    - name: documents\n      path: " + documents + "\nenvironments:\n    - name: local\n",
	}
	for rel, content := range map[string]string{
		"module.codefly.yaml":                  "kind: module\nname: authority\nservices:\n    - name: local\n",
		"services/local/service.codefly.yaml":  testServiceYAML("local", fixturefragments.Group),
		"configurations/local/" + fragmentFile: fixtureFragmentHead,
	} {
		writeFileUnder(t, authority, rel, content)
	}
	for rel, content := range map[string]string{
		"module.codefly.yaml":                     "kind: module\nname: documents\nservices:\n    - name: documents\n",
		"services/documents/service.codefly.yaml": testServiceYAML("documents"),
		"configurations/local/" + fragmentFile: fixtureFragmentHead + `roles:
  - organization: acme
    name: viewer
    scopes:
      - {kind: documents, actions: [read]}
`,
	} {
		writeFileUnder(t, documents, rel, content)
	}
	for k, v := range extra {
		switch {
		case strings.HasPrefix(k, "authority/"):
			writeFileUnder(t, authority, strings.TrimPrefix(k, "authority/"), v)
		case strings.HasPrefix(k, "documents/"):
			writeFileUnder(t, documents, strings.TrimPrefix(k, "documents/"), v)
		default:
			files[k] = v
		}
	}
	return writeTestWorkspace(t, files)
}

const fragmentFile = fixturefragments.Group + ".yaml"

func writeFileUnder(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
}

// Two composed modules shipping a fixture-composition fragment is the design,
// not a conflict: the run merges them. Readiness must agree with the run.
func TestDoctorWorkspaceAcceptsAFragmentFromEveryComposedModule(t *testing.T) {
	dir := twoFragmentWorkspace(t, nil)
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	require.Equal(t, readinessStatusReady, report.Status, reportJSON(t, report))
	requireNoCode(t, report, codeConfigurationDuplicate)
	requireNoCode(t, report, codeConfigurationMissing)
}

// Every other workspace configuration keeps the single-provider rule.
func TestDoctorWorkspaceStillRefusesTwoProvidersOfAnOrdinaryGroup(t *testing.T) {
	dir := twoFragmentWorkspace(t, map[string]string{
		"authority/services/local/service.codefly.yaml": testServiceYAML("local", fixturefragments.Group, "engine"),
		"authority/configurations/local/engine.env":     "MODE=fast\n",
		"documents/configurations/local/engine.env":     "MODE=slow\n",
	})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	require.Equal(t, readinessStatusNotReady, report.Status, reportJSON(t, report))
	d := requireCode(t, report, codeConfigurationDuplicate, "fail")
	require.True(t, strings.Contains(d.Message, "engine"), d.Message)
	require.False(t, strings.Contains(d.Message, fixturefragments.Group), d.Message)
}

// --json makes stdout the report and nothing else. Core logs the composed
// modules' disagreement over fixture-composition while reading the
// configurations, on a context with no logger of the CLI's, so the line takes
// wool's fallback; it belongs on stderr, where it stays visible.
func TestDoctorWorkspaceJSONKeepsStdoutForTheReport(t *testing.T) {
	t.Chdir(twoFragmentWorkspace(t, nil))
	stdout, stderr := os.Stdout, os.Stderr
	outRead, outWrite, err := os.Pipe()
	require.NoError(t, err)
	errRead, errWrite, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout, os.Stderr = outWrite, errWrite
	t.Cleanup(func() { os.Stdout, os.Stderr = stdout, stderr })
	require.NoError(t, DoctorWorkspaceCmd.Flags().Set("json", "true"))
	t.Cleanup(func() { doctorWorkspaceJSON = false; DoctorWorkspaceCmd.Flags().Lookup("json").Changed = false })
	DoctorWorkspaceCmd.SetContext(context.Background())

	runErr := DoctorWorkspaceCmd.RunE(DoctorWorkspaceCmd, nil)
	require.NoError(t, outWrite.Close())
	require.NoError(t, errWrite.Close())
	os.Stdout, os.Stderr = stdout, stderr
	emitted, err := io.ReadAll(outRead)
	require.NoError(t, err)
	logged, err := io.ReadAll(errRead)
	require.NoError(t, err)

	require.NoError(t, runErr, string(emitted))
	require.Contains(t, string(logged), "composed modules disagree", "the fixture no longer produces the warning this test is about")
	var report workspaceReadinessReport
	require.NoError(t, json.Unmarshal(emitted, &report), "stdout is not the report alone:\n%s", emitted)
	require.Equal(t, readinessStatusReady, report.Status)
}
