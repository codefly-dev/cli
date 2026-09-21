package selectionguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	core "github.com/codefly-dev/core/composition"
	"gopkg.in/yaml.v3"
)

var ErrUnboundExecution = errors.New("composition execution is blocked: Core's typed selection-to-build/render binding and executor acknowledgement are not available")

const SelectionFile = "module.codefly.selection.json"

// RejectUnboundExecution is an explicit stopgap while Core #589 defines the
// execution binding contract. Legacy executors must not ignore product-owned
// selections, even when they could still render the inherited source tree.
// This is rejection, not deployment admission or a replacement compatibility model.
func RejectUnboundExecution(roots ...string) error {
	for _, root := range roots {
		if root == "" {
			return errors.New("execution input root is required")
		}
		if _, err := os.Lstat(filepath.Join(root, SelectionFile)); err == nil {
			return fmt.Errorf("%s: %w", root, ErrUnboundExecution)
		} else if !os.IsNotExist(err) {
			return err
		}
		data, err := os.ReadFile(filepath.Join(root, core.DescriptorFileName))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var descriptor struct {
			Kind         string               `yaml:"kind"`
			Modules      core.ModuleInstances `yaml:"modules"`
			Replacements []core.Replacement   `yaml:"replacements"`
		}
		if err := yaml.Unmarshal(data, &descriptor); err != nil {
			return err
		}
		if descriptor.Kind == core.DescriptorKind && (len(descriptor.Modules.Include) > 0 || len(descriptor.Replacements) > 0) {
			return fmt.Errorf("%s: %w", root, ErrUnboundExecution)
		}
	}
	return nil
}
