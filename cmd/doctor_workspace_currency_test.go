package cmd

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/codefly-dev/cli/cmd/deploy"
	"github.com/codefly-dev/cli/pkg/composition"
	"github.com/stretchr/testify/require"
)

func currencySource(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...).CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	var commits []string
	for range 4 {
		git("commit", "-q", "--allow-empty", "-m", "c")
		commits = append(commits, git("rev-parse", "HEAD"))
	}
	git("tag", "--no-sign", "v1.0.0", commits[0])
	git("tag", "--no-sign", "v1.1.0", commits[3])
	return "file://" + dir, commits
}

func TestJudgePin(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	source, commits := currencySource(t)
	ctx := context.Background()
	head, err := composition.ReadSourceHead(ctx, source)
	require.NoError(t, err)
	judge := func(version string) currencyVerdict {
		return judgePin(ctx, &currencyPin{label: "m", name: "m", source: source, version: version}, head)
	}

	require.True(t, judge(commits[3]).current, "the tip is current")
	behind := judge(commits[1])
	require.False(t, behind.current)
	require.Contains(t, behind.detail, "2 commits behind main")
	require.True(t, judge("1.1.0").current, "the newest release is current")
	older := judge("1.0.0")
	require.False(t, older.current)
	require.Contains(t, older.detail, "1 release behind the newest, 1.1.0")
	require.True(t, judge(">=1.0.0").current, "a range that reaches the newest release")
	require.False(t, judge("~1.0").current, "a range that cannot")
	require.True(t, judge("").current)
	require.Contains(t, judge(strings.Repeat("b", 40)).detail, "not in the history")
	ahead := judge("2.0.0-rc1")
	require.False(t, ahead.current)
	require.Contains(t, ahead.detail, "newer than the newest stable release, 1.1.0")
}

func TestPinHoldsNeedAReasonAnEndDateAndANamedPin(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	names := []string{"saas"}
	report := &workspaceReadinessReport{Status: readinessStatusReady}
	holds := []pinHold{
		{Name: "saas", Pin: "v1", Reason: "waiting on QA", Until: "2026-11-20"},
		{Name: "saas", Pin: "v1", Reason: "", Until: "2026-11-20"},
		{Name: "saas", Pin: "v1", Reason: "old", Until: "2026-11-01"},
		{Name: "saas", Pin: "v1", Reason: "no date"},
		{Name: "ghost", Pin: "v1", Reason: "names nothing", Until: "2026-11-20"},
	}
	usable := validPinHolds(holds, names, now, report)
	require.Equal(t, []bool{true, false, false, false, false}, usable)
	require.Equal(t, readinessStatusNotReady, report.Status, "an unusable hold fails the run")

	hold, index := matchingHold(holds, usable, &currencyPin{name: "saas", version: "v1"})
	require.Equal(t, 0, index)
	require.Equal(t, "waiting on QA", hold.Reason)

	hold, _ = matchingHold(holds, usable, &currencyPin{name: "saas", version: "v2"})
	require.Nil(t, hold, "a hold names the exact pin: moving the pin ends it")
}

const currencyWorkspace = "name: solution\nlayout: modules\nmodule-resolution:\n    saas: git\nmodules:\n    - name: saas\n      source: owner/saas\n      version: \"%s\"\n%s"

func stubSource(t *testing.T, head *composition.SourceHead, err error) {
	t.Helper()
	previous := readSourceHead
	readSourceHead = func(context.Context, string) (*composition.SourceHead, error) { return head, err }
	t.Cleanup(func() { readSourceHead = previous })
}

func TestDoctorWorkspaceReportsAStalePinAndRefusesItWhenRequired(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	stubSource(t, &composition.SourceHead{Branch: "main", Tip: strings.Repeat("c", 40), Tags: []string{"v0.2.0"}}, nil)
	dir := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": fmt.Sprintf(currencyWorkspace, "0.1.0", "")})

	report := runReadiness(t, workspaceReadinessOptions{dir: dir})
	diag := requireCode(t, report, codePinStale, "warn")
	require.Contains(t, diag.Message, "1 release behind the newest, 0.2.0")
	require.Empty(t, failedPinChecks(report), "locally a stale pin is a warning")

	report = runReadiness(t, workspaceReadinessOptions{dir: dir, requireCurrentPins: true})
	requireCode(t, report, codePinStale, "fail")
	require.NotEmpty(t, failedPinChecks(report), "a render refuses it")
}

