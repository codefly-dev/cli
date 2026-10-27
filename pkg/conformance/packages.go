package conformance

import (
	"bufio"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Packages lists, relative to the repository root, every package a job must
// compile to prove the row: its gate packages, plus every package holding a
// file whose build constraint names the row's build tag. Deriving the list is
// what stops a new gate or tagged test from being silently left out of the job
// that claims the row.
func (r *Row) Packages(root string) ([]string, error) {
	set := map[string]bool{}
	for _, gate := range r.Gates {
		set[gate] = true
	}
	if r.BuildTag != "" {
		tagged, err := TaggedPackages(root, r.BuildTag)
		if err != nil {
			return nil, err
		}
		for _, pkg := range tagged {
			set[pkg] = true
		}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("conformance row %s has no gate package and no build-tagged file: nothing would run", r.ID)
	}
	packages := make([]string, 0, len(set))
	for pkg := range set {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	return packages, nil
}

// TaggedPackages lists, relative to root, every package with a Go file whose
// //go:build constraint names tag. It skips the directories the go tool
// ignores (testdata, and names starting with "." or "_") and node_modules.
func TaggedPackages(root, tag string) ([]string, error) {
	set := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (name == "testdata" || name == "node_modules" ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		names, err := constraintNamesTag(path, tag)
		if err != nil || !names {
			return err
		}
		pkg, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		set[filepath.ToSlash(pkg)] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find packages tagged %s: %w", tag, err)
	}
	packages := make([]string, 0, len(set))
	for pkg := range set {
		packages = append(packages, pkg)
	}
	sort.Strings(packages)
	return packages, nil
}

// constraintNamesTag reports whether the file's //go:build line, which can
// only appear in the comment block before the package clause, names tag.
func constraintNamesTag(path, tag string) (bool, error) {
	file, err := os.Open(path) //nolint:gosec // G304: walking the repository's own sources
	if err != nil {
		return false, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "//") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		named := false
		expr.Eval(func(atom string) bool {
			named = named || atom == tag
			return true
		})
		return named, nil
	}
	return false, scanner.Err()
}
