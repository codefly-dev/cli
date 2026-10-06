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