func TestDoctorWorkspaceShowsAHeldPinAndStillRefusesAnUnheldOne(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	stubSource(t, &composition.SourceHead{Branch: "main", Tip: strings.Repeat("c", 40), Tags: []string{"v0.2.0"}}, nil)
	hold := "pin-holds:\n    - name: saas\n      pin: \"0.1.0\"\n      reason: QA of 0.2.0 is not done\n      until: 2999-01-01\n"
	dir := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": fmt.Sprintf(currencyWorkspace, "0.1.0", hold)})
	report := runReadiness(t, workspaceReadinessOptions{dir: dir, requireCurrentPins: true})
	diag := requireCode(t, report, codePinHeld, "warn")
	require.Contains(t, diag.Message, "QA of 0.2.0 is not done")
	require.Empty(t, failedPinChecks(report), "a stated hold passes even a render")

	// Moving the pin ends the hold: it names the exact value it holds.
	moved := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": fmt.Sprintf(currencyWorkspace, "0.1.5", hold)})
	report = runReadiness(t, workspaceReadinessOptions{dir: moved, requireCurrentPins: true})
	requireCode(t, report, codePinStale, "fail")
	requireCode(t, report, codePinHoldInvalid, "warn")
}

func TestDoctorWorkspaceNeverPassesPinsItCouldNotCompare(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	stubSource(t, nil, errors.New("no network"))
	dir := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": fmt.Sprintf(currencyWorkspace, "0.1.0", "")})

	diag := requireCode(t, runReadiness(t, workspaceReadinessOptions{dir: dir}), codePinCurrencyUnverified, "warn")
	require.Contains(t, diag.Message, "NOT verified")

	report := runReadiness(t, workspaceReadinessOptions{dir: dir, requireCurrentPins: true})
	requireCode(t, report, codePinCurrencyUnverified, "fail")
}

// failedPinChecks lists the pin-currency checks that failed: the rest of the
// readiness report (a module nobody materialized in a temp dir) is not theirs.
func failedPinChecks(report *workspaceReadinessReport) []string {
	var failed []string
	for _, check := range report.Checks {
		if strings.HasPrefix(check.Code, "pin_") && check.Status == checkStatusFail {
			failed = append(failed, check.Code)
		}
	}
	return failed
}

func TestDoctorWorkspaceNarrowedToOneModuleKeepsEveryHoldValid(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	stubSource(t, &composition.SourceHead{Branch: "main", Tip: strings.Repeat("c", 40), Tags: []string{"v0.2.0"}}, nil)
	two := "name: solution\nlayout: modules\nmodule-resolution:\n    saas: git\n    wiki: git\nmodules:\n" +
		"    - name: saas\n      source: owner/saas\n      version: \"0.2.0\"\n" +
		"    - name: wiki\n      source: owner/wiki\n      version: \"0.1.0\"\n" +
		"pin-holds:\n    - name: wiki\n      pin: \"0.1.0\"\n      reason: held\n      until: 2999-01-01\n"
	dir := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": two})

	report := runReadiness(t, workspaceReadinessOptions{dir: dir, module: "saas", requireCurrentPins: true})
	require.Empty(t, failedPinChecks(report), "rendering saas must not trip over a hold on wiki, nor judge wiki")
	require.Nil(t, findCheck(report, "pin currency of wiki"))
	require.NotNil(t, findCheck(report, "pin currency of saas"))
}

func TestRenderGateRefusesAStalePinAndShowsAHeldOne(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	stubSource(t, &composition.SourceHead{Branch: "main", Tip: strings.Repeat("c", 40), Tags: []string{"v0.2.0"}}, nil)
	dir := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": fmt.Sprintf(currencyWorkspace, "0.1.0", "")})
	t.Chdir(dir)

	err := readinessError(deploy.WorkspaceReadiness(context.Background(), deploy.ReadinessQuestion{Env: "local", Module: "", Enforcing: true, JudgePins: true}))
	require.Error(t, err)
	require.Contains(t, err.Error(), codePinStale, "the delivery gate must be strict about a stale pin")
}

