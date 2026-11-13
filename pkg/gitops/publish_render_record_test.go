package gitops

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// renderPublishFixtureFailing is renderPublishFixture's render failing part
// way, as `codefly deploy gitops render saas --env blue` did on 2026-11-12
// when a dependency image was refused.
func renderPublishFixtureFailing(t *testing.T, root, module, environment string) {
	t.Helper()
	_, err := RenderOwnedTree(context.Background(), &RenderOptions{
		Destination: workingrender.Module(root, environment, module), Module: module, UnitNames: []string{"api"},
		OwnedPath:   filepath.ToSlash(filepath.Join("environments", "modules", module)),
		Units:       promotableServiceGraph(module, []string{"api"}),
		Environment: environment, Namespace: "payments", AppProject: "payments", Promotable: true,
	}, func(context.Context, string) error {
		return errors.New("image registry.example/payments/api:0.1.0 is already published from other code")
	})
	require.ErrorContains(t, err, "already published from other code")
}

func TestPublishIsAllowedRightAfterASuccessfulRender(t *testing.T) {
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{Module: "payments", Environment: "production", Local: true})
	require.NoError(t, err)
}

// The 2026-11-12 promotion: a render failed, the tree an earlier render left
// was still in place, and publish delivered it. Publish now refuses until a
// render succeeds again.
func TestPublishRefusesTheTreeAFailedRenderLeft(t *testing.T) {
	ctx := context.Background()
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	request := PublishRequest{Module: "payments", Environment: "production", Local: true}
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	renderPublishFixtureFailing(t, workspace.Dir(), "payments", "production")

	_, err := PlanPublish(ctx, workspace, &request)
	require.ErrorContains(t, err, "the last render of module payments for environment production failed")
	require.ErrorContains(t, err, "already published from other code")
	require.ErrorContains(t, err, "run `codefly deploy gitops render payments --env production` again")

	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	_, err = PlanPublish(ctx, workspace, &request)
	require.NoError(t, err, "a render that succeeds again lifts the refusal")
}

// A render that never finished — interrupted — leaves the earlier tree too.
func TestPublishRefusesARenderThatNeverFinished(t *testing.T) {
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	require.NoError(t, writeRenderRecord(workingrender.Module(workspace.Dir(), "production", "payments"), &renderRecord{Outcome: renderRecordRendering}))

	_, err := PlanPublish(context.Background(), workspace, &PublishRequest{Module: "payments", Environment: "production", Local: true})
	require.ErrorContains(t, err, "never finished")
}

// A render reads the composition's pins; once one moves, the tree is of
// another composition and publish names the pin.
func TestPublishRefusesARenderWhosePinsMoved(t *testing.T) {
	ctx := context.Background()
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	workspace.Modules = []*resources.ModuleReference{{Name: "billing", Source: "github.com/acme/billing", Version: "v1.0.0"}}
	destination := workingrender.Module(workspace.Dir(), "production", "payments")
	_, err := RenderOwnedTree(ctx, &RenderOptions{
		Destination: destination, Module: "payments", UnitNames: []string{"api"},
		OwnedPath:   "environments/modules/payments",
		Units:       promotableServiceGraph("payments", []string{"api"}),
		Environment: "production", Namespace: "payments", AppProject: "payments", Promotable: true,
		CompositionPins: compositionPins(workspace),
	}, func(_ context.Context, stage string) error {
		return writePublishServiceFixture(stage, "production", "api")
	})
	require.NoError(t, err)
	require.NoError(t, holdRenderRecord(workspace, "payments", "production"))

	workspace.Modules[0].Version = "v1.1.0"
	_, err = loadPublicationInventory(ctx, workspace, &PublishRequest{Module: "payments", Environment: "production"}, destination, "environments")
	require.ErrorContains(t, err, "the composition's pins moved since module payments was rendered for environment production")
	require.ErrorContains(t, err, "billing github.com/acme/billing@v1.0.0 → github.com/acme/billing@v1.1.0")
	require.ErrorContains(t, err, "run `codefly deploy gitops render payments --env production` again")

	workspace.Modules = append(workspace.Modules, &resources.ModuleReference{Name: "ledger", Source: "github.com/acme/ledger", Version: "v2.0.0"})
	workspace.Modules[0].Version = "v1.0.0"
	err = holdRenderRecord(workspace, "payments", "production")
	require.ErrorContains(t, err, "ledger added at github.com/acme/ledger@v2.0.0")
}

