package environment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/shared"
)

const profileMarker = "# GENERATED — do not edit.\n# codefly environment import: "

// materializeConfigurationProfile uses Core's profile derivation: shared inputs
// stay authored once, while each environment owns its generated runnable bindings.
// Validate the complete inherited source before writing its generated declaration.
func materializeConfigurationProfile(ctx context.Context, directory string, env *environments.Environment, dryRun bool) error {
	if env.ConfigurationSource == "" {
		return nil
	}
	target, err := env.ConfigurationProfileName()
	if err != nil {
		return err
	}
	label := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	if !label.MatchString(env.ConfigurationSource) || !label.MatchString(target) || target == env.ConfigurationSource {
		return fmt.Errorf("configuration-source and destination must be distinct profile names")
	}
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	configurationRoot := filepath.Join(root, "configurations")
	resolved, err := filepath.EvalSymlinks(configurationRoot)
	if err != nil || resolved != configurationRoot {
		return fmt.Errorf("configuration profiles require an ordinary configurations directory")
	}
	destination := filepath.Join(configurationRoot, target)
	layers, exists, err := configurations.ProfileLayers(ctx, root, "configurations", []string{env.ConfigurationSource})
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("configuration-source profile does not exist")
	}
	for _, layer := range layers {
		if layerErr := validateConfigurationSourceLayer(layer, destination); layerErr != nil {
			return layerErr
		}
	}
	output := filepath.Join(destination, configurations.ProfileDerivationFile)
	retired, err := retiredGeneratedConfigurations(destination, output)
	if err != nil {
		return err
	}
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	body := profileMarker + env.ConfigurationSource + "\nderives-from: " + env.ConfigurationSource + "\n"
	if err := shared.WriteFileAtomic(ctx, output, []byte(body), 0o644); err != nil {
		return err
	}
	for _, path := range retired {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// validateConfigurationSourceLayer refuses a source layer that is not an ordinary
// directory of flat .env inputs: a symlink, a derivation from the destination, an
// environment-specific runnable binding, or a secret file holding a value rather
// than an empty placeholder.
func validateConfigurationSourceLayer(layer, destination string) error {
	if layer == destination {
		return fmt.Errorf("configuration-source must not derive from its destination")
	}
	resolved, err := filepath.EvalSymlinks(layer)
	if err != nil || resolved != layer {
		return fmt.Errorf("configuration source must be an ordinary profile directory")
	}
	entries, err := os.ReadDir(layer)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".env") && entry.Name() != configurations.ProfileDerivationFile {
			return fmt.Errorf("configuration-source supports only flat .env inputs and profile derivation metadata: %s", entry.Name())
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("configuration source %s must be a regular file", entry.Name())
		}
		if entry.Name() == "runnable-bindings.env" {
			return fmt.Errorf("configuration-source must not contain environment-specific runnable bindings")
		}
		if strings.HasSuffix(entry.Name(), ".secret.env") {
			if err := validateSecretPlaceholders(filepath.Join(layer, entry.Name()), entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateSecretPlaceholders refuses a source secret file whose keys carry a value:
// a configuration source declares which secrets exist, never what they hold.
func validateSecretPlaceholders(path, name string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		_, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(value) != "" {
			return fmt.Errorf("configuration-source secret file %s must contain empty placeholders only", name)
		}
	}
	return nil
}

// retiredGeneratedConfigurations lists the generated files an earlier import left in
// the destination, which a re-import removes; it refuses to overwrite an authored one
// and a destination that is not an ordinary directory.
func retiredGeneratedConfigurations(destination, output string) ([]string, error) {
	if info, err := os.Lstat(destination); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("generated profile must be an ordinary directory")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	previous, err := os.ReadDir(destination)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var retired []string
	for _, entry := range previous {
		if !strings.HasSuffix(entry.Name(), ".env") && entry.Name() != configurations.ProfileDerivationFile {
			continue
		}
		path := filepath.Join(destination, entry.Name())
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("generated configuration %s must be a regular file", entry.Name())
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		owned := strings.HasPrefix(string(body), profileMarker)
		if !owned && entry.Name() != "runnable-bindings.env" {
			return nil, fmt.Errorf("refusing to overwrite authored configuration %s", entry.Name())
		}
		if path != output && owned {
			retired = append(retired, path)
		}
	}
	return retired, nil
}
