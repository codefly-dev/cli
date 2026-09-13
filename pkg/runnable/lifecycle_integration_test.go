//go:build integration

package runnable_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runnableops "github.com/codefly-dev/cli/pkg/runnable"
	"github.com/codefly-dev/core/agents/manager"
	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	builderv0 "github.com/codefly-dev/core/generated/go/codefly/services/builder/v0"
	"github.com/codefly-dev/core/resources"
	corerunnable "github.com/codefly-dev/core/runnable"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This qualification uses the real CLI and a separately built agent. The
// environment is required so an unqualified machine cannot silently skip it.
func TestCreateBuildAndInvokeRunnable(t *testing.T) {
	agentSource := os.Getenv("CODEFLY_RUNNABLE_AGENT_BINARY")
	require.NotEmpty(t, agentSource, "build runnable-python and set CODEFLY_RUNNABLE_AGENT_BINARY")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	root := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(root, "home"))
	t.Setenv(manager.AgentSourceEnv, "local")
	agent, err := resources.ParseAgent(ctx, resources.RunnableAgent, "codefly.dev/python:0.0.1")
	require.NoError(t, err)
	agentPath, err := agent.Path(ctx)
	require.NoError(t, err)
	binary, err := os.ReadFile(agentSource)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(agentPath), 0700))
	require.NoError(t, os.WriteFile(agentPath, binary, 0700))

	cliPath := filepath.Join(root, "codefly")
	compile := exec.CommandContext(ctx, "go", "build", "-o", cliPath, "./cmd/codefly")
	compile.Dir = "../.."
	output, err := compile.CombinedOutput()
	require.NoError(t, err, string(output))
	workspaceDir := filepath.Join(root, "workspace")
	require.NoError(t, os.Mkdir(workspaceDir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(workspaceDir, "workspace.codefly.yaml"), []byte("kind: workspace\nname: proof\nlayout: flat\nservices: []\n"), 0600))
	run := func(args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, cliPath, args...)
		command.Dir = workspaceDir
		var stderr strings.Builder
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Logf("codefly %v: %s", args, stderr.String())
		}
		return output, err
	}
	for _, command := range []string{"add", "build"} {
		help, err := run(command, "runnable", "--help")
		require.NoError(t, err)
		require.Contains(t, string(help), "--json")
	}
	_, err = run("add", "runnable", "invalid", "--agent=python:0.0.1", "--handler=handler.invalid")
	require.Error(t, err)
	require.NoDirExists(t, filepath.Join(workspaceDir, "runnables", "invalid"))
	workspace, err := resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	require.NoError(t, err)
	_, err = workspace.FindRunnableByName(ctx, "invalid")
	require.Error(t, err, "failed creation must roll back its module reference")

	created, err := run("add", "runnable", "word-count", "--agent=python:0.0.1", "--handler=handler.py", "--json")
	require.NoError(t, err)
	var receipt map[string]string
	require.NoError(t, json.Unmarshal(created, &receipt))
	source := receipt["directory"]
	require.FileExists(t, filepath.Join(source, "codefly_types.py"))
	workspace, err = resources.LoadWorkspaceFromDir(ctx, workspaceDir)
	require.NoError(t, err)
	r, err := workspace.FindRunnableByName(ctx, "word-count")
	require.NoError(t, err)
	r.Contract.Input.Fields = []*resources.RunnableField{{Name: "text", Type: "string"}}
	r.Contract.Output.Fields = []*resources.RunnableField{{Name: "count", Type: "integer"}}
	require.NoError(t, r.Save(ctx))
	require.NoError(t, os.WriteFile(filepath.Join(source, "handler.py"), []byte("def handle(context, input):\n    return {\"count\": len(input[\"text\"].split())}\n"), 0600))
	buildDir := filepath.Join(root, "build")
	encoded, err := run("build", "runnable", "word-count", "--output="+buildDir, "--json")
	require.NoError(t, err)
	pkg := &basev0.RunnablePackage{}
	require.NoError(t, protojson.Unmarshal(encoded, pkg))
	require.NoError(t, corerunnable.VerifyPackage(pkg))
	require.FileExists(t, filepath.Join(buildDir, "artifacts", runnableops.PackageFile))
	require.Len(t, pkg.GetArtifacts(), 1)
	artifact := pkg.GetArtifacts()[0]
	archive := filepath.Join(buildDir, "artifacts", artifact.GetReference())
	installed := filepath.Join(root, "installed")
	require.NoError(t, os.Mkdir(installed, 0700))
	unpack := exec.CommandContext(ctx, "tar", "-xzf", archive, "-C", installed)
	output, err = unpack.CombinedOutput()
	require.NoError(t, err, string(output))
	// The native archive must work after both author source and prepared files disappear.
	require.NoError(t, os.Rename(source, source+"-unavailable"))
	require.NoError(t, os.RemoveAll(filepath.Join(buildDir, "prepared")))
	for _, test := range []struct {
		input   string
		count   int
		outcome basev0.RunnableCompletion_Outcome
	}{
		{`{"text":"one two three"}`, 3, basev0.RunnableCompletion_SUCCEEDED},
		{`{"text":"four five"}`, 2, basev0.RunnableCompletion_SUCCEEDED},
		{`{"text":42}`, 0, basev0.RunnableCompletion_CRASHED},
	} {
		started := time.Now()
		inv, err := corerunnable.PrepareInvocation(&basev0.RunnableInvocation{
			Protocol: corerunnable.ProtocolV1, Runnable: pkg.GetIdentity(), InvocationId: "inv-1", IntentId: "intent-1",
			IssuedAt: timestamppb.New(started), Deadline: timestamppb.New(started.Add(10 * time.Second)), Input: []byte(test.input),
		}, pkg)
		require.NoError(t, err)
		requestPath := filepath.Join(t.TempDir(), "invocation.json")
		resultPath := filepath.Join(t.TempDir(), "result.json")
		request, err := corerunnable.EncodeInvocation(inv)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(requestPath, request, 0600))
		argv := artifact.GetCommand()
		invoke := exec.CommandContext(ctx, filepath.Join(installed, argv[0]), argv[1:]...)
		invoke.Dir = installed
		invoke.Env = os.Environ()
		for key, value := range corerunnable.InvocationEnvironment(inv, requestPath, resultPath) {
			invoke.Env = append(invoke.Env, key+"="+value)
		}
		diagnostics, runErr := invoke.CombinedOutput()
		require.NotNil(t, invoke.ProcessState, "start invocation: %v", runErr)
		result, err := os.ReadFile(resultPath)
		if err != nil {
			require.True(t, os.IsNotExist(err), "%v", err)
		}
		completed, err := corerunnable.Complete(inv, pkg, corerunnable.Observation{Result: result, ExitCode: int32(invoke.ProcessState.ExitCode()), StartedAt: started, EndedAt: time.Now()})
		require.NoError(t, err)
		require.Equal(t, test.outcome, completed.GetOutcome(), string(diagnostics))
		if test.outcome == basev0.RunnableCompletion_SUCCEEDED {
			var output struct {
				Count int `json:"count"`
			}
			require.NoError(t, json.Unmarshal(completed.GetResult().GetOutput(), &output))
			require.Equal(t, test.count, output.Count)
		} else {
			require.False(t, corerunnable.OutcomeIsCertain(completed.GetOutcome()))
		}
		t.Logf("input=%s outcome=%s output=%s", test.input, completed.GetOutcome(), completed.GetResult().GetOutput())
	}
	require.NoError(t, os.Rename(source+"-unavailable", source))
	_, err = run("build", "runnable", "word-count", "--output="+buildDir)
	require.Error(t, err, "existing output must not be overwritten")
	archive, err = filepath.EvalSymlinks(archive)
	require.NoError(t, err)
	platform := strings.Split(artifact.GetPlatform(), "/")
	emitted := []*builderv0.PackageArtifact{{Kind: builderv0.PackageArtifact_ARCHIVE, Path: archive,
		Target: &builderv0.PackageTarget{Os: platform[0], Architecture: platform[1]}, Sha256: strings.TrimPrefix(artifact.GetDigest(), "sha256:"), Command: artifact.GetCommand()}}
	require.NoError(t, runnableops.VerifyBuild(ctx, workspace, r, pkg, emitted, filepath.Dir(archive)))
	require.NoError(t, os.WriteFile(archive, []byte("changed after packaging"), 0600))
	require.ErrorContains(t, runnableops.VerifyBuild(ctx, workspace, r, pkg, emitted, filepath.Dir(archive)), "content digest does not match")
}
