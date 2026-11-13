package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/workingrender"
)

// writeRecord leaves the render record a render of saas for blue would,
// carrying pinCurrency (raw JSON, "" for none).
func writeRecord(t *testing.T, dir, outcome, pinCurrency string) {
	t.Helper()
	path := workingrender.Record(dir, "blue", "saas")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"module":"saas","environment":"blue","outcome":"` + outcome + `"`
	if pinCurrency != "" {
		record += `,"pinCurrency":` + pinCurrency
	}
	if err := os.WriteFile(path, []byte(record+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The 2026-11-13 Blue deploy: a render judged every pin current, and while it
// ran (6–16 minutes) other merges moved a pinned source's main, so publish —
// judging the pins again — refused the render's own commits as "N commits
// behind main", and the round was rescued with hand-written pin-holds. A
// publication of a render that recorded its judgement is held to that
// judgement: the pins are not judged again.
func TestPublishIsHeldToThePinJudgementItsRenderRecorded(t *testing.T) {
	withSkip(t, false)
	stubReadiness(t, func(context.Context, string, string) error { return nil })
	dir := t.TempDir()
	writeRecord(t, dir, "rendered", `{}`)

	if err := publishReady(context.Background(), "publish", dir, "blue", "saas"); err != nil {
		t.Fatalf("a render that judged its pins current was refused: %v", err)
	}
	if len(judged) != 1 || judged[0] {
		t.Fatalf("the publication judged the pins again (%v); the render's judgement binds", judged)
	}
}

// What the safety was for stays: a render that ran under the override on
// pins behind their sources recorded them, and its publication refuses them
// by name — and lets them through only under the same override.
func TestPublishRefusesPinsItsRenderFoundBehind(t *testing.T) {
	withSkip(t, false)
	stubReadiness(t, func(context.Context, string, string) error { return nil })
	dir := t.TempDir()
	writeRecord(t, dir, "rendered", `{"behind":["saas pins owner/saas at commit 49d5158e: 4 commits behind main (188b1fb2)"]}`)

	err := publishReady(context.Background(), "publish", dir, "blue", "saas")
	if err == nil {
		t.Fatal("a render on pins behind their sources was published")
	}
	for _, want := range []string{"4 commits behind main", "render again", "refusing to publish", "--" + SkipWorkspaceReadinessFlag} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}

	withSkip(t, true)
	lines := captureWarnings(t)
	if err = publishReady(context.Background(), "publish", dir, "blue", "saas"); err != nil {
		t.Fatalf("the override must let the publication proceed: %v", err)
	}
	if !strings.Contains(joined(lines), "4 commits behind main") {
		t.Errorf("the override proceeded without naming the pins it lets through:\n%s", joined(lines))
	}
}

// A render that recorded no judgement — an older CLI's, or no record at all
// (publish itself refuses that one) — has its pins judged now, as before.
func TestPublishJudgesThePinsOfARenderThatRecordedNoJudgement(t *testing.T) {
	withSkip(t, false)
	for name, prepare := range map[string]func(dir string){
		"no record":         func(string) {},
		"no judgement":      func(dir string) { writeRecord(t, dir, "rendered", "") },
		"a failed render":   func(dir string) { writeRecord(t, dir, "failed", `{}`) },
		"an unfinished one": func(dir string) { writeRecord(t, dir, "rendering", `{}`) },
	} {
		t.Run(name, func(t *testing.T) {
			stubReadiness(t, func(context.Context, string, string) error { return nil })
			dir := t.TempDir()
			prepare(dir)
			if err := publishReady(context.Background(), "plan", dir, "blue", "saas"); err != nil {
				t.Fatal(err)
			}
			if len(judged) != 1 || !judged[0] {
				t.Fatalf("the pins of a render with no judgement were not judged (%v)", judged)
			}
		})
	}
}

// A render records what its gate judged: nothing when no gate ran, and the
// verdict otherwise.
func TestARenderCarriesItsGatesPinJudgement(t *testing.T) {
	withSkip(t, false)
	stubReadiness(t, func(context.Context, string, string) error { return nil })
	pins, err := requireWorkspaceReady(context.Background(), "render", "blue", "saas")
	if err != nil || pins == nil || len(pins.Behind) != 0 {
		t.Fatalf("a passing render gate judged %+v (%v), want every pin current", pins, err)
	}
	if len(judged) != 1 || !judged[0] {
		t.Fatalf("a render did not ask to judge its pins (%v)", judged)
	}

	previous := WorkspaceReadiness
	WorkspaceReadiness = nil
	t.Cleanup(func() { WorkspaceReadiness = previous })
	if pins, err = requireWorkspaceReady(context.Background(), "render", "blue", "saas"); err != nil || pins != nil {
		t.Fatalf("with no engine nothing was judged, yet the render carries %+v (%v)", pins, err)
	}
}
