//go:build integration

package runnable_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	r.Execution.Cancellation = resources.RunnableCancellationSignal
	require.NoError(t, r.Save(ctx))
	// The handler runs long on one reserved input so the launcher's
	// interruption of a live harness is qualified against the real one.
	require.NoError(t, os.WriteFile(filepath.Join(source, "handler.py"), []byte("import time\n\n\ndef handle(context, input):\n    if input[\"text\"] == \"sleep\":\n        time.sleep(30)\n    return {\"count\": len(input[\"text\"].split())}\n"), 0600))
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
	binding, err := corerunnable.PrepareBinding(&basev0.RunnableBinding{
		Schema: corerunnable.BindingSchemaV1, Identity: pkg.GetIdentity(), PackageDigest: pkg.GetDigest(),
		Facility: &basev0.RunnableFacility{Kind: basev0.RunnableFacility_NATIVE}, Artifact: artifact,
	}, pkg)
	require.NoError(t, err)
	launcher, err := runnableops.NewNativeLauncher(pkg, binding, installed)
	require.NoError(t, err)
	// The native archive must work after both author source and prepared files disappear.
	require.NoError(t, os.Rename(source, source+"-unavailable"))
	require.NoError(t, os.RemoveAll(filepath.Join(buildDir, "prepared")))

	// Every invocation below goes through the CLI's launcher, so what is
	// qualified is the real framing between it and the agent's generated
	// harness: the documents, the process boundary and the typed completion.
	invocations := filepath.Join(root, "invocations")
	invoke := func(ctx context.Context, id string, input string) (*basev0.RunnableCompletion, string, string) {
		issued := time.Now()
		var out, logs strings.Builder
		completed, err := launcher.Invoke(ctx, runnableops.Run{
			Invocation: &basev0.RunnableInvocation{
				Protocol: corerunnable.ProtocolV1, Runnable: pkg.GetIdentity(), InvocationId: id, IntentId: "intent-" + id,
				IssuedAt: timestamppb.New(issued), Deadline: timestamppb.New(issued.Add(90 * time.Second)), Input: []byte(input),
			},
			Directory: filepath.Join(invocations, id), Stdout: &out, Stderr: &logs,
		})
		require.NoError(t, err)
		require.FileExists(t, filepath.Join(invocations, id, runnableops.InvocationFile), "the dispatched document is retained as evidence")
		t.Logf("id=%s input=%s outcome=%s output=%s stderr=%s", id, input, completed.GetOutcome(), completed.GetResult().GetOutput(), logs.String())
		return completed, out.String(), logs.String()
	}
	for i, test := range []struct {
		input   string
		count   int
		outcome basev0.RunnableCompletion_Outcome
	}{
		{`{"text":"one two three"}`, 3, basev0.RunnableCompletion_SUCCEEDED},
		{`{"text":"four five"}`, 2, basev0.RunnableCompletion_SUCCEEDED},
		{`{"text":42}`, 0, basev0.RunnableCompletion_CRASHED},
	} {
		completed, out, logs := invoke(ctx, fmt.Sprintf("inv-%d", i), test.input)
		require.Equal(t, test.outcome, completed.GetOutcome(), logs)
		if test.outcome == basev0.RunnableCompletion_SUCCEEDED {
			var output struct {
				Count int `json:"count"`
			}
			require.NoError(t, json.Unmarshal(completed.GetResult().GetOutput(), &output))
			require.Equal(t, test.count, output.Count)
			// Completion data never travels as process output.
			require.Empty(t, out)
			require.Equal(t, uint64(0), completed.GetLogs().GetStdout().GetBytes())
		} else {
			require.False(t, corerunnable.OutcomeIsCertain(completed.GetOutcome()))
			// Wrong input shape is refused before the handler runs, and the
			// refusal is a diagnostic rather than a completion.
			require.Contains(t, logs, "invalid_input")
			require.Nil(t, completed.GetResult())
		}
	}

	// Interrupting a live harness: the package declares signal cancellation, so
	// the launcher may end the process group and the outcome stays uncertain.
	interrupting, interrupt := context.WithCancel(ctx)
	go func() {
		time.Sleep(2 * time.Second)
		interrupt()
	}()
	completed, _, logs := invoke(interrupting, "inv-canceled", `{"text":"sleep"}`)
	interrupt()
	require.Equal(t, basev0.RunnableCompletion_CANCELED, completed.GetOutcome(), logs)
	require.False(t, corerunnable.OutcomeIsCertain(completed.GetOutcome()))
	require.NoError(t, os.Rename(source+"-unavailable", source))
	_, err = run("build", "runnable", "word-count", "--output="+buildDir)
	require.Error(t, err, "an explicit output directory must not be overwritten")

	// VerifyBuild is given the path the agent reports, not a pre-resolved one:
	// normalizing it is the CLI's job, and a symlinked output must still match.
	platform := strings.Split(artifact.GetPlatform(), "/")
	emitted := []*builderv0.PackageArtifact{{Kind: builderv0.PackageArtifact_ARCHIVE, Path: archive,
		Target: &builderv0.PackageTarget{Os: platform[0], Architecture: platform[1]}, Sha256: strings.TrimPrefix(artifact.GetDigest(), "sha256:"), Command: artifact.GetCommand()}}
	require.NoError(t, runnableops.VerifyBuild(ctx, workspace, r, pkg, emitted, filepath.Dir(archive)))
	require.NoError(t, os.WriteFile(archive, []byte("changed after packaging"), 0600))
	require.ErrorContains(t, runnableops.VerifyBuild(ctx, workspace, r, pkg, emitted, filepath.Dir(archive)), "content digest does not match")

	// A package whose build evidence no longer describes the author's source is
	// rejected even though every digest inside it is self-consistent: this is
	// the stale-snapshot case the archive digest alone cannot see.
	handler := filepath.Join(source, "handler.py")
	edited := []byte("def handle(context, input):\n    return {\"count\": 1 + len(input[\"text\"].split())}\n")
	original, err := os.ReadFile(handler)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(handler, edited, 0600))
	require.ErrorContains(t, runnableops.VerifyBuild(ctx, workspace, r, pkg, emitted, filepath.Dir(archive)),
		"packaged different bytes than the declaration references")
	require.NoError(t, os.WriteFile(handler, original, 0600))

	// The CLI-owned default directory is scratch, not a release: the ordinary
	// edit/rebuild loop must not strand itself on its deterministic path.
	first, err := run("build", "runnable", "word-count", "--json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(handler, edited, 0600))
	second, err := run("build", "runnable", "word-count", "--json")
	require.NoError(t, err, "rebuilding into the default output must succeed")
	firstPkg, secondPkg := &basev0.RunnablePackage{}, &basev0.RunnablePackage{}
	require.NoError(t, protojson.Unmarshal(first, firstPkg))
	require.NoError(t, protojson.Unmarshal(second, secondPkg))
	require.NotEqual(t, firstPkg.GetDigest(), secondPkg.GetDigest(), "edited source must produce a different release digest")
}
