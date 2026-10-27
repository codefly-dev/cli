package generators

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DiffTrees returns the slash-separated, sorted set of relative paths whose
// content (or presence) differs between the two directory trees.
func DiffTrees(generated, existing string) ([]string, error) {
	generatedFiles, err := ReadTree(generated)
	if err != nil {
		return nil, err
	}
	existingFiles, err := ReadTree(existing)
	if err != nil {
		return nil, err
	}

	seen := map[string]struct{}{}
	for f := range generatedFiles {
		seen[f] = struct{}{}
	}
	for f := range existingFiles {
		seen[f] = struct{}{}
	}

	var diffs []string
	for f := range seen {
		a, okA := generatedFiles[f]
		b, okB := existingFiles[f]
		if okA != okB || !bytes.Equal(a, b) {
			diffs = append(diffs, f)
		}
	}
	sort.Strings(diffs)
	return diffs, nil
}

// ReadTree reads a directory into relative slash-separated file names and bytes.
// A missing tree is empty, allowing generation checks to report absent outputs.
func ReadTree(root string) (map[string][]byte, error) {
	files := map[string][]byte{}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	err = filepath.WalkDir(root, func(p string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(p) //nolint:gosec // p is walked from root, a generated-contracts directory this command owns
		if readErr != nil {
			return readErr
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return files, err
}
