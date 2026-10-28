package orchestration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/internal/protocoltest"
	"github.com/codefly-dev/core/agents/manager"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	runtimev0 "github.com/codefly-dev/core/generated/go/codefly/services/runtime/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/runners/recoveryscope"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func disposableCommandCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "codefly")
	if os.Getenv("CODEFLY_TEST_DISPOSABLE_ENGINE") == "1" {
		binary = filepath.Join(os.Getenv("CODEFLY_TEST_DISPOSABLE_EVIDENCE"), "codefly")
	}
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../cmd/codefly")
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	if os.Getenv("CODEFLY_TEST_DISPOSABLE_ENGINE") == "1" {
		data, readErr := os.ReadFile(binary)
		require.NoError(t, readErr)
		metadata, readErr := exec.CommandContext(t.Context(), "go", "version", "-m", binary).CombinedOutput()
		require.NoError(t, readErr)
		require.NoError(t, os.WriteFile(binary+".provenance.txt", append([]byte(fmt.Sprintf("sha256 %x\n", sha256.Sum256(data))), metadata...), 0600))
	}
	return binary
}

func disposableCommandWorkspace(t *testing.T) string {
	t.Helper()
	ws := writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml":                               "name: disposable-commands\nlayout: modules\nmodules:\n  - name: app\n",
		"modules/app/module.codefly.yaml":                      "name: app\nservices:\n  - name: subject\n  - name: dependency\n",
		"modules/app/services/subject/service.codefly.yaml":    disposableCommandDeclaration("subject") + "service-dependencies:\n  - name: dependency\n    kind: runtime\n",
		"modules/app/services/dependency/service.codefly.yaml": disposableCommandDeclaration("dependency"),
		"modules/app/services/subject/code/payload":            t.TempDir(),
		"modules/app/services/dependency/code/payload":         t.TempDir(),
	})
	return ws.Dir()
}

func disposableCommandDeclaration(name string) string {
	return "name: " + name + "\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: example.test\n  name: disposable-command\n  version: 0.0.1\n"
}

// Process-local fake engine for commands: reuse the maintained stub, extending
// only architecture and scoped digest responses. It has no real-engine fallback.
func disposableCommandDocker(t *testing.T) *dockerStub {
	t.Helper()
	stub := installDockerStub(t, true)
	path := filepath.Join(stub.dir, "docker")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	script := strings.Replace(string(data), `case "$1 $2" in`, `case "$1 $2" in
"info --format") printf 'arm64\n'; exit 0 ;;
"buildx inspect") printf 'running\n'; exit 0 ;;`, 1)
	script = strings.Replace(script, `printf '["codefly-local/runtime@sha256:%064d"]\n' 7`, `for arg in "$@"; do image="$arg"; done
    repo="${image%:*}"
    printf '["%s@sha256:%064d"]\n' "$repo" 7`, 1)
	require.NoError(t, os.WriteFile(path, []byte(script), 0700))
	return stub
}

func disposableCommand(t *testing.T, binary, root, phase string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	t.Cleanup(cancel)
	args := []string{"ci", "run", "--phase", phase, "--changed-file", "modules/app/services/subject/code/payload", "--disposable", "--runtime-context", "container", "--image-sbom", "--output", filepath.Join(root, ".reports")}
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = root
	// Copy only the installed test peer into a new home. Controllers, agent
	// registries and logs are never shared by concurrent command processes.
	agent := &resources.Agent{Kind: resources.ServiceAgent, Publisher: "example.test", Name: "disposable-command", Version: "0.0.1"}
	installed, err := agent.Path(t.Context())
	require.NoError(t, err)
	relative, err := filepath.Rel(resources.CodeflyHomeDir(), installed)
	require.NoError(t, err)
	home := t.TempDir()
	target := filepath.Join(home, relative)
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0700))
	require.NoError(t, os.Symlink(installed, target))
	command.Env = append(os.Environ(), resources.CodeflyHomeEnv+"="+home)
	command.Env = append(command.Env, extraEnv...)
	return command
}

