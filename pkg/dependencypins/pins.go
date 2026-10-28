// Package dependencypins selects existing Go requirements before a pin mutates locks.
package dependencypins

import (
	"fmt"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"os"
	"path/filepath"
	"strings"
)

// Plan validates selections across every module before returning each module's pins.
// Requirements may be direct or indirect; absent requirements are never introduced.
func Plan(dirs, dependencies []string) (map[string][]string, error) {
	pins := make(map[string][]string)
	selected := make(map[string]string)
	for _, dependency := range dependencies {
		path, version, found := strings.Cut(dependency, "@")
		if !found || version == "" || strings.ContainsAny(version, "@ \t\r\n") || module.CheckPath(path) != nil {
			return nil, fmt.Errorf("dependency %q must be module@version", dependency)
		}
		if _, exists := selected[path]; exists {
			return nil, fmt.Errorf("dependency %q is selected more than once", path)
		}
		selected[path] = dependency
	}
	matched := make(map[string]bool)
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			return nil, err
		}
		file, err := modfile.Parse("go.mod", data, nil)
		if err != nil {
			return nil, err
		}
		for _, requirement := range file.Require {
			if dependency, exists := selected[requirement.Mod.Path]; exists {
				pins[dir] = append(pins[dir], dependency)
				matched[requirement.Mod.Path] = true
			}
		}
	}
	for path := range selected {
		if !matched[path] {
			return nil, fmt.Errorf("dependency %q is not required by any selected module", path)
		}
	}
	return pins, nil
}
