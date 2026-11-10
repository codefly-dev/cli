package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

// stubReadiness installs a stand-in for the readiness engine (which lives in
// package cmd) and restores the previous wiring afterwards.
func stubReadiness(t *testing.T, verdict func(ctx context.Context, env, module string) error) *[]string {
	t.Helper()
	previous := WorkspaceReadiness
	t.Cleanup(func() { WorkspaceReadiness = previous })
	enforced = nil
	t.Cleanup(func() { enforced = nil })
	var asked []string
	WorkspaceReadiness = func(ctx context.Context, env, module string, enforcing bool) error {
		asked = append(asked, env+"/"+module)
		enforced = append(enforced, enforcing)
		return verdict(ctx, env, module)
	}
	return &asked
}

// enforced records, per question, whether the engine was asked to enforce. The
// override must report without enforcing, so the engine does not print a refusal
// banner on a verb that proceeds.
var enforced []bool

func withSkip(t *testing.T, value bool) {
	t.Helper()
	previous := skipWorkspaceReadiness
	t.Cleanup(func() { skipWorkspaceReadiness = previous })
	skipWorkspaceReadiness = value
}

func TestRequireWorkspaceReadyRefusesAndNamesTheOverride(t *testing.T) {
	withSkip(t, false)
	stubReadiness(t, func(context.Context, string, string) error {
		return errors.New("workspace is not ready for environment \"staging\"")
	})

	err := requireWorkspaceReady(context.Background(), "render", "staging", "saas")
	if err == nil {
		t.Fatal("a not-ready workspace must refuse the verb")
	}
	if !strings.Contains(err.Error(), "--"+SkipWorkspaceReadinessFlag) {
		t.Fatalf("refusal %q does not name the override", err)
	}
	if !strings.Contains(err.Error(), "refusing to render") {
		t.Fatalf("refusal %q does not name the verb it refused", err)
	}
}

func TestRequireWorkspaceReadyPassesAReadyWorkspace(t *testing.T) {
	withSkip(t, false)
	asked := stubReadiness(t, func(context.Context, string, string) error { return nil })

	if err := requireWorkspaceReady(context.Background(), "render", "staging", "saas"); err != nil {
		t.Fatalf("a ready workspace must not be refused: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "staging/saas" {
		t.Fatalf("readiness was asked %v, want one question about staging/saas", *asked)
	}
}

// The override exists for an operator mid-repair, whose sibling module is
// unready: it lets the verb PROCEED. It skips the refusal and not the
// evaluation, so the operator is told what they are overriding.
//
// It used to skip both. That made it a trap rather than an override: a render
// succeeded while `publish` held the same precondition and refused it, so the
// operator learned what readiness already knew one verb later, holding a rendered
// tree that could not be published. On 2026-11-10 that happened for real — a
// render passed under this flag with two operations missing from
// `runnable-bindings`, and publish rejected the delivery.
func TestSkipWorkspaceReadinessProceedsAndStillReportsTheVerdict(t *testing.T) {
	withSkip(t, true)
	asked := stubReadiness(t, func(context.Context, string, string) error {
		return errors.New("workspace is not ready")
	})

	if err := requireWorkspaceReady(context.Background(), "render", "staging", "saas"); err != nil {
		t.Fatalf("the override must let the verb proceed: %v", err)
	}
	if len(*asked) != 1 || (*asked)[0] != "staging/saas" {
		t.Fatalf("the override must still evaluate readiness so it can be reported, asked %v", *asked)
	}
}

// A passing workspace under the override is not silence either: the operator is
// told the flag changed nothing, so a habit of passing it is visible.
func TestSkipWorkspaceReadinessEvaluatesEvenWhenItWouldHavePassed(t *testing.T) {
	withSkip(t, true)
	asked := stubReadiness(t, func(context.Context, string, string) error { return nil })

	if err := requireWorkspaceReady(context.Background(), "render", "staging", "saas"); err != nil {
		t.Fatalf("a ready workspace under the override must not be refused: %v", err)
	}
	if len(*asked) != 1 {
		t.Fatalf("readiness was asked %v, want one question even under the override", *asked)
	}
}

// With no engine installed the gate cannot evaluate anything, and the override
// must not pretend it did.
func TestSkipWorkspaceReadinessWithNoEngineProceeds(t *testing.T) {
	withSkip(t, true)
	previous := WorkspaceReadiness
	WorkspaceReadiness = nil
	t.Cleanup(func() { WorkspaceReadiness = previous })

	if err := requireWorkspaceReady(context.Background(), "render", "staging", "saas"); err != nil {
		t.Fatalf("with no readiness engine the verb proceeds: %v", err)
	}
}

// Every delivery verb — and only the delivery verbs — carries the override.
// `observe`, `rollback` and the `remote` verbs act on an already-reviewed
// revision and are what an operator reaches for while the workspace is broken.
func TestOnlyTheDeliveryVerbsCarryTheOverride(t *testing.T) {
	gated := map[string]bool{"render": true, "snapshot": true, "plan": true, "publish": true}
	seen := map[string]bool{}
	for _, command := range GitOpsCmd.Commands() {
		name := command.Name()
		has := command.Flags().Lookup(SkipWorkspaceReadinessFlag) != nil
		if gated[name] {
			seen[name] = true
			if !has {
				t.Errorf("delivery verb %q does not carry --%s", name, SkipWorkspaceReadinessFlag)
			}
			if has && command.Flags().Lookup(SkipWorkspaceReadinessFlag).DefValue != "false" {
				t.Errorf("--%s must not default to on for %q", SkipWorkspaceReadinessFlag, name)
			}
			continue
		}
		if has {
			t.Errorf("recovery verb %q must not be gated on workspace readiness", name)
		}
	}
	for name := range gated {
		if !seen[name] {
			t.Errorf("delivery verb %q is not registered under `deploy gitops`", name)
		}
	}
	// The remote sub-tree is recovery too.
	for _, command := range gitOpsRemoteCmd.Commands() {
		if command.Flags().Lookup(SkipWorkspaceReadinessFlag) != nil {
			t.Errorf("remote verb %q must not be gated on workspace readiness", command.Name())
		}
	}
}

// The gate runs before the verb's expensive work: a refusal returns without the
// coordinator, the promotion clone, or the builder agents ever being reached.
// Proven by stubbing the module loader — everything after the gate needs a
// resolved environment and a cluster-shaped module, so a refusal that reaches
// any of it fails with a different error than the gate's.
func TestDeliveryVerbsRefuseBeforeAnyWork(t *testing.T) {
	sentinel := errors.New("readiness refused")
	for _, command := range []*cobra.Command{gitOpsRenderCmd, gitOpsSnapshotCmd, gitOpsPlanCmd, gitOpsPublishCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			withSkip(t, false)
			stubReadiness(t, func(context.Context, string, string) error { return sentinel })
			previous := loadGitOpsModule
			t.Cleanup(func() { loadGitOpsModule = previous })
			loadGitOpsModule = stubModuleLoader(t)

			err := command.RunE(command, []string{"saas"})
			if !errors.Is(err, sentinel) {
				t.Fatalf("%s returned %v, want the readiness refusal — the verb did work before the gate", command.Name(), err)
			}
		})
	}
}

