package deployment

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// An explicit production-import allowlist prevents indirect network escape via
// subprocesses, plugins, cgo, syscall or a newly introduced standard-library
// transport. All source files are scanned, including inactive build tags.
var productionImports = map[string]bool{
	"bytes": true, "crypto/sha256": true, "encoding/hex": true, "encoding/json": true,
	"errors": true, "fmt": true, "io": true, "os": true, "path": true, "reflect": true,
	"regexp": true, "slices": true, "sort": true, "strconv": true, "strings": true, "unicode/utf8": true,
	"github.com/codefly-dev/cli/contracts/deployment": true,
}

func sourceBoundary(name string, source any) error {
	file, err := parser.ParseFile(token.NewFileSet(), name, source, parser.ParseComments)
	if err != nil {
		return err
	}
	for _, imp := range file.Imports {
		value, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return err
		}
		if !productionImports[value] {
			return refuse("IMPORT_BOUNDARY", name, "unreviewed production import: "+value)
		}
	}
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, "go:linkname") {
				return refuse("IMPORT_BOUNDARY", name, "linkname is forbidden")
			}
		}
	}
	return nil
}
func TestOfflineImportBoundary(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".s") || strings.HasSuffix(path, ".c") || strings.HasSuffix(path, ".syso") {
			return refuse("IMPORT_BOUNDARY", path, "native escape hatch")
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		return sourceBoundary(path, nil)
	})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "list", "-deps", "-json", "./...")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var pkg struct {
			ImportPath string
			Standard   bool
		}
		err := decoder.Decode(&pkg)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !pkg.Standard && pkg.ImportPath != "github.com/codefly-dev/cli/contracts/deployment" && !strings.HasPrefix(pkg.ImportPath, "github.com/codefly-dev/cli/contracts/deployment/") {
			t.Fatalf("dependency leaves standalone module: %s", pkg.ImportPath)
		}
		if pkg.ImportPath == "net" || strings.HasPrefix(pkg.ImportPath, "net/") || pkg.ImportPath == "os/exec" || pkg.ImportPath == "plugin" {
			t.Fatalf("production dependency can perform network I/O: %s", pkg.ImportPath)
		}
	}
}
func TestBoundaryRejectsNetworkAndEscapes(t *testing.T) {
	for _, imp := range []string{"net/http", "net", "os/exec", "syscall", "unsafe", "plugin", "C", "github.com/codefly-dev/cli/cmd"} {
		if sourceBoundary("candidate.go", "package candidate\nimport _ "+strconv.Quote(imp)) == nil {
			t.Fatalf("accepted %s", imp)
		}
	}
}