// A whole-composition render that stopped before reaching a module records
// that module's render as failed, so its earlier tree is refused as well.
func TestPublishRefusesAModuleTheCompositionRenderNeverReached(t *testing.T) {
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	require.NoError(t, RecordRenderFailure(workspace.Dir(), "production", "payments", errors.New("render --all stopped at billing before rendering it")))

	err := holdRenderRecord(workspace, "payments", "production")
	require.ErrorContains(t, err, "failed at")
	require.ErrorContains(t, err, "render --all stopped at billing")
}

// A working render no record vouches for — rendered by a CLI that kept none,
// or put there by hand — is refused with the render that lifts it.
func TestPublishRefusesAWorkingRenderWithNoRecord(t *testing.T) {
	workspace := &resources.Workspace{Name: "payments"}
	workspace.WithDir(t.TempDir())
	err := holdRenderRecord(workspace, "payments", "production")
	require.ErrorContains(t, err, "no record of the render that produced it")
	require.ErrorContains(t, err, "run `codefly deploy gitops render payments --env production` again")
}

// A render records the pin judgement of the readiness gate it passed, and a
// later render that failed, or no gate at all, leaves none for publish to
// rely on: RenderedPinCurrency answers only for a render that finished.
func TestARenderRecordsThePinJudgementItPassed(t *testing.T) {
	root := t.TempDir()
	render := func(judged *PinCurrency) {
		t.Helper()
		_, err := RenderOwnedTree(context.Background(), &RenderOptions{
			Destination: workingrender.Module(root, "production", "payments"), Module: "payments", UnitNames: []string{"api"},
			OwnedPath:   filepath.ToSlash(filepath.Join("environments", "modules", "payments")),
			Units:       promotableServiceGraph("payments", []string{"api"}),
			Environment: "production", Namespace: "payments", AppProject: "payments", Promotable: true,
			PinCurrency: judged,
		}, func(_ context.Context, stage string) error {
			return writePublishServiceFixture(stage, "production", "api")
		})
		require.NoError(t, err)
	}

	render(&PinCurrency{Behind: []string{"saas pins owner/saas at tag 0.1.0: 1 release behind the newest, 0.2.0"}})
	require.Equal(t, &PinCurrency{Behind: []string{"saas pins owner/saas at tag 0.1.0: 1 release behind the newest, 0.2.0"}},
		RenderedPinCurrency(root, "production", "payments"))

	render(&PinCurrency{})
	require.Equal(t, &PinCurrency{}, RenderedPinCurrency(root, "production", "payments"), "every pin current or held")

	render(nil)
	require.Nil(t, RenderedPinCurrency(root, "production", "payments"), "no gate judged this render")

	render(&PinCurrency{})
	renderPublishFixtureFailing(t, root, "payments", "production")
	require.Nil(t, RenderedPinCurrency(root, "production", "payments"), "the last render failed: no judgement stands")
	require.Nil(t, RenderedPinCurrency(root, "production", "never-rendered"))
}

// RenderStands is publish's own first hold, asked alone: a whole-composition
// publish renders only when it fails.
func TestRenderStandsIsPublishsFirstHold(t *testing.T) {
	workspace := loadGitopsWorkspace(t, createBareRepository(t))
	env := selectedEnvironment(t, workspace, "production")
	require.ErrorContains(t, RenderStands(context.Background(), workspace, env, "payments"), "has never been rendered")

	renderPublishFixture(t, workspace.Dir(), "payments", "production", "api")
	require.NoError(t, RenderStands(context.Background(), workspace, env, "payments"))

	renderPublishFixtureFailing(t, workspace.Dir(), "payments", "production")
	require.ErrorContains(t, RenderStands(context.Background(), workspace, env, "payments"), "failed")
}

// A judgement that relied on a pin-hold stands through the hold's date and no
// longer after it, as the hold itself does.
func TestAPinJudgementEndsWithTheHoldItReliedOn(t *testing.T) {
	day := func(value string) time.Time {
		parsed, err := time.Parse("2006-01-02 15:04", value)
		require.NoError(t, err)
		return parsed
	}
	held := &PinCurrency{HeldUntil: "2026-11-16"}
	require.False(t, held.Expired(day("2026-11-16 23:00")), "a hold applies through its date")
	require.True(t, held.Expired(day("2026-11-17 01:00")))
	require.False(t, (&PinCurrency{}).Expired(day("2099-01-01 00:00")), "no hold, nothing to end")
	require.False(t, (*PinCurrency)(nil).Expired(day("2099-01-01 00:00")))
	require.True(t, (&PinCurrency{HeldUntil: "someday"}).Expired(day("2026-11-13 00:00")), "an unreadable date is not a judgement to rely on")
}
