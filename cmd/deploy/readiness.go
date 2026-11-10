package deploy

import (
	"context"
	"fmt"

	"github.com/codefly-dev/cli/pkg/cli"
)

// SkipWorkspaceReadinessFlag is the one name of the override, used for the flag
// itself and for every message that mentions it, so the hint an operator is
// given is always the string they can paste.
const SkipWorkspaceReadinessFlag = "skip-workspace-readiness"

// WorkspaceReadiness evaluates `codefly doctor workspace --env <env> --module
// <module>` and returns nil when the workspace is ready for that module in that
// environment, or an error carrying the doctor's own failing diagnostics and
// fix hints when it is not. It prints the report as the doctor does, so the
// operator sees the same thing whichever entry point refused.
//
// It is wired from package cmd, which owns the readiness engine, to avoid an
// import cycle: package cmd already imports this package to register GitOpsCmd,
// so this package cannot import cmd back. It is nil in this package's unit
// tests unless one installs a stand-in, which is how the gate's behaviour is
// exercised without the engine.
var WorkspaceReadiness func(ctx context.Context, env, module string) error

// skipWorkspaceReadiness is the operator's explicit override. It exists for one
// situation: a workspace mid-repair, where the module being delivered is fine
// and a sibling is not. It is never the default, and taking it says so loudly.
var skipWorkspaceReadiness bool

// requireWorkspaceReady is the precondition every delivery verb evaluates
// before it does any expensive work — before cloning a promotion repository,
// before driving the builder agents that compile and push container images.
//
// It runs after the module has been resolved, not before: resolving the module
// materializes the workspace's composed pinned modules, which is what the
// readiness check itself needs in order to judge configuration at all (an
// unmaterialized workspace can only be reported as unmaterialized), and which
// is free once it has happened. Everything the failure used to be discovered
// behind — the render, the builds, the pushes, the clone — still comes after.
func requireWorkspaceReady(ctx context.Context, verb, env, module string) error {
	if WorkspaceReadiness == nil {
		if skipWorkspaceReadiness {
			cli.Warning("--%s: workspace readiness for environment %q was NOT evaluated before `%s`", SkipWorkspaceReadinessFlag, env, verb)
		}
		return nil
	}
	// The override skips the REFUSAL, not the evaluation.
	//
	// It used to skip both, and that made it a trap rather than an override: a
	// render would succeed while `publish` held the same precondition and refused
	// it, so the operator learned what readiness already knew one verb later, with
	// a rendered tree in hand that could not be published. On 2026-11-10 that is
	// exactly what happened — a render passed under this flag with two operations
	// missing from `runnable-bindings`, and publish rejected it.
	//
	// So the report is always produced. Under the flag it is printed and the verb
	// proceeds, naming what the next verb will refuse; without it, it refuses here
	// as before.
	if skipWorkspaceReadiness {
		if err := WorkspaceReadiness(ctx, env, module); err != nil {
			cli.Warning("--%s: workspace readiness for environment %q FAILED and was not enforced before `%s`", SkipWorkspaceReadinessFlag, env, verb)
			cli.Warning("--%s: `publish` evaluates the same precondition and will refuse this delivery until it is fixed", SkipWorkspaceReadinessFlag)
			return nil
		}
		cli.Warning("--%s: workspace readiness for environment %q passed, so the override changed nothing before `%s`", SkipWorkspaceReadinessFlag, env, verb)
		return nil
	}
	if err := WorkspaceReadiness(ctx, env, module); err != nil {
		return fmt.Errorf("%w — refusing to %s (override with --%s)", err, verb, SkipWorkspaceReadinessFlag)
	}
	return nil
}
