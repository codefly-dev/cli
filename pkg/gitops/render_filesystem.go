package gitops

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Kustomize must use the same boundary as projections. Every read checks all
// components from the retained root; no on-disk fallback can follow a symlink
// or a reference outside the tree. Builds cannot write to their inputs.
type renderFileSystem struct{ tree *renderRoot }

var _ filesys.FileSystem = (*renderFileSystem)(nil)

func (tree *renderRoot) fileSystem() (filesys.FileSystem, error) {
	if err := tree.inspect(tree.path); err != nil {
		return nil, err
	}
	return &renderFileSystem{tree: tree}, nil
}

func (filesystem *renderFileSystem) ReadFile(name string) ([]byte, error) {
	checked, err := filesystem.tree.checkedPath(name)
	if err != nil {
		return nil, err
	}
	return filesystem.tree.read(filepath.Dir(checked), filepath.Base(checked))
}

func (filesystem *renderFileSystem) Open(name string) (filesys.File, error) {
	checked, err := filesystem.tree.checkedPath(name)
	if err != nil {
		return nil, err
	}
	root, relative, err := filesystem.tree.access(filepath.Dir(checked), filepath.Base(checked))
	if err != nil {
		return nil, err
	}
	return root.Open(relative)
}

func (filesystem *renderFileSystem) stat(name string) (fs.FileInfo, error) {
	checked, err := filesystem.tree.checkedPath(name)
	if err != nil {
		return nil, err
	}
	// The boundary itself is also a valid directory to inspect.
	relative, err := filepath.Rel(filesystem.tree.path, checked)
	if err != nil {
		return nil, err
	}
	return filesystem.tree.stat(filesystem.tree.path, relative)
}

func (filesystem *renderFileSystem) Exists(name string) bool {
	_, err := filesystem.stat(name)
	return err == nil
}

func (filesystem *renderFileSystem) IsDir(name string) bool {
	info, err := filesystem.stat(name)
	return err == nil && info.IsDir()
}

func (filesystem *renderFileSystem) ReadDir(name string) ([]string, error) {
	root, relative, err := filesystem.tree.access(name, ".")
	if err != nil {
		return nil, err
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func (filesystem *renderFileSystem) CleanedAbs(name string) (filesys.ConfirmedDir, string, error) {
	checked, err := filesystem.tree.checkedPath(name)
	if err != nil {
		return "", "", err
	}
	info, err := filesystem.stat(checked)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return filesys.ConfirmedDir(checked), "", nil
	}
	return filesys.ConfirmedDir(filepath.Dir(checked)), filepath.Base(checked), nil
}

func (filesystem *renderFileSystem) Walk(name string, visit filepath.WalkFunc) error {
	return filesystem.tree.walk(name, func(checked string, entry fs.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return visit(checked, info, nil)
	})
}

func (filesystem *renderFileSystem) Glob(pattern string) ([]string, error) {
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(filesystem.tree.path, pattern)
	}
	var matches []string
	err := filesystem.tree.walk(filesystem.tree.path, func(name string, _ fs.DirEntry) error {
		matched, err := filepath.Match(pattern, name)
		if matched {
			matches = append(matches, name)
		}
		return err
	})
	return matches, err
}

func (*renderFileSystem) Create(name string) (filesys.File, error) { return nil, renderReadOnly(name) }
func (*renderFileSystem) Mkdir(name string) error                  { return renderReadOnly(name) }
func (*renderFileSystem) MkdirAll(name string) error               { return renderReadOnly(name) }
func (*renderFileSystem) RemoveAll(name string) error              { return renderReadOnly(name) }
func (*renderFileSystem) WriteFile(name string, _ []byte) error    { return renderReadOnly(name) }
func renderReadOnly(name string) error {
	return fmt.Errorf("cannot write render input %s while building Kustomize", name)
}

// Core owns the projection and its validation. Its filesystem-based API may
// replace files in the staged tree, while Kustomize and ordinary readers get
// the read-only adapter. Both use the same retained descriptor.
type projectionFileSystem struct {
	filesys.FileSystem
	tree *renderRoot
}

func (filesystem *projectionFileSystem) WriteFile(name string, data []byte) error {
	checked, err := filesystem.tree.checkedPath(name)
	if err != nil {
		return err
	}
	return filesystem.tree.write(filepath.Dir(checked), filepath.Base(checked), data)
}
