package librarystore

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// workspaceLibraries mirrors the `libraries.publish` block of a workspace's
// workspace.codefly.yaml. It is read independently of resources.Workspace,
// which does not carry this block as a typed field yet.
type workspaceLibraries struct {
	Publish struct {
		Go struct {
			Owner string `yaml:"owner"`
		} `yaml:"go"`
		TypeScript struct {
			Registry string `yaml:"registry"`
			Scope    string `yaml:"scope"`
		} `yaml:"typescript"`
		Python struct {
			Owner string `yaml:"owner"`
		} `yaml:"python"`
	} `yaml:"publish"`
}

// LoadStoreConfig reads the `libraries.publish` block from the workspace
// configuration at workspaceDir, e.g.:
//
//	libraries:
//	  publish:
//	    go: {owner: codefly-dev}
//	    typescript: {registry: https://npm.pkg.github.com, scope: "@codefly-dev"}
//	    python: {owner: codefly-dev}
func LoadStoreConfig(workspaceDir string) (StoreConfig, error) {
	path := filepath.Join(workspaceDir, resources.WorkspaceConfigurationName)
	data, err := os.ReadFile(path)
	if err != nil {
		return StoreConfig{}, fmt.Errorf("librarystore: read %s: %w", path, err)
	}
	var doc struct {
		Libraries workspaceLibraries `yaml:"libraries"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return StoreConfig{}, fmt.Errorf("librarystore: parse %s: %w", path, err)
	}
	return StoreConfig{
		GoOwner:     doc.Libraries.Publish.Go.Owner,
		NpmRegistry: doc.Libraries.Publish.TypeScript.Registry,
		NpmScope:    doc.Libraries.Publish.TypeScript.Scope,
		PythonOwner: doc.Libraries.Publish.Python.Owner,
	}, nil
}