func TestDisposableCommandsUseSelectedManagersAndArtifactBuilder(t *testing.T) {
	protocoltest.Install(t, "disposable-command")
	t.Setenv(manager.AgentSourceEnv, "local")
	t.Setenv(recoveryscope.EnvironmentVariable, "")
	t.Setenv("CODEFLY_TEST_PEER_IMAGES", "1")
	t.Setenv("CODEFLY_TEST_PEER_VALIDATION", "stack")
	binary := disposableCommandCLI(t)
	for _, phase := range []string{"lint", "compile", "test", "build", "build-error", "sbom-error"} {
		t.Run(phase, func(t *testing.T) {
			stub := disposableCommandDocker(t)
			root := disposableCommandWorkspace(t)
			actualPhase := phase
			var env []string
			if phase == "sbom-error" {
				actualPhase = "build"
				env = []string{"CODEFLY_TEST_PEER_SBOM_ERROR=1"}
			}
			if phase == "build-error" {
				actualPhase = "build"
				require.NoError(t, os.WriteFile(filepath.Join(stub.dir, "hook"), []byte("echo intentional-test-build-failure >&2; exit 96\n"), 0600))
			}
			output, err := disposableCommand(t, binary, root, actualPhase, env...).CombinedOutput()
			if strings.HasSuffix(phase, "-error") {
				require.Error(t, err)
				require.Contains(t, string(output), "intentional")
				require.Equal(t, 1, stub.calls(t, "build"))
				return
			}
			require.NoError(t, err, "%s", output)
			require.Positive(t, stub.calls(t, "build"))
			assertDisposableCommandDelivery(t, root, phase, false)
		})
	}
}

// Inspect actual recorded gRPC deliveries from command-created managers. Real
// engine qualification additionally compares the delivered subjects to Docker.
func assertDisposableCommandDelivery(t *testing.T, root, phase string, real bool) {
	t.Helper()
	for _, service := range []string{"subject", "dependency"} {
		if phase == "build" && service == "dependency" {
			continue
		}
		calls := protocoltest.Calls(t, filepath.Join(root, "modules/app/services", service))
		var images []string
		methods := map[string]bool{}
		for _, call := range calls {
			methods[call.Method] = true
			switch call.Method {
			case "Builder.EmittedPlan":
				plan := &builderv0.DockerBuildPlan{}
				require.NoError(t, protojson.Unmarshal(call.Request, plan))
				require.Len(t, plan.Recipes, 1)
				require.Equal(t, "codefly-dev/app/subject:0.0.0", plan.Recipes[0].Image)
			case "Runtime.Init":
				request := &runtimev0.InitRequest{}
				require.NoError(t, protojson.Unmarshal(call.Request, request))
				images = append(images, request.RuntimeImage)
			case "Builder.SBOM":
				request := &builderv0.SBOMRequest{}
				require.NoError(t, protojson.Unmarshal(call.Request, request))
				for _, subject := range request.Subjects {
					images = append(images, subject.Reference)
					if real {
						id, err := inspectLocalImageID(t.Context(), subject.Reference)
						require.NoError(t, err)
						require.Equal(t, id, subject.Digest)
						platform, err := exec.CommandContext(t.Context(), "docker", "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", subject.Reference).Output()
						require.NoError(t, err)
						require.Equal(t, strings.TrimSpace(string(platform)), subject.Platform)
					}
				}
			}
		}
		require.True(t, methods["Builder.Build"])
		if service == "subject" {
			expected := map[string]string{"lint": "Runtime.Lint", "compile": "Runtime.Build", "test": "Runtime.Test", "build": "Builder.SBOM"}[phase]
			require.True(t, methods[expected], "command must execute its operation, not skip")
		}
		require.NotEmpty(t, images, "%s/%s must reach actual image delivery", phase, service)
		for _, image := range images {
			require.Contains(t, image, "codefly-local/disposable/")
			if real {
				_, err := inspectLocalImageID(t.Context(), image)
				require.NoError(t, err)
			}
		}
		if real {
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CODEFLY_TEST_DISPOSABLE_EVIDENCE"), phase+"-"+service+"-delivered.txt"), []byte(strings.Join(images, "\n")), 0600))
		}
	}
}
