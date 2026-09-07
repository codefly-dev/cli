package yamledit

import (
	"strings"
	"testing"
)

func TestSetMapValuePreservesCommentsAndPosition(t *testing.T) {
	src := `name: demo # keep me
color: blue
nested:
    a: 1
`
	doc, root, err := Document([]byte(src))
	if err != nil {
		t.Fatal(err)
	}

	// Replace an existing key: keeps its position and key-node comment.
	SetMapValue(root, "color", Scalar("red"))
	// Add a new key: appended.
	SetMapValue(root, "extra", Scalar("added"))
	// Edit a nested mapping.
	nested := EnsureMap(root, "nested")
	SetMapValue(nested, "b", Scalar("2"))

	out, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if !strings.Contains(got, "# keep me") {
		t.Errorf("lost line comment:\n%s", got)
	}
	if !strings.Contains(got, "color: red") {
		t.Errorf("color not replaced:\n%s", got)
	}
	if strings.Index(got, "color:") > strings.Index(got, "extra:") {
		t.Errorf("replaced key lost its position:\n%s", got)
	}
	if !strings.Contains(got, "b: \"2\"") && !strings.Contains(got, "b: 2") {
		t.Errorf("nested key not added:\n%s", got)
	}
}

func TestEndLineCountsBlockScalarLines(t *testing.T) {
	// The literal block scalar spans lines 2-4; EndLine must report 4 (its last
	// source line), not 2 (its start), so a splice does not strand lines 3-4.
	src := "name: azure\ndescription: |\n    one\n    two\nlast: end\n"
	doc, root, err := Document([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	desc := MapValue(root, "description")
	if got := EndLine(desc); got != 4 {
		t.Errorf("EndLine(block scalar) = %d, want 4", got)
	}
	// The whole document node's span reaches the last key on line 5.
	if got := EndLine(doc); got != 5 {
		t.Errorf("EndLine(doc) = %d, want 5", got)
	}
}

func TestDocumentRejectsNonMapping(t *testing.T) {
	if _, _, err := Document([]byte("- a\n- b\n")); err == nil {
		t.Fatal("expected error for non-mapping root")
	}
	if _, _, err := Document(nil); err == nil {
		t.Fatal("expected error for empty document")
	}
}