// stubModuleLoader stands in for the loader every gitops verb goes through, so
// the gate can be reached without a workspace on disk.
func stubModuleLoader(t *testing.T) func(ctx context.Context, args []string) (*resources.Workspace, *resources.Module, error) {
	t.Helper()
	return func(_ context.Context, args []string) (*resources.Workspace, *resources.Module, error) {
		name := "saas"
		if len(args) > 0 {
			name = args[0]
		}
		workspace := &resources.Workspace{Name: "demo"}
		workspace.WithDir(t.TempDir())
		return workspace, &resources.Module{Name: name}, nil
	}
}

// The override reports without enforcing, so the engine prints its diagnostics
// and not the refusal banner: a verb that proceeds and exits 0 must not leave
// "Workspace is NOT ready" in the log for a scraper to read as a failure.
func TestSkipWorkspaceReadinessReportsWithoutEnforcing(t *testing.T) {
	withSkip(t, true)
	stubReadiness(t, func(context.Context, string, string) error { return errors.New("workspace is not ready") })

	if err := requireWorkspaceReady(context.Background(), "render", "staging", "saas"); err != nil {
		t.Fatalf("the override must let the verb proceed: %v", err)
	}
	if len(enforced) != 1 || enforced[0] {
		t.Fatalf("the override must ask without enforcing, got %v", enforced)
	}
}

// Enforcing asks to enforce, so the banner is printed by the verb that refuses.
func TestRequireWorkspaceReadyEnforcesWhenItRefuses(t *testing.T) {
	withSkip(t, false)
	stubReadiness(t, func(context.Context, string, string) error { return errors.New("workspace is not ready") })

	if err := requireWorkspaceReady(context.Background(), "render", "staging", "saas"); err == nil {
		t.Fatal("an unready workspace must be refused")
	}
	if len(enforced) != 1 || !enforced[0] {
		t.Fatalf("the enforcing path must ask to enforce, got %v", enforced)
	}
}
