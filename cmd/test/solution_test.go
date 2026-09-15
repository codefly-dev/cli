package test

import (
	"testing"

	"github.com/codefly-dev/cli/cmd/run"
	"github.com/codefly-dev/cli/pkg/orchestration"
	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// testVerbs are the two commands that drive the shared test path.
func testVerbs() map[string]*cobra.Command {
	return map[string]*cobra.Command{"service": ServiceCmd, "solution": SolutionCmd}
}

// `test solution` exists and takes no service selector: it resolves the
// solution's own service-entry rather than being handed one.
func TestSolutionCommandResolvesItsOwnEntry(t *testing.T) {
	if SolutionCmd.RunE == nil || SolutionCmd.Run != nil {
		t.Fatal("test solution must return errors through RunE")
	}
	if err := SolutionCmd.Args(SolutionCmd, []string{"backend"}); err == nil {
		t.Fatal("test solution accepted a positional service selector")
	}
}

// Both test verbs take the run-parity flags, and the port-isolation pair must
// describe the same mechanism as their `run` twins — otherwise `--help`
// documents one mechanism three different ways.
func TestTestCommandsExposeRunParityFlags(t *testing.T) {
	for _, name := range []string{"env", "profile", "exclude-dependency", "output-env", "naming-scope", "temporary-ports", "fixture"} {
		for verb, cmd := range testVerbs() {
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("test %s has no --%s flag", verb, name)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"naming-scope", run.NamingScopeUsage},
		{"temporary-ports", run.TemporaryPortsUsage},
	} {
		for verb, cmd := range testVerbs() {
			flag := cmd.Flags().Lookup(tc.name)
			if flag == nil {
				t.Fatalf("test %s has no --%s flag", verb, tc.name)
			}
			if flag.Usage != tc.usage {
				t.Errorf("test %s --%s help diverges from the run path: %q", verb, tc.name, flag.Usage)
			}
		}
	}
}

// Isolation is on by default on the test path: two concurrent
// `codefly test service` invocations in one workspace must not share ports,
// container names or state directories. The run path defaults it off, because a
// run is a long-lived stack an operator returns to by name.
func TestTestPathIsolatesByDefault(t *testing.T) {
	for verb, cmd := range testVerbs() {
		if got := cmd.Flags().Lookup("temporary-ports").DefValue; got != "true" {
			t.Errorf("test %s --temporary-ports default = %q, want true (isolation by default)", verb, got)
		}
	}
	if got := run.ServiceCmd.Flags().Lookup("temporary-ports").DefValue; got != "false" {
		t.Errorf("run --temporary-ports default = %q, want false", got)
	}
}

// The dead `--scope` flag is gone. It was declared and never read, so it
// silently accepted an isolation request it did not honor.
func TestDeadScopeFlagIsGone(t *testing.T) {
	if ServiceCmd.Flags().Lookup("scope") != nil {
		t.Error("test service still declares the no-op --scope flag")
	}
}

// An explicit --naming-scope names the run; an explicitly empty one clears a
// workspace-declared scope, and both suppress the generated isolation identity.
func TestShouldIsolateInvocation(t *testing.T) {
	for _, tc := range []struct {
		temporaryPorts, explicit, want bool
	}{
		{temporaryPorts: true, explicit: false, want: true},
		{temporaryPorts: true, explicit: true, want: false},
		{temporaryPorts: false, explicit: false, want: false},
	} {
		if got := shouldIsolateInvocation(tc.temporaryPorts, tc.explicit); got != tc.want {
			t.Errorf("shouldIsolateInvocation(%v, %v) = %v, want %v",
				tc.temporaryPorts, tc.explicit, got, tc.want)
		}
	}
}

// --env selects the environment whose declaration carries the fixture, and
// --naming-scope applies to this invocation's copy only. Previously the test
// path hardcoded local, so an environment-declared fixture was unreachable.
func TestTestEnvironmentSelectsAndScopes(t *testing.T) {
	restore := func(env, scope string, explicit bool) {
		environmentName, namingScope, namingScopeExplicit = env, scope, explicit
	}
	t.Cleanup(func() { restore(environmentName, namingScope, namingScopeExplicit) })

	workspace := &resources.Workspace{
		Name: "solution",
		Environments: []*resources.Environment{
			{Name: orchestration.LocalEnvironmentName, Fixture: "dev-admin", NamingScope: "declared"},
		},
	}

	restore(orchestration.LocalEnvironmentName, "", false)
	env, err := testEnvironment(workspace)
	if err != nil {
		t.Fatalf("testEnvironment: %v", err)
	}
	if env.Fixture != "dev-admin" {
		t.Errorf("fixture = %q, want the environment-declared dev-admin", env.Fixture)
	}
	if env.NamingScope != "declared" {
		t.Errorf("naming scope = %q, want the declared scope kept when the flag is absent", env.NamingScope)
	}

	restore(orchestration.LocalEnvironmentName, "alpha", false)
	if env, err = testEnvironment(workspace); err != nil || env.NamingScope != "alpha" {
		t.Errorf("naming scope = %q (err %v), want the --naming-scope override", env.NamingScope, err)
	}

	restore(orchestration.LocalEnvironmentName, "", true)
	if env, err = testEnvironment(workspace); err != nil || env.NamingScope != "" {
		t.Errorf("naming scope = %q (err %v), want an explicitly empty scope to clear the declared one", env.NamingScope, err)
	}

	// The selection is a copy: an invocation-scoped override must not leak into
	// the declaration shared by concurrent flows.
	if workspace.Environments[0].NamingScope != "declared" {
		t.Errorf("workspace declaration mutated to %q", workspace.Environments[0].NamingScope)
	}

	restore("staging", "", false)
	if _, err = testEnvironment(workspace); err == nil {
		t.Error("selecting an undeclared environment unexpectedly succeeded")
	}
}
