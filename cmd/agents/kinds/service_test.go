package kinds

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents/manager"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

func TestServiceCommandReturnsErrors(t *testing.T) {
	if ServiceCmd.RunE == nil || ServiceCmd.Run != nil {
		t.Fatal("agent kind service command must return errors through RunE")
	}
	if err := ServiceCmd.Args(ServiceCmd, []string{"extra"}); err == nil {
		t.Fatal("agent kind service command accepted positional arguments")
	}
	if err := serviceInfo(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "--agent is required") {
		t.Fatalf("empty agent error = %v", err)
	}
}

func TestRunnableCommandReturnsErrors(t *testing.T) {
	if RunnableCmd.RunE == nil || RunnableCmd.Run != nil {
		t.Fatal("agent kind runnable command must return errors through RunE")
	}
	if err := RunnableCmd.Args(RunnableCmd, []string{"extra"}); err == nil {
		t.Fatal("agent kind runnable command accepted positional arguments")
	}
	if err := runnableInfo(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "--agent is required") {
		t.Fatalf("empty agent error = %v", err)
	}
}

// TestRunnableAgentKindIsResolvedThroughTheRegistry proves the runnable agent
// reaches the same resolution machinery as a service agent — the language is
// the agent's name, not a branch in the CLI.
func TestRunnableAgentKindIsResolvedThroughTheRegistry(t *testing.T) {
	agent, err := resources.ParseAgent(context.Background(), resources.RunnableAgent, "python:0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !agent.IsRunnable() {
		t.Fatalf("parsed agent is not a runnable agent: %+v", agent)
	}
	registration, err := resources.AgentKindRegistrationFor(agent.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if got := registration.ExecutableName(agent.Name); got != "runnable-python" {
		t.Fatalf("executable name = %q, want runnable-python", got)
	}
	if got := registration.GitHubRepository(agent.Name); got != "runnable-python" {
		t.Fatalf("repository = %q, want runnable-python", got)
	}
	if registration.InstallSubdirectory != "runnables" {
		t.Fatalf("install subdirectory = %q, want runnables", registration.InstallSubdirectory)
	}
}

// TestAgentInfoNarrationIsStablePerKind pins the narration the existing
// service command has always emitted. Sharing its body with the runnable
// command silently rewrote both the header text and the wool identifier
// ("cmd.info.agentInput.service"), which anyone grepping logs depends on.
func TestAgentInfoNarrationIsStablePerKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(context.Context, string) error
		want string
	}{
		{name: "service", call: serviceInfo, want: "Fetching information about Service Agent <codefly.dev/absent:0.0.1> information"},
		{name: "runnable", call: runnableInfo, want: "Fetching information about Runnable Agent <codefly.dev/absent:0.0.1> information"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
			t.Setenv(manager.AgentSourceEnv, string(resources.AgentStoreLocal))

			var lines []string
			cli.SetOutputSink(func(_ wool.Loglevel, msg string) { lines = append(lines, msg) })
			t.Cleanup(func() { cli.SetOutputSink(nil) })

			// The agent does not exist, so this fails at resolution — after the
			// first header, which is the line under test.
			if err := tc.call(context.Background(), "absent:0.0.1"); err == nil {
				t.Fatal("loading an absent agent returned success")
			}
			if len(lines) == 0 || lines[0] != tc.want {
				t.Fatalf("first narration line = %q, want %q", lines, tc.want)
			}
		})
	}
}
