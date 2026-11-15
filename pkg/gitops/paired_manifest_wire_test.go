package gitops

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/solutionhost"
	"github.com/stretchr/testify/require"
)

// pairedManifestFloor is the oldest core whose module manifest decoder reads
// every field this CLI renders. The paired delivery image decodes the signed
// manifest strictly (solutionhost.ManifestFromVerified), so its core must be
// at least this one (TestThePairedDeliveryImageDecodesWhatThisCLIRenders).
// It is the manifest's WIRE, not the CLI's core: a core release that leaves
// the manifest's fields alone — an authorization rule, a render moved to the
// authority library — moves neither this floor nor the image. Raise it, and
// publish a companion built from a core at least this one, exactly when
// TestTheModuleManifestWireIsFrozenAtTheFloor fails.
const pairedManifestFloor = "v0.19.30"

// The module manifest's fields, as its JSON names them, are frozen at
// pairedManifestFloor in testdata/paired-manifest-fields.golden. A field core
// adds, renames or drops is a manifest a paired image on an older core refuses
// ("field … not found"), so this fails first: raise pairedManifestFloor to the
// core that changed it, publish a codefly companion from this checkout, pin its
// digest as pairedDeliveryImage, and write the new field set here.
func TestTheModuleManifestWireIsFrozenAtTheFloor(t *testing.T) {
	var fields []string
	jsonFields(reflect.TypeFor[solutionhost.ModuleManifest](), "", map[reflect.Type]bool{}, &fields)
	slices.Sort(fields)
	got := strings.Join(fields, "\n") + "\n"
	want, err := os.ReadFile(filepath.Join("testdata", "paired-manifest-fields.golden"))
	require.NoError(t, err)
	require.Equal(t, string(want), got,
		"core's module manifest encoding moved since %s: a paired image on that core refuses it. Raise pairedManifestFloor, publish a codefly companion, pin it as pairedDeliveryImage, and update the golden", pairedManifestFloor)
}

// jsonFields appends every JSON field path of t under prefix, descending into
// structs, pointers, slices and maps, each type once per path.
func jsonFields(t reflect.Type, prefix string, seen map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	defer delete(seen, t)
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		*out = append(*out, path)
		jsonFields(field.Type, path, seen, out)
	}
}
