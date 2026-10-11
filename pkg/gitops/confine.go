package gitops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/codefly-dev/core/resources/names"
)

// renderRoot is the one trusted boundary of a render. Resolve that boundary
// once; never promote a service directory (or an overlay) to a new boundary.
type renderRoot struct {
	path       string
	declared   string
	root       *os.Root
	anchor     *os.Root
	anchorPath string
	identity   fs.FileInfo
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
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	identity, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	// Keep an anchor for detecting ancestor replacement too. A handle to the
	// immediate parent would move with that parent and miss its displacement.
	anchorDirectory := filepath.VolumeName(resolved) + string(filepath.Separator)
	anchor, err := os.OpenRoot(anchorDirectory)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	anchorPath, err := filepath.Rel(anchorDirectory, resolved)
	if err != nil {
		_ = root.Close()
		_ = anchor.Close()
		return nil, err
	}
	tree := &renderRoot{path: resolved, declared: declared, root: root, identity: identity, anchor: anchor, anchorPath: anchorPath}
	if err := tree.checkIdentity(); err != nil {
		_ = tree.Close()
		return nil, err
	}
	return tree, nil
}

func (tree *renderRoot) Close() error { return errors.Join(tree.root.Close(), tree.anchor.Close()) }

// This pathname check only detects displacement so a stale render fails. It is
// not the confinement boundary: all content I/O uses the original descriptor,
// even if an ancestor is replaced between this check and the operation.
func (tree *renderRoot) checkIdentity() error {
	info, err := tree.anchor.Lstat(tree.anchorPath)
	if err != nil {
		return fmt.Errorf("render root changed: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("render root %s became a symbolic link", tree.path)
	}
	if !os.SameFile(tree.identity, info) {
		return fmt.Errorf("render root %s changed during render", tree.path)
	}
	return nil
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
	if err := tree.checkIdentity(); err != nil {
		return "", err
	}
	current := tree.path
	relative := "."
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for i, component := range parts {
		relative = filepath.Join(relative, component)
		current = filepath.Join(current, component)
		info, statErr := tree.root.Lstat(relative)
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

// inspect rejects links and special files before projections or builds use
// the retained filesystem. WalkDir does not follow links.
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
	return tree.root, relative, nil
}

func (tree *renderRoot) read(directory, name string) ([]byte, error) {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return nil, err
	}
	return root.ReadFile(relative)
}

func (tree *renderRoot) write(directory, name string, data []byte) error {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return err
	}
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
	if err := root.Rename(temporary, relative); err != nil {
		return err
	}
	return tree.checkIdentity()
}

func (tree *renderRoot) stat(directory, name string) (fs.FileInfo, error) {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return nil, err
	}
	return root.Lstat(relative)
}

func (tree *renderRoot) open(directory, name string) (*os.File, error) {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return nil, err
	}
	return root.Open(relative)
}

func (tree *renderRoot) readDir(directory string) ([]fs.DirEntry, error) {
	file, err := tree.open(directory, ".")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, err
}

func (tree *renderRoot) mkdirAll(directory string) error {
	root, relative, err := tree.access(directory, ".")
	if err != nil {
		return err
	}
	current := "."
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		if err := root.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := tree.stat(tree.path, current)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("render directory %s is not a directory", current)
		}
	}
	return tree.checkIdentity()
}

func (tree *renderRoot) remove(directory, name string) error {
	root, relative, err := tree.access(directory, name)
	if err != nil {
		return err
	}
	return root.Remove(relative)
}

func (tree *renderRoot) removeAll(directory string) error {
	root, relative, err := tree.access(directory, ".")
	if err != nil {
		return err
	}
	return root.RemoveAll(relative)
}

func (tree *renderRoot) rename(source, destination string) error {
	root, from, err := tree.access(filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return err
	}
	_, to, err := tree.access(filepath.Dir(destination), filepath.Base(destination))
	if err != nil {
		return err
	}
	if err = root.Rename(from, to); err != nil {
		return err
	}
	return tree.checkIdentity()
}

// copyFrom reads a separately trusted source through its retained handle and
// writes each entry through this render's handle. Neither pathname is reopened.
func (tree *renderRoot) copyFrom(source *renderRoot, sourcePath, destination string) error {
	return source.walk(sourcePath, func(name string, entry fs.DirEntry) error {
		relative, err := filepath.Rel(sourcePath, name)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return tree.mkdirAll(target)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed", name)
		}
		data, err := source.read(filepath.Dir(name), filepath.Base(name))
		if err != nil {
			return err
		}
		if err = tree.mkdirAll(filepath.Dir(target)); err != nil {
			return err
		}
		return tree.write(filepath.Dir(target), filepath.Base(target), data)
	})
}

func (tree *renderRoot) walkRegularFiles(directory string, visit func(string, string, fs.FileInfo) error) error {
	checked, err := tree.checkedPath(directory)
	if err != nil {
		return err
	}
	return tree.walk(checked, func(name string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s: non-regular files are not allowed in rendered output", name)
		}
		relative, err := filepath.Rel(checked, name)
		if err != nil {
			return err
		}
		return visit(name, filepath.ToSlash(relative), info)
	})
}

// These helpers also serve non-render callers whose directory is their trusted
// root. Render projections retain their original renderRoot through every call.
func readWithin(directory, name string) ([]byte, error) {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	return tree.read(tree.path, name)
}

func openWithin(directory, name string) (*os.File, error) {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	root, relative, err := tree.access(tree.path, name)
	if err != nil {
		return nil, err
	}
	return root.Open(relative)
}

func writeWithin(directory, name string, data []byte) error {
	tree, err := newRenderRoot(directory)
	if err != nil {
		return err
	}
	defer tree.Close()
	return tree.write(tree.path, name, data)
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
	if err := tree.inspect(tree.path); err != nil {
		releaseRenderRoot(tree, opts)
		return nil, err
	}
	return tree, nil
}

func releaseRenderRoot(tree *renderRoot, opts *RenderOptions) {
	if tree != opts.renderRoot {
		_ = tree.Close()
	}
}

// os.Root.Rename handles initial publication. Replacing a nonempty directory
// requires the platform's atomic exchange, with both parent directories opened
// through the retained root before passing their descriptors to the syscall.
func exchangeDirectoriesInRoot(root *os.Root, left, right string) error {
	leftParent, err := root.Open(filepath.Dir(left))
	if err != nil {
		return err
	}
	defer leftParent.Close()
	rightParent, err := root.Open(filepath.Dir(right))
	if err != nil {
		return err
	}
	defer rightParent.Close()
	return exchangeDirectoriesAt(leftParent, filepath.Base(left), rightParent, filepath.Base(right))
}
