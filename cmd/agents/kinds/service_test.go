package kinds

import (
	"context"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
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