// An abbreviated SHA resolves nowhere: git resolves one locally and a remote does
// not, so every clone reads it as a ref name and the render dies with
// "Remote branch <pin> not found in upstream origin".
//
// The second case is the one that matters, and the first version of this check
// got it wrong. A commit pin is exactly what a source that has not cut a tag yet
// is pinned by, and the release comparison answers "no release to compare with"
// as CURRENT — so judged below it, an unresolvable pin on a tagless source
// reported GREEN and the render still died fifteen minutes later.
func TestJudgePinNamesAnAbbreviatedCommitSHA(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	source, commits := currencySource(t)
	ctx := context.Background()
	head, err := composition.ReadSourceHead(ctx, source)
	require.NoError(t, err)
	judge := func(h *composition.SourceHead, version string) currencyVerdict {
		return judgePin(ctx, &currencyPin{label: "m", name: "m", source: source, version: version}, h)
	}

	short := commits[3][:12]
	verdict := judge(head, short)
	require.False(t, verdict.current, "an abbreviated SHA resolves nowhere, so it is never current")
	require.Contains(t, verdict.detail, "abbreviated commit SHA")
	require.Contains(t, verdict.detail, "Use the full SHA")

	// The regression: a source that publishes no release must still be judged.
	tagless := &composition.SourceHead{Branch: head.Branch, Tip: head.Tip}
	verdict = judge(tagless, short)
	require.False(t, verdict.current, "a tagless source must not report an unresolvable pin as current")
	require.Contains(t, verdict.detail, "abbreviated commit SHA")

	// A tag whose name happens to be hex is a tag, not a near miss.
	hexTag := &composition.SourceHead{Branch: head.Branch, Tip: head.Tip, Tags: []string{"deadbeef"}}
	require.NotContains(t, judge(hexTag, "deadbeef").detail, "abbreviated commit SHA")

	// The full object name is a commit pin and judged against the branch.
	require.True(t, judge(head, commits[3]).current, "the tip is current")
}

// readinessError is the error a delivery verb's readiness question answered.
func readinessError(_ deploy.ReadinessVerdict, err error) error { return err }

// The engine judges the pins only when the question asks it to: a render's
// gate judges them and hands back the ones behind (recorded with the render),
// and a publication of a render that recorded its judgement does not judge
// them again against a source that moved since.
func TestTheDeliveryGateJudgesPinsOnlyWhenAsked(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	stubSource(t, &composition.SourceHead{Branch: "main", Tip: strings.Repeat("c", 40), Tags: []string{"v0.2.0"}}, nil)
	dir := writeTestWorkspace(t, map[string]string{"workspace.codefly.yaml": fmt.Sprintf(currencyWorkspace, "0.1.0", "")})
	t.Chdir(dir)
	ctx := context.Background()

	verdict, err := deploy.WorkspaceReadiness(ctx, deploy.ReadinessQuestion{Env: "local", JudgePins: true})
	require.ErrorContains(t, err, codePinStale)
	require.True(t, verdict.PinsJudged)
	require.Len(t, verdict.Behind, 1, "the pin behind its source is handed back for the render to record")
	require.Contains(t, verdict.Behind[0], "behind the newest")

	// The fixture is not materialized, so the gate still fails — on that, and
	// not on the pin.
	verdict, err = deploy.WorkspaceReadiness(ctx, deploy.ReadinessQuestion{Env: "local", Enforcing: true})
	require.Error(t, err)
	require.NotContains(t, err.Error(), codePinStale, "a publication held to its render's judgement does not judge the pins again")
	require.False(t, verdict.PinsJudged)
	require.Empty(t, verdict.Behind)
}

// A workspace that fails before the pin comparison runs has judged no pin,
// whatever the question asked, so a render under the override records none.
func TestNoPinIsJudgedWhenTheComparisonNeverRan(t *testing.T) {
	t.Setenv("CODEFLY_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	verdict, err := deploy.WorkspaceReadiness(context.Background(), deploy.ReadinessQuestion{Env: "local", JudgePins: true})
	require.Error(t, err)
	require.False(t, verdict.PinsJudged)
}
