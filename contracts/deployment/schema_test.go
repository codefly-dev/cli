package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFrozenSchema(t *testing.T) {
	for _, context := range []bool{false, true} {
		name := "inventory.schema.json"
		if context {
			name = "context.schema.json"
		}
		if !bytes.Equal(read(t, name), SchemaJSON(context)) {
			t.Fatalf("%s differs from closed wire types; regenerate deliberately", name)
		}
	}
}
func TestSchemaRefusesOverlap(t *testing.T) {
	for _, name := range []string{"overlap-different-keys", "overlap-subset", "carrier-overlap"} {
		err := CheckSchema(read(t, "testdata/generated/"+name+".input.json"))
		var v *Violation
		if !errors.As(err, &v) || v.Rule != "SELECTOR_OVERLAP" {
			t.Fatal(err)
		}
	}
}
func TestSchemaClosesEveryObject(t *testing.T) {
	var base any
	_ = json.Unmarshal(read(t, "testdata/base.json"), &base)
	var visit func(any, []any)
	visit = func(value any, path []any) {
		switch x := value.(type) {
		case map[string]any:
			// Open label/annotation/image maps are values, not field-bearing objects.
			if len(path) > 0 {
				last := path[len(path)-1]
				if last == "labels" || last == "selector" || last == "annotations" || last == "nodeSelector" {
					return
				}
			}
			copyBytes, _ := json.Marshal(base)
			var clone any
			_ = json.Unmarshal(copyBytes, &clone)
			node := clone
			for _, key := range path {
				switch k := key.(type) {
				case string:
					node = node.(map[string]any)[k]
				case int:
					node = node.([]any)[k]
				}
			}
			node.(map[string]any)["unsupported_field"] = true
			input, _ := json.Marshal(clone)
			err := CheckSchema(input)
			var v *Violation
			if !errors.As(err, &v) || v.Rule != "SCHEMA_UNKNOWN" {
				t.Fatalf("open object at %v: %v", path, err)
			}
			for _, k := range sortedKeys(x) {
				visit(x[k], append(append([]any{}, path...), k))
			}
		case []any:
			for i, v := range x {
				visit(v, append(append([]any{}, path...), i))
			}
		}
	}
	visit(base, nil)
}
func TestCorpusRegeneratesExactly(t *testing.T) {
	temp := t.TempDir()
	for _, name := range []string{"base.json", "base-context.json", "mutations.json", "generate.py"} {
		if err := os.WriteFile(filepath.Join(temp, name), read(t, filepath.Join("testdata", name)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("python3", filepath.Join(temp, "generate.py"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	var expected, actual fixtureManifest
	_ = json.Unmarshal(read(t, "testdata/manifest.json"), &expected)
	_ = json.Unmarshal(read(t, filepath.Join(temp, "manifest.json")), &actual)
	if !reflect.DeepEqual(expected, actual) {
		t.Fatal("manifest does not regenerate")
	}
	for _, f := range expected.Cases {
		files := []string{f.Input, f.Context}
		if f.Canonical != nil {
			files = append(files, *f.Canonical)
		}
		for _, file := range files {
			if !bytes.Equal(read(t, filepath.Join("testdata", file)), read(t, filepath.Join(temp, file))) {
				t.Fatalf("generated file drift: %s", file)
			}
		}
	}
}

// The projection must be reachable from a schema-checked inventory ALONE,
// without a validation context.
//
// Why this is a test and not a convenience: the row projection is a pure
// function of Workloads — namespace, selector, service account, authenticating
// container, the container-name-to-image map, and application/init membership.
// A consumer holding an inventory that was already approved, but no longer
// holding the context that approval was checked against, must be able to ask
// the contract for its rows. If it cannot, it writes its own traversal of
// Workloads[].Template.Spec, which is a second implementation of the
// projection — the duplication this module exists to prevent. The first
// consumer (the platform Catalogue) hit exactly that and said so.
//
// It also pins the two forms to AGREE: Check and Validate must produce
// identical canonical bytes, identical digests and identical rows, or an
// approval signed over one and compared against the other would disagree for
// no visible reason.
func TestTheProjectionIsReachableWithoutAContext(t *testing.T) {
	inventory := read(t, "testdata/base.json")

	checked, err := Check(inventory)
	if err != nil {
		t.Fatalf("Check on the base inventory: %v", err)
	}
	rows := checked.Rows()
	if len(rows) == 0 {
		t.Fatal("a schema-checked inventory projected no rows, so a consumer cannot use the contract's own projection")
	}
	for _, r := range rows {
		if r.Namespace == "" || r.ServiceAccount == "" {
			t.Errorf("row is missing an authoritative field: %+v", r)
		}
		// The authenticating container is the designation that CANNOT be
		// inferred from the image map, so a row that has lost it has lost the
		// thing admission needs most.
		if r.Container != nil {
			if _, ok := r.Images[*r.Container]; !ok {
				t.Errorf("authenticating container %q is absent from the image map: %+v", *r.Container, r)
			}
		}
		if len(r.Images) == 0 || len(r.App) == 0 {
			t.Errorf("row carries no container membership: %+v", r)
		}
		for _, name := range append(append([]string{}, r.App...), r.Init...) {
			if _, ok := r.Images[name]; !ok {
				t.Errorf("container %q is in membership but absent from the image map: %+v", name, r)
			}
		}
	}

	// And the two forms agree, so a digest signed over one matches the other.
	validated, err := Validate(inventory, read(t, "testdata/base-context.json"))
	if err != nil {
		t.Fatalf("Validate on the base inventory and context: %v", err)
	}
	if checked.Digest() != validated.Digest() {
		t.Errorf("Check and Validate disagree on the digest: %s vs %s", checked.Digest(), validated.Digest())
	}
	if !bytes.Equal(checked.Canonical(), validated.Canonical()) {
		t.Error("Check and Validate disagree on the canonical bytes")
	}
	if a, b := mustJSON(t, checked.Rows()), mustJSON(t, validated.Rows()); !bytes.Equal(a, b) {
		t.Errorf("Check and Validate disagree on the projection:\n  check:    %s\n  validate: %s", a, b)
	}

	// A refused inventory yields no projection at all: the type is the gate.
	if _, err := Check([]byte(`{"schema":"wrong"}`)); err == nil {
		t.Error("an inventory with an unsupported schema was accepted by Check")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
