package gitops

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/modulecontract"
)

var updateWire = flag.Bool("update-wire", false, "rewrite docs/wire from the Go types")

// TestWireShapesArePinnedByDigest is the stopgap for the two wire contracts
// this repository still implements — codefly/module-contract/v1 and
// codefly/cell/v1 — until core owns them: the shape of each, read off the Go
// types (every field by its wire name and type, nested), is written to
// docs/wire and pinned there by digest. A field added, renamed or retyped
// fails this test until `go test ./pkg/gitops -run WireShapes -update-wire`
// rewrites the description and docs/wire/README.md records the new digest —
// deliberately, and visibly to the host and infra-base sessions, which pin
// the same digests against their own readers.
func TestWireShapesArePinnedByDigest(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "wire")
	shapes := map[string]reflect.Type{
		"module-contract.v1": reflect.TypeOf(modulecontract.Contract{}),
		"cell.v1":            reflect.TypeOf(CellFile{}),
	}
	readme := "# Wire shapes pinned by digest\n\nThe two wire contracts this repository implements until core owns them\n(`codefly/module-contract/v1`, `codefly/cell/v1`), described field by field\nfrom the Go types and pinned by the SHA-256 of each description. A reader in\nanother repository pins the same digest; `TestWireShapesArePinnedByDigest`\nrefuses a change to either shape until the description and this digest are\nregenerated with `go test ./pkg/gitops -run WireShapes -update-wire`.\n\n"
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		description := describeWire(shapes[name])
		sum := sha256.Sum256([]byte(description))
		digest := hex.EncodeToString(sum[:])
		readme += fmt.Sprintf("- `%s.txt` — sha256 `%s`\n", name, digest)
		path := filepath.Join(root, name+".txt")
		if *updateWire {
			if err := os.WriteFile(path, []byte(description), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		pinned, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the wire shape %s is not pinned: %v (run with -update-wire)", name, err)
		}
		if string(pinned) != description {
			t.Fatalf("the wire shape %s changed; a wire contract changes deliberately: regenerate with -update-wire and tell the host and infra-base sessions the new digest\n--- pinned\n%s\n--- now\n%s", name, pinned, description)
		}
	}
	// What the digest pins is stated beside it, so the stopgap is never read
	// as more than it is.
	readme += "\nWhat the digest pins, and what it does not: the descriptions are read off the\nGo types' YAML tags, so the digest is a **structural drift detector** for the\ntagged fields — it pins the descriptions, not the behaviour. A custom decoder\nis outside it: `Ceiling.Actions` and `Ceiling.Scopes` carry no tags because\n`Ceiling.UnmarshalYAML` implements the scope-ceiling union itself (a sequence\nof bare actions, or a sequence of {resource_kind, actions} entries, never\nmixed and never a mapping, with their refusals), so a change to the forms\nthat decoder accepts, or to any refusal rule, leaves the digest unchanged. Those rules are held by `pkg/modulecontract`'s tests here and by\nnothing shared; the shared fixtures arrive with the move to core, which is\nthe item this stopgap stands in for.\n"
	if *updateWire {
		if err := os.WriteFile(filepath.Join(root, "README.md"), []byte(readme), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	pinned, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(pinned) != readme {
		t.Fatalf("docs/wire/README.md does not record the digests of the pinned shapes; regenerate with -update-wire")
	}
}

// describeWire writes a type's wire shape: every field with a yaml tag, by
// wire name and type, nested through structs, pointers, slices and maps, so
// two readers of the description agree on exactly what travels.
func describeWire(root reflect.Type) string {
	var lines []string
	var walk func(prefix string, kind reflect.Type)
	walk = func(prefix string, kind reflect.Type) {
		for kind.Kind() == reflect.Pointer {
			kind = kind.Elem()
		}
		switch kind.Kind() {
		case reflect.Struct:
			for i := 0; i < kind.NumField(); i++ {
				field := kind.Field(i)
				tag := field.Tag.Get("yaml")
				if tag == "" || tag == "-" {
					continue
				}
				name, options, _ := strings.Cut(tag, ",")
				if name == "" {
					name = strings.ToLower(field.Name)
				}
				optional := ""
				if strings.Contains(options, "omitempty") {
					optional = " (optional)"
				}
				lines = append(lines, prefix+name+": "+wireKind(field.Type)+optional)
				walk(prefix+name+".", field.Type)
			}
		case reflect.Slice, reflect.Array, reflect.Map:
			walk(prefix+"[]", kind.Elem())
		}
	}
	walk("", root)
	return strings.Join(lines, "\n") + "\n"
}

func wireKind(kind reflect.Type) string {
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	switch kind.Kind() {
	case reflect.Struct:
		return "object"
	case reflect.Slice, reflect.Array:
		return "list of " + wireKind(kind.Elem())
	case reflect.Map:
		return "map of " + wireKind(kind.Key()) + " to " + wireKind(kind.Elem())
	default:
		return kind.Kind().String()
	}
}
