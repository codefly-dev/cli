package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

func TestConfigureServicePersistsPythonEnvironmentForNextTestRun(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"pyproject.toml": `[project]
name = "runtime-recovery"
version = "0.0.0"
`,
		filepath.Join(".github", "workflows", "test.yml"): `jobs:
  test:
    steps:
      - run: pytest -v
`,
		"test_environment.py": `import os
import unittest

class EnvironmentTest(unittest.TestCase):
    def test_configured_value_reaches_runtime(self):
        self.assertEqual(os.environ.get("RECOVERY_FLAG"), "enabled")
`,
	}
	for relative, body := range files {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	configured, err := server.ConfigureService(t.Context(), &gatewayv1.ConfigureServiceRequest{
		Changes: []*builderv0.ConfigChange{
			{Path: "test.env.RECOVERY_FLAG", Value: "enabled", Op: builderv0.ConfigChange_SET},
			{Path: "test.provisioning.editable", Value: "false", Op: builderv0.ConfigChange_SET},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := configured.GetResponse()
	if response.GetState().GetState() != builderv0.ConfigureStatus_SUCCESS {
		t.Fatalf("configure status = %s (%s)", response.GetState().GetState(), response.GetState().GetMessage())
	}
	if !strings.Contains(response.GetEffectiveYaml(), "RECOVERY_FLAG: enabled") {
		t.Fatalf("effective configuration omitted persisted environment: %s", response.GetEffectiveYaml())
	}
	reset, err := server.ConfigureService(t.Context(), &gatewayv1.ConfigureServiceRequest{
		Changes: []*builderv0.ConfigChange{{
			Path: "test.provisioning.editable", Op: builderv0.ConfigChange_UNSET,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resetResponse := reset.GetResponse()
	if resetResponse.GetState().GetState() != builderv0.ConfigureStatus_SUCCESS {
		t.Fatalf("reset status = %s (%s)", resetResponse.GetState().GetState(), resetResponse.GetState().GetMessage())
	}
	if strings.Contains(resetResponse.GetEffectiveYaml(), "editable:") {
		t.Fatalf("reset left the explicit editable override in place: %s", resetResponse.GetEffectiveYaml())
	}
	if !strings.Contains(resetResponse.GetEffectiveYaml(), "RECOVERY_FLAG: enabled") {
		t.Fatalf("reset discarded an unrelated persisted setting: %s", resetResponse.GetEffectiveYaml())
	}

	tested, err := server.Test(t.Context(), &gatewayv1.TestRequest{RuntimeRequest: &runtimev0.TestRequest{
		Target: "test_environment.py::EnvironmentTest::test_configured_value_reaches_runtime",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result := tested.GetRuntimeResponse().GetResult(); result.GetState() != runtimev0.TestRunResult_PASSED {
		t.Fatalf("test result = %s (%s)\noutput:\n%s\nfailures: %v", result.GetState(), result.GetMessage(), tested.GetOutput(), tested.GetFailures())
	}
	if tested.GetTestsPassed() != 1 {
		t.Fatalf("passed tests = %d, want 1", tested.GetTestsPassed())
	}
}
