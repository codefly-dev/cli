// Package monorepo locates modules in the codefly development checkout.
package monorepo

import (
	"os"
	"path/filepath"

	"golang.org/x/mod/modfile"
)

const (
	CoreModulePath = "github.com/codefly-dev/core"
	CLIModulePath  = "github.com/codefly-dev/cli"
)

// FindRoot walks up from start and returns the codefly monorepo root. The
// root is identified by the module declaration in core/go.mod rather than by
// the presence of a bare directory, so unrelated trees cannot be mistaken for
// the checkout. The former top-level wool module no longer exists.
func FindRoot(start string) string {
	cur, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	if info, statErr := os.Stat(cur); statErr == nil && !info.IsDir() {
		cur = filepath.Dir(cur)
	}

	for {
		if IsModule(filepath.Join(cur, "core"), CoreModulePath) {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// IsModule reports whether dir contains a go.mod declaring modulePath.
func IsModule(dir, modulePath string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	return modfile.ModulePath(data) == modulePath
}
