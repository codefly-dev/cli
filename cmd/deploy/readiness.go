package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/timings"
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
var WorkspaceReadiness func(ctx context.Context, question ReadinessQuestion) (ReadinessVerdict, error)

// ReadinessQuestion is what a delivery verb asks the readiness engine.
type ReadinessQuestion struct {
	Env, Module string
	// Enforcing is false when the operator took the override: the engine then
	// reports its verdict without printing the refusal banner a verb that
	// proceeds has not earned.
	Enforcing bool
	// JudgePins compares the composition's pins with their sources now, and
	// refuses one behind its source that no hold covers. A render sets it. A
	// publication of a render that recorded its own judgement does not: the
	// render's judgement is the one it is held to (publishReady).
	JudgePins bool
}

// ReadinessVerdict is what a passing (or overridden) readiness found that the
// verb carries forward.
type ReadinessVerdict struct {
	// PinsJudged reports that the composition's pins were compared with their
	// sources; Behind then names each one behind its source, or not compared,
	// that no hold covers — non-empty only under the override.
	PinsJudged bool
	Behind     []string
	// HeldUntil is the earliest end date of the pin-holds the judgement
	// relied on, empty when it relied on none.
	HeldUntil string
}

// pinCurrency is the verdict as a render records it: nil when no pin was judged.
func (verdict ReadinessVerdict) pinCurrency() *gitops.PinCurrency {
	if !verdict.PinsJudged {
		return nil
	}
	return &gitops.PinCurrency{Behind: verdict.Behind, HeldUntil: verdict.HeldUntil}
}

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
func requireWorkspaceReady(ctx context.Context, verb, env, module string) (*gitops.PinCurrency, error) {
	verdict, err := askReadiness(ctx, verb, ReadinessQuestion{Env: env, Module: module, JudgePins: true})
	return verdict.pinCurrency(), err
}

// publishReady is the precondition of a verb that delivers a render — plan
// and publish. Its pins are held to the judgement the render recorded, not
// judged again against sources that moved while it rendered: a render takes
// minutes, other merges move a pinned source's main meanwhile, and judging
// the pins again refused, as "N commits behind main", the very commits the
// render was right to ship. A render whose pins moved since is refused by
// publish itself (it is of another composition), so what is delivered is
// always what was judged. A render that recorded no judgement — an older
// CLI's — has its pins judged now, as before.
func publishReady(ctx context.Context, verb, workspaceDir, env, module string) error {
	judged := gitops.RenderedPinCurrency(workspaceDir, env, module)
	if judged.Expired(time.Now()) {
		// A hold the render relied on has ended since: its judgement no
		// longer stands, and the pins are judged now.
		cli.Warning("a pin-hold the render of %s relied on ended %s: its pins are judged against their sources now", module, judged.HeldUntil)
		judged = nil
	}
	if _, err := askReadiness(ctx, verb, ReadinessQuestion{Env: env, Module: module, JudgePins: judged == nil}); err != nil || judged == nil {
		return err
	}
	if len(judged.Behind) == 0 {
		return nil
	}
	behind := fmt.Sprintf("module %s was rendered for environment %s on pins its render found behind their sources and held by nothing: %s; move each pin (or state a hold in pin-holds) and render again",
		module, env, strings.Join(judged.Behind, "; "))
	if !skipWorkspaceReadiness {
		return fmt.Errorf("%s — refusing to %s (override with --%s)", behind, verb, SkipWorkspaceReadinessFlag)
	}
	cli.Warning("--%s: %s", SkipWorkspaceReadinessFlag, behind)
	return nil
}

func askReadiness(ctx context.Context, verb string, question ReadinessQuestion) (ReadinessVerdict, error) {
	defer timings.Start("readiness")()
	env := question.Env
	if WorkspaceReadiness == nil {
		if skipWorkspaceReadiness {
			cli.Warning("--%s: workspace readiness for environment %q was NOT evaluated before `%s`", SkipWorkspaceReadinessFlag, env, verb)
		}
		return ReadinessVerdict{}, nil
	}
	if !skipWorkspaceReadiness {
		question.Enforcing = true
		verdict, err := WorkspaceReadiness(ctx, question)
		if err != nil {
			return ReadinessVerdict{}, fmt.Errorf("%w — refusing to %s (override with --%s)", err, verb, SkipWorkspaceReadinessFlag)
		}
		return verdict, nil
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
	// as before. The pins it found behind are carried forward either way: a
	// render records them, and its publish refuses them.
	verdict, err := WorkspaceReadiness(ctx, question)
	if err != nil {
		cli.Warning("--%s: workspace readiness for environment %q FAILED and was not enforced before `%s`", SkipWorkspaceReadinessFlag, env, verb)
		cli.Warning("--%s: every delivery verb evaluates this precondition, so the next one will refuse until it is fixed", SkipWorkspaceReadinessFlag)
		return verdict, nil
	}
	cli.Warning("--%s: workspace readiness for environment %q passed, so the override changed nothing before `%s`", SkipWorkspaceReadinessFlag, env, verb)
	return verdict, nil
}
