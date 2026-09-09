package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/semver"
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

// TestMatrixCoreMatchesGoMod stops the declared core release line from
// outliving the dependency the CLI actually builds against. Pseudo-versions
// within a line are ignored: a support claim is about the line, and every open
// pull request would otherwise redden the moment a core bump lands on main.
func TestMatrixCoreMatchesGoMod(t *testing.T) {
	gomod := readFile(t, filepath.Join(repositoryRoot(t), "go.mod"))
	pin := regexp.MustCompile(`github\.com/codefly-dev/core (v\S+)`).FindStringSubmatch(gomod)
	if pin == nil {
		t.Fatal("go.mod does not require github.com/codefly-dev/core")
	}
	line := releaseLine(pin[1])
	if Default().Core != line {
		t.Fatalf("matrix core = %s, go.mod core = %s (release line %s)", Default().Core, pin[1], line)
	}
}

// releaseLine strips a pseudo-version's prerelease and build metadata.
func releaseLine(version string) string {
	version = strings.TrimSuffix(version, semver.Build(version))
	return strings.TrimSuffix(version, semver.Prerelease(version))
}

// workflowGates is every name a row may legitimately cite for one workflow:
// its job identifiers, plus the matrix values that name a job's gates.
//
// Resolving against the parsed document rather than the file text matters:
// gate names like "coverage", "race" and "lint" also occur in comments,
// filenames and flags, so a substring test passes even after the job itself is
// deleted.
func workflowGates(t *testing.T, document any) map[string]bool {
	t.Helper()
	gates := map[string]bool{}
	root, ok := document.(map[string]any)
	if !ok {
		return gates
	}
	jobs, ok := root["jobs"].(map[string]any)
	if !ok {
		return gates
	}
	for id, job := range jobs {
		gates[id] = true
		definition, isMap := job.(map[string]any)
		if !isMap {
			continue
		}
		strategy, isMap := definition["strategy"].(map[string]any)
		if !isMap {
			continue
		}
		for _, value := range matrixScalars(strategy["matrix"]) {
			gates[value] = true
		}
	}
	return gates
}

// matrixScalars flattens every string a strategy matrix expands into.
func matrixScalars(node any) []string {
	var values []string
	switch typed := node.(type) {
	case string:
		values = append(values, typed)
	case []any:
		for _, item := range typed {
			values = append(values, matrixScalars(item)...)
		}
	case map[string]any:
		for _, item := range typed {
			values = append(values, matrixScalars(item)...)
		}
	}
	return values
}

// TestWorkflowGatesResolveJobsNotSubstrings pins the reason this resolves
// against the parsed document: every gate name below also appears in the
// workflow's text, so a substring test would call the deleted job real.
func TestWorkflowGatesResolveJobsNotSubstrings(t *testing.T) {
	var document any
	if err := yaml.Unmarshal([]byte(`
jobs:
  quality:
    # The coverage and race gates take 4-5 minutes.
    strategy:
      matrix:
        include:
          - gate: race
    steps:
      - run: go test ./... -coverprofile=cover.out
  lint:
    steps:
      - run: golangci-lint run ./...
`), &document); err != nil {
		t.Fatal(err)
	}
	gates := workflowGates(t, document)
	for _, real := range []string{"quality", "lint", "race"} {
		if !gates[real] {
			t.Errorf("%q is a job or matrix gate but did not resolve", real)
		}
	}
	// Present in a comment and a flag, absent as a job or gate.
	if gates["coverage"] {
		t.Error("a gate name occurring only in prose resolved as real")
	}
	// A single letter matches almost any file as a substring.
	if gates["e"] {
		t.Error("an arbitrary substring resolved as a real gate")
	}
}

// TestQualifiedRowsNameRealCIJobs rejects a support claim backed by a job that
// does not exist.
func TestQualifiedRowsNameRealCIJobs(t *testing.T) {
	root := repositoryRoot(t)
	for _, row := range Default().Rows {
		for _, entry := range row.CI {
			workflow, gate, found := strings.Cut(entry, "#")
			if !found || gate == "" {
				t.Errorf("row %s has CI entry %q without a job or gate", row.ID, entry)
				continue
			}
			path := filepath.Join(root, ".github", "workflows", workflow)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("row %s names workflow %s: %v", row.ID, workflow, err)
				continue
			}
			var document any
			if err := yaml.Unmarshal(body, &document); err != nil {
				t.Errorf("parse workflow %s: %v", workflow, err)
				continue
			}
			if !workflowGates(t, document)[gate] {
				t.Errorf("row %s names gate %q, which is not a job or matrix gate in %s",
					row.ID, gate, workflow)
			}
		}
	}
}

// claimKeys are the workflow keys that declare which rows a job proves: the
// environment variable the gate reads, and the matrix key feeding it. The
// matrix key is named for what it holds because "conformance" alone already
// means something else in this repo (an agent CI manifest mode).
var claimKeys = map[string]bool{RequiredEnv: true, "conformance_rows": true}

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

var gateCall = regexp.MustCompile(`(?:conformancetest\.)?\bGate\(\w+,\s*"([a-z0-9-]+)"`)

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
