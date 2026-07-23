package control

import (
	"context"
	"strings"
	"testing"
)

const fixtureFile = "modules/backend/services/api/hello.txt"

func TestSourceFileRoundTrip(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	p := New()

	if err := p.WriteFile(ctx, fixtureFile, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	data, err := p.ReadFile(ctx, fixtureFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi" {
		t.Errorf("read = %q, want hi", data)
	}

	// CreateFile must refuse to overwrite an existing file.
	if err := p.CreateFile(ctx, fixtureFile, []byte("x")); err == nil {
		t.Error("CreateFile should refuse to overwrite an existing file")
	}
}

func TestApplyEditReplacesAndReportsMisses(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	p := New()

	if err := p.WriteFile(ctx, fixtureFile, []byte("hi there")); err != nil {
		t.Fatal(err)
	}
	if err := p.ApplyEdit(ctx, Edit{Path: fixtureFile, OldText: "hi", NewText: "bye"}); err != nil {
		t.Fatal(err)
	}
	data, _ := p.ReadFile(ctx, fixtureFile)
	if string(data) != "bye there" {
		t.Errorf("after edit = %q, want %q", data, "bye there")
	}

	// A non-matching edit must error, never pass silently.
	if err := p.ApplyEdit(ctx, Edit{Path: fixtureFile, OldText: "zzz", NewText: "x"}); err == nil {
		t.Error("ApplyEdit should error when OldText is not found")
	}
}

func TestSearchFindsContent(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	p := New()

	if err := p.WriteFile(ctx, fixtureFile, []byte("needle in a haystack")); err != nil {
		t.Fatal(err)
	}
	hits, err := p.Search(ctx, SearchRequest{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one search hit")
	}
	if !strings.Contains(hits[0].Text, "needle") {
		t.Errorf("hit text = %q, want it to contain needle", hits[0].Text)
	}
}

func TestSourceRejectsPathEscape(t *testing.T) {
	t.Chdir(writeWorkspace(t))
	ctx := context.Background()
	if _, err := New().ReadFile(ctx, "../../../etc/passwd"); err == nil {
		t.Error("ReadFile should reject a path escaping the workspace root")
	}
}
