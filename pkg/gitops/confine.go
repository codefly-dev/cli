package gitops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/codefly-dev/core/resources/names"
)

// renderRoot is the one trusted boundary of a render. Resolve that boundary
// once; never promote a service directory (or an overlay) to a new boundary.
type renderRoot struct {
	path     string
	declared string
}

func newRenderRoot(directory string) (*renderRoot, error) {
	declared, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(declared)
	if err != nil {
		return nil, err
	}
	return &renderRoot{path: resolved, declared: declared}, nil
}

// checkedPath Lstats every component below the root, including the final file.
// Missing components are safe for creation; existing links are never followed.
func (tree *renderRoot) checkedPath(name string) (string, error) {
	if tree == nil {
		return "", fmt.Errorf("render root is required")
	}
	if filepath.IsAbs(name) {
		base := tree.path
		if name == tree.declared || strings.HasPrefix(name, tree.declared+string(filepath.Separator)) {
			base = tree.declared
		}
		var err error
		name, err = filepath.Rel(base, name)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("render path %q escapes root %s", name, tree.path)
	}
	// The already-resolved boundary itself must not have been replaced by a
	// link. Use rooted Lstat for this check as well as all descendant checks.
	parent, err := os.OpenRoot(filepath.Dir(tree.path))
	if err != nil {
		return "", err
	}
	info, statErr := parent.Lstat(filepath.Base(tree.path))
	closeErr := parent.Close()
	if err = errors.Join(statErr, closeErr); err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("render root %s became a symbolic link", tree.path)
	}
	root, err := os.OpenRoot(tree.path)
	if err != nil {
		return "", err
	}
	defer root.Close()
	current := tree.path
	relative := "."
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for i, component := range parts {
		relative = filepath.Join(relative, component)
		current = filepath.Join(current, component)
		info, statErr := root.Lstat(relative)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("render path %q traverses symbolic link %s", name, current)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("render path %q traverses non-directory %s", name, current)
		}
	}
	return current, nil
}

// inspect checks a subtree before handing it to a library that reads or writes
// paths itself (Core projections and Kustomize). WalkDir does not follow links.
func (tree *renderRoot) inspect(directory string) error {
	return tree.walk(directory, func(name string, entry fs.DirEntry) error {
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed in rendered output", name)
		}
		return nil
	})
}

func (tree *renderRoot) walk(directory string, visit func(string, fs.DirEntry) error) error {
	root, relative, err := tree.access(directory, ".")
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), filepath.ToSlash(relative), func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		checked, err := tree.checkedPath(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		return visit(checked, entry)
	})
}

func (tree *renderRoot) access(directory, name string) (*os.Root, string, error) {
	directory, err := tree.checkedPath(directory)
	if err != nil {
		return nil, "", err
	}
	if !filepath.IsLocal(name) {
		return nil, "", fmt.Errorf("path %q escapes directory %s", name, directory)
	}
	path, err := tree.checkedPath(filepath.Join(directory, name))
	if err != nil {
		return nil, "", err
	}
	relative, err := filepath.Rel(tree.path, path)
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(tree.path)
	return root, relative, err
}

func (tree *renderRoot) read(directory, name string) ([]byte, error) {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(relative)
}

func (tree *renderRoot) write(directory, name string, data []byte) error {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return err
	}
	defer root.Close()
	// Replace the directory entry, never truncate an inode supplied by an
	// agent. A hard link into another tree must not change that tree's bytes.
	temporary := filepath.Join(filepath.Dir(relative), ".codefly-write-"+uuid.NewString())
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(temporary) }()
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	if _, err = tree.checkedPath(filepath.Join(directory, name)); err != nil {
		return err
	}
	return root.Rename(temporary, relative)
}

func (tree *renderRoot) stat(directory, name string) (fs.FileInfo, error) {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Lstat(relative)
}

// These helpers also serve non-render callers whose directory is their trusted
// root. Render projections retain their original renderRoot through every call.
func readWithin(directory, name string) ([]byte, error) {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	return tree.read(tree.path, name)
}

func openWithin(directory, name string) (*os.File, error) {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	root, relative, err := tree.access(tree.path, name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(relative)
}

func writeWithin(directory, name string, data []byte) error {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return err
	}
	return tree.write(tree.path, name, data)
}

func removeWithin(directory, name string) error {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return err
	}
	root, relative, err := tree.access(tree.path, name)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(relative)
}

func statWithin(directory, name string) (fs.FileInfo, error) {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	return tree.stat(tree.path, name)
}

// pathSegment admits value as one path element — an environment or a unit
// name a tree is laid out by — and refuses anything that would step out of
// the directory it is joined to.
func pathSegment(what, value string) (string, error) {
	segment := filepath.Base(value)
	if value == "" || segment != value || segment == "." || segment == ".." {
		return "", fmt.Errorf("%s %q is not one path element", what, value)
	}
	return segment, nil
}

// Validate composition names before any projection writes files derived from
// them. The module/service grammar is Core's; environments use the same safe
// path-component rule as GitOps publication. The retained renderRoot checks every
// component, including ancestors of the service directory.
func serviceRenderOverlay(tree *renderRoot, root, module, service, environment string) (string, error) {
	for _, component := range []struct{ kind, value string }{{"module", module}, {"service", service}} {
		if err := validateRenderComponent(component.kind, component.value); err != nil {
			return "", err
		}
		if !names.IsModule(component.value) {
			return "", fmt.Errorf("%s %q must contain lowercase alphanumerics and single dashes", component.kind, component.value)
		}
	}
	return renderOverlay(tree, root, environment)
}

func validateRenderComponent(kind, value string) error {
	if strings.ContainsAny(value, "/\\") || strings.Contains(value, "..") {
		return fmt.Errorf("%s %q is not a safe path component", kind, value)
	}
	return validatePathComponent(kind, value)
}

// Shared by service projection and the token/trust-anchor patch writer.
func renderOverlay(tree *renderRoot, root, environment string) (string, error) {
	if err := validateRenderComponent("environment", environment); err != nil {
		return "", err
	}
	return tree.checkedPath(filepath.Join(root, "overlays", environment))
}

func inspectRenderRoot(root string, opts *RenderOptions) (*renderRoot, error) {
	tree := opts.renderRoot
	if tree == nil {
		var err error
		tree, err = newRenderRoot(root)
		if err != nil {
			return nil, err
		}
	}
	return tree, tree.inspect(tree.path)
}
