package gitops

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	solutionhost "github.com/codefly-dev/authority/libraries/solutionhost/go"
	modulecontract "github.com/codefly-dev/core/contracts/module"
	"github.com/stretchr/testify/require"
)

// pairedManifestFloor is the oldest core whose module manifest decoder reads
// every manifest this CLI renders. The paired delivery image decodes the
// signed manifest strictly (solutionhost.ManifestFromVerified: strict decode,
// Validate, and its canonical bytes compared with the signed ones), so its
// core must be at least this one
// (TestThePairedDeliveryImageDecodesWhatThisCLIRenders). It is the manifest's
// WIRE, not the CLI's core: a core release that leaves the manifest alone — an
// authorization rule, a render moved to the authority library — moves neither
// this floor nor the image. Raise it, and publish a companion built from a
// core at least this one, exactly when TestTheModuleManifestWireIsFrozenAtTheFloor
// fails.
const pairedManifestFloor = "v0.19.30"

// updateWire writes the golden again, only together with raising
// pairedManifestFloor and publishing a companion.
var updateWire = flag.Bool("update-wire", false, "write testdata/paired-manifest-wire.golden from this core")

// The module manifest's wire is frozen at pairedManifestFloor in
// testdata/paired-manifest-wire.golden, in the three ways a paired image on an
// older core refuses what a newer one renders:
//
//   - its fields, each with its whole JSON tag (a field added, renamed or
//     dropped is "field … not found"; an omitempty added or removed changes
//     the bytes the image re-encodes and compares);
//   - the closed value sets its decoder enforces, read from their owners
//     declarations: the binding operations, the edge presences, the kinds and
//     the schema (a value allowed later is refused by an older image);
//   - the canonical bytes of one manifest exercising every section
//     (normalisation, ordering, any encoding change).
//
// When it fails: raise pairedManifestFloor to the core that changed it,
// publish a codefly companion from this checkout, pin its digest as
// pairedDeliveryImage, and write the golden again.
func TestTheModuleManifestWireIsFrozenAtTheFloor(t *testing.T) {
	var lines []string
	var fields []string
	jsonFields(reflect.TypeFor[solutionhost.ModuleManifest](), "", map[reflect.Type]bool{}, &fields)
	slices.Sort(fields)
	for _, field := range fields {
		lines = append(lines, "field "+field)
	}
	lines = append(lines, coreValueSets(t)...)
	payload, err := solutionhost.SignedPayload(wireManifest())
	require.NoError(t, err, "the representative manifest is a valid one")
	lines = append(lines, "canonical "+string(payload))
	got := strings.Join(lines, "\n") + "\n"
	golden := filepath.Join("testdata", "paired-manifest-wire.golden")
	if *updateWire {
		require.NoError(t, os.WriteFile(golden, []byte(got), 0o644))
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	require.Equal(t, string(want), got,
		"the module manifest wire moved since %s: a paired image on that core refuses it. Raise pairedManifestFloor, publish a codefly companion, pin it as pairedDeliveryImage, and update the golden", pairedManifestFloor)
}

// wireManifest is a manifest with every section the CLI renders filled:
// identity, host, release, workloads, its group, a federated page with its
// payload (a bundled page is the other spelling of the same section), its
// endpoints, and an authority block with every list and every operation.
func wireManifest() *solutionhost.ModuleManifest {
	return &solutionhost.ModuleManifest{
		Schema: solutionhost.SchemaModuleManifestV1, Binding: "ws.aurora.shop", Alias: "shop", Kind: solutionhost.KindSolution, Generation: 3,
		Host:      solutionhost.HostTarget{Coordinate: "local", Domain: "local"},
		Release:   solutionhost.Release{Publisher: "acme", Name: "shop", Version: "1.2.3"},
		Workloads: []solutionhost.Workload{{Service: "api", Container: "app"}, {Service: "worker", Container: "run"}},
		Group:     &solutionhost.SolutionGroup{ID: "operations", Title: "Operations", Order: 2},
		Frontend: &solutionhost.Frontend{Title: "Shop", Order: 10, ExposedModule: "./Page", HostContract: 1,
			Payload: &solutionhost.FrontendPayload{Artifact: FrontendArtifactName, Digest: solutionhost.PayloadDigest("sha256:" + strings.Repeat("e", 64)), MediaType: "application/x-tar"}},
		Endpoints: []solutionhost.Endpoint{{Name: "grpc", Service: "api", API: "grpc", Address: "http://api.shop.svc.cluster.local:9090"}},
		Authority: &solutionhost.Authority{
			Principal: "shop", InstallationRole: "admin",
			EdgeDependencies: []modulecontract.EdgeDependency{{Module: "sidekick", Presence: modulecontract.PresenceRequired}, {Module: "notes", Presence: modulecontract.PresenceOptional}},
			SourceGovernance: []string{"shop.item:write"},
			Bindings: []solutionhost.AuthorityBinding{
				{ID: "store", Audience: "store", Operations: map[string][]string{
					modulecontract.OperationInvoke: {"store.item:read", "store.item:write"}, modulecontract.OperationLookup: {"store.item:read"}, modulecontract.OperationHeadless: {"store.item:read"}}},
				{ID: "self", Audience: "shop", Operations: map[string][]string{modulecontract.OperationInvoke: {"shop.item:read"}}},
			},
			Queues: []string{"orders"}, Namespaces: []string{"shop"}, ResourceKinds: []string{"shop.item"},
		},
	}
}

// coreValueSets are the closed value sets core's manifest decoder enforces,
// read from the host library's and core's source at the versions this CLI builds with: the
// constants that spell them and the lists it checks against, each resolved to
// its values.
func coreValueSets(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/codefly-dev/core").Output()
	require.NoError(t, err, "locate core's source")
	dir := strings.TrimSpace(string(out))
	contract := packageConsts(t, filepath.Join(dir, "contracts", "module"))
	// The manifest's own types and value sets are the host's library's.
	out, err = exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/codefly-dev/authority/libraries/solutionhost/go").Output()
	require.NoError(t, err, "locate the host library's source")
	host := packageConsts(t, strings.TrimSpace(string(out)))
	var lines []string
	for _, set := range []struct {
		pkg    string
		consts map[string]string
		prefix string
	}{{"modulecontract", contract.consts, "Operation"}, {"modulecontract", contract.consts, "Presence"}, {"solutionhost", host.consts, "Kind"}, {"solutionhost", host.consts, "Schema"}} {
		for _, name := range slices.Sorted(maps.Keys(set.consts)) {
			if strings.HasPrefix(name, set.prefix) {
				lines = append(lines, "const "+set.pkg+"."+name+" = "+strconv.Quote(set.consts[name]))
			}
		}
	}
	for _, name := range []string{"operations", "kinds"} {
		list, ok := host.lists[name]
		require.True(t, ok, "the host library's solutionhost declares no %s list: the decoder's value set moved", name)
		var values []string
		for _, element := range list {
			switch e := element.(type) {
			case *ast.BasicLit:
				value, _ := strconv.Unquote(e.Value)
				values = append(values, value)
			case *ast.Ident:
				values = append(values, host.consts[e.Name])
			case *ast.SelectorExpr:
				values = append(values, contract.consts[e.Sel.Name])
			default:
				t.Fatalf("solutionhost.%s holds %T, which this reader cannot resolve", name, element)
			}
		}
		lines = append(lines, "var solutionhost."+name+" = "+strings.Join(values, " "))
	}
	return lines
}

type declarations struct {
	consts map[string]string
	lists  map[string][]ast.Expr
}

// packageConsts reads a package's string constants and its composite-literal
// variables from its non-test source.
func packageConsts(t *testing.T, dir string) declarations {
	t.Helper()
	d := declarations{consts: map[string]string{}, lists: map[string][]ast.Expr{}}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					switch v := value.Values[i].(type) {
					case *ast.BasicLit:
						if gen.Tok == token.CONST && v.Kind == token.STRING {
							d.consts[name.Name], _ = strconv.Unquote(v.Value)
						}
					case *ast.CompositeLit:
						if gen.Tok == token.VAR {
							d.lists[name.Name] = v.Elts
						}
					}
				}
			}
		}
	}
	return d
}

// jsonFields appends every field path of t under prefix with its whole JSON
// tag, descending into structs, pointers, slices and maps, each type once per
// path.
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
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
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
		*out = append(*out, path+" `"+tag+"`")
		jsonFields(field.Type, path, seen, out)
	}
}
