package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repositoryRoot walks up from the package directory to the module root.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the conformance package")
		}
		dir = parent
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// TestMatrixOnDiskMatchesEmbeddedMatrix keeps the published document and the
// compiled claim from drifting apart.
func TestMatrixOnDiskMatchesEmbeddedMatrix(t *testing.T) {
	onDisk := readFile(t, filepath.Join(repositoryRoot(t), filepath.FromSlash(MatrixRelativePath)))
	if onDisk != string(embeddedMatrix) {
		t.Fatal("matrix.json on disk differs from the embedded copy")
	}
}

// TestMatrixCoreMatchesGoMod stops the declared core pin from outliving the
// dependency the CLI actually builds against.
func TestMatrixCoreMatchesGoMod(t *testing.T) {
	gomod := readFile(t, filepath.Join(repositoryRoot(t), "go.mod"))
	pin := regexp.MustCompile(`github\.com/codefly-dev/core (v[\d.]+)`).FindStringSubmatch(gomod)
	if pin == nil {
		t.Fatal("go.mod does not require github.com/codefly-dev/core")
	}
	if Default().Core != pin[1] {
		t.Fatalf("matrix core = %s, go.mod core = %s", Default().Core, pin[1])
	}
}

// TestQualifiedRowsNameRealCIJobs rejects a support claim backed by a job that
// does not exist.
func TestQualifiedRowsNameRealCIJobs(t *testing.T) {
	root := repositoryRoot(t)
	for _, row := range Default().Rows {
		for _, entry := range row.CI {
			workflow, gate, _ := strings.Cut(entry, "#")
			path := filepath.Join(root, ".github", "workflows", workflow)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("row %s names workflow %s: %v", row.ID, workflow, err)
				continue
			}
			if !strings.Contains(readFile(t, path), gate) {
				t.Errorf("row %s names gate %q, absent from %s", row.ID, gate, workflow)
			}
		}
	}
}

// claimKeys are the workflow keys that declare which rows a job proves: the
// environment variable the gate reads, and the matrix key feeding it.
var claimKeys = map[string]bool{RequiredEnv: true, "conformance": true}

// requiredByWorkflows maps every row identifier a workflow claims to the
// workflows claiming it. Values that are GitHub expressions are indirections
// to another key, not claims of their own.
func requiredByWorkflows(t *testing.T) map[string][]string {
	t.Helper()
	root := repositoryRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	claimed := map[string][]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		var document any
		if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, ".github", "workflows", entry.Name()))), &document); err != nil {
			t.Fatalf("parse workflow %s: %v", entry.Name(), err)
		}
		for _, id := range collectClaims(document) {
			claimed[id] = append(claimed[id], entry.Name())
		}
	}
	return claimed
}

func collectClaims(node any) []string {
	var claims []string
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			scalar, isString := value.(string)
			if claimKeys[key] && isString && !strings.Contains(scalar, "${{") {
				for _, id := range strings.Split(scalar, ",") {
					if id = strings.TrimSpace(id); id != "" {
						claims = append(claims, id)
					}
				}
				continue
			}
			claims = append(claims, collectClaims(value)...)
		}
	case []any:
		for _, item := range typed {
			claims = append(claims, collectClaims(item)...)
		}
	}
	return claims
}

// TestQualifiedRowsAreRequiredInCI is the core of the gate: a row cannot claim
// to be qualified unless a CI job declares it required, which is what stops the
// row's tests from skipping themselves when a backend is missing.
func TestQualifiedRowsAreRequiredInCI(t *testing.T) {
	claimed := requiredByWorkflows(t)
	for _, row := range Default().Rows {
		switch {
		case row.Status == StatusQualified && row.Requirable():
			if len(claimed[row.ID]) == 0 {
				t.Errorf("row %s is qualified but no workflow lists it in %s", row.ID, RequiredEnv)
			}
		case len(claimed[row.ID]) > 0:
			t.Errorf("row %s is %s but %s claims it in %v", row.ID, row.Status, RequiredEnv, claimed[row.ID])
		}
	}
	for id, workflows := range claimed {
		if _, ok := Default().Row(id); !ok {
			t.Errorf("%s in %v names unknown row %q", RequiredEnv, workflows, id)
		}
	}
}

var gateCall = regexp.MustCompile(`conformance\.Gate\(\w+,\s*"([a-z0-9-]+)"\)`)

// gateCallSites maps each row identifier to the packages that gate on it.
func gateCallSites(t *testing.T) map[string]map[string]bool {
	t.Helper()
	root := repositoryRoot(t)
	sites := map[string]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		pkg, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		for _, match := range gateCall.FindAllStringSubmatch(readFile(t, path), -1) {
			if sites[match[1]] == nil {
				sites[match[1]] = map[string]bool{}
			}
			sites[match[1]][filepath.ToSlash(pkg)] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

// TestDeclaredGatesMatchCallSites keeps the matrix's claim about where a row is
// enforced true in both directions.
func TestDeclaredGatesMatchCallSites(t *testing.T) {
	sites := gateCallSites(t)
	declared := map[string]map[string]bool{}
	for _, row := range Default().Rows {
		declared[row.ID] = map[string]bool{}
		for _, gate := range row.Gates {
			declared[row.ID][gate] = true
			if !sites[row.ID][gate] {
				t.Errorf("row %s declares gate package %s, which has no conformance.Gate call for it", row.ID, gate)
			}
		}
	}
	for id, packages := range sites {
		for pkg := range packages {
			if !declared[id][pkg] {
				t.Errorf("package %s gates on row %q, which does not declare it", pkg, id)
			}
		}
	}
}

// TestSupportedMatrixDocumentListsEveryRow keeps the human-readable inventory
// honest: every row appears with the status the matrix declares, and the
// document invents none.
func TestSupportedMatrixDocumentListsEveryRow(t *testing.T) {
	document := readFile(t, filepath.Join(repositoryRoot(t), "docs", "supported-matrix.md"))
	documented := map[string]string{}
	entry := regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\| ([a-z-]+) \\|")
	for _, match := range entry.FindAllStringSubmatch(document, -1) {
		documented[match[1]] = match[2]
	}
	for _, row := range Default().Rows {
		status, listed := documented[row.ID]
		if !listed {
			t.Errorf("docs/supported-matrix.md does not list row %s", row.ID)
			continue
		}
		if status != string(row.Status) {
			t.Errorf("docs/supported-matrix.md lists row %s as %s, matrix says %s", row.ID, status, row.Status)
		}
	}
	for id := range documented {
		if _, ok := Default().Row(id); !ok {
			t.Errorf("docs/supported-matrix.md lists unknown row %s", id)
		}
	}
}
