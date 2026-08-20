package runbooks

import (
	"strings"
	"testing"
)

func TestNamesExcludesIndexAndIsSorted(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("expected at least one runbook")
	}
	prev := ""
	for _, n := range names {
		if n == "README" {
			t.Errorf("README should not be listed as a runbook topic")
		}
		if strings.HasSuffix(n, ".md") {
			t.Errorf("name %q should not carry the .md suffix", n)
		}
		if prev != "" && n < prev {
			t.Errorf("names not sorted: %q before %q", prev, n)
		}
		prev = n
	}
}

func TestGetKnownAndUnknown(t *testing.T) {
	// A runbook that must exist.
	r, err := Get("bump-go-version")
	if err != nil {
		t.Fatalf("Get(bump-go-version): %v", err)
	}
	if r.Name != "bump-go-version" {
		t.Errorf("name = %q, want bump-go-version", r.Name)
	}
	if !strings.Contains(r.Content, "# Runbook: Bump the Go version") {
		t.Errorf("content missing expected heading")
	}
	if r.Title == "" || r.Summary == "" {
		t.Errorf("title/summary should be populated, got title=%q summary=%q", r.Title, r.Summary)
	}
	if strings.HasPrefix(r.Title, "Runbook: ") {
		t.Errorf("title should have the 'Runbook: ' prefix stripped, got %q", r.Title)
	}

	// The .md suffix is accepted and normalized away.
	if r2, err := Get("bump-go-version.md"); err != nil || r2.Name != "bump-go-version" {
		t.Errorf("Get with .md suffix: name=%q err=%v", r2.Name, err)
	}

	if _, err := Get("does-not-exist"); err == nil {
		t.Errorf("expected error for unknown runbook")
	}
	// README is a real file so Get reads it; Names/List are what exclude it.
	if _, err := Get("README"); err != nil {
		t.Errorf("Get(README) should read the index file: %v", err)
	}
}

func TestListHasNoContentButHasSummary(t *testing.T) {
	for _, r := range List() {
		if r.Content != "" {
			t.Errorf("List() entries should omit Content, %q had it", r.Name)
		}
		if r.Summary == "" {
			t.Errorf("List() entry %q has empty summary", r.Name)
		}
	}
}

func TestFirstSentenceTrims(t *testing.T) {
	got := firstSentence("Do the thing across repos. Then verify it.")
	if got != "Do the thing across repos." {
		t.Errorf("firstSentence = %q", got)
	}
}
