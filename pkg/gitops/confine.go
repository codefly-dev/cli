package gitops

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources/names"
)

// The render reads and inspects files by names it derives — an environment,
// a unit, a kustomization reference, an overlay entry — joined to a directory
// it owns. Each access below goes through an os.Root opened at that
// directory, so a name that resolves outside it, through a symlink or a
// traversal, is refused by the operating system rather than trusted on the
// path's own say-so.

// readWithin reads name beneath directory.
func readWithin(directory, name string) ([]byte, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.ReadFile(name)
}

// openWithin opens name beneath directory for reading; the caller closes it.
func openWithin(directory, name string) (*os.File, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(name)
}

// writeWithin writes name beneath directory as the tool's own output.
func writeWithin(directory, name string, data []byte) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.WriteFile(name, data, 0o600)
}

// removeWithin removes name beneath directory.
func removeWithin(directory, name string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(name)
}

// statWithin describes name beneath directory.
func statWithin(directory, name string) (fs.FileInfo, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Stat(name)
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
// path-component rule as GitOps publication. confinedJoin checks filepath.Rel
// against the render root and refuses symlinked directories as well.
func serviceRenderOverlay(root, module, service, environment string) (string, error) {
	const environmentComponent = "environment"
	for _, component := range []struct{ kind, value string }{{"module", module}, {"service", service}, {environmentComponent, environment}} {
		if strings.ContainsAny(component.value, "/\\") || strings.Contains(component.value, "..") {
			return "", fmt.Errorf("%s %q is not a safe path component", component.kind, component.value)
		}
		if err := validatePathComponent(component.kind, component.value); err != nil {
			return "", err
		}
		if component.kind != environmentComponent && !names.IsModule(component.value) {
			return "", fmt.Errorf("%s %q must contain lowercase alphanumerics and single dashes", component.kind, component.value)
		}
	}
	return confinedJoin(root, filepath.Join("overlays", environment))
}
