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
	if err = validateConfigurationSources(layers, destination); err != nil {
		return err
	}
	confined, err := os.OpenRoot(configurationRoot)
	if err != nil {
		return err
	}
	defer confined.Close()
	retired, err := importedProfileRetirements(confined, target)
	if err != nil {
		return err
	}
	output := filepath.Join(destination, configurations.ProfileDerivationFile)
	if dryRun {
		return nil
	}
	if err := confined.MkdirAll(target, 0o755); err != nil {
		return err
	}
	body := profileMarker + env.ConfigurationSource + "\nderives-from: " + env.ConfigurationSource + "\n"
	if err := shared.WriteFileAtomic(ctx, output, []byte(body), 0o644); err != nil {
		return err
	}
	for _, path := range retired {
		if err := confined.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func validateConfigurationSources(layers []string, destination string) error {
	for _, layer := range layers {
		if layer == destination {
			return fmt.Errorf("configuration-source must not derive from its destination")
		}
		resolved, resolveErr := filepath.EvalSymlinks(layer)
		if resolveErr != nil || resolved != layer {
			return fmt.Errorf("configuration source must be an ordinary profile directory")
		}
		entries, err := os.ReadDir(layer)
		if err != nil {
			return err
		}
		confined, err := os.OpenRoot(layer)
		if err != nil {
			return err
		}
		defer confined.Close()
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".env") && entry.Name() != configurations.ProfileDerivationFile {
				return fmt.Errorf("configuration-source supports only flat .env inputs and profile derivation metadata: %s", entry.Name())
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("configuration source %s must be a regular file", entry.Name())
			}
			if entry.Name() == "runnable-bindings.env" {
				return fmt.Errorf("configuration-source must not contain environment-specific runnable bindings")
			}
			if strings.HasSuffix(entry.Name(), ".secret.env") {
				body, err := confined.ReadFile(entry.Name())
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
						return fmt.Errorf("configuration-source secret file %s must contain empty placeholders only", entry.Name())
					}
				}
			}
		}
	}
	return nil
}

func importedProfileRetirements(confined *os.Root, target string) ([]string, error) {
	if info, err := confined.Lstat(target); err == nil && !info.IsDir() {
		return nil, fmt.Errorf("generated profile must be an ordinary directory")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	var retired []string
	previous, err := os.ReadDir(filepath.Join(confined.Name(), target))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range previous {
		if !strings.HasSuffix(entry.Name(), ".env") && entry.Name() != configurations.ProfileDerivationFile {
			continue
		}
		path := filepath.Join(target, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("generated configuration %s must be a regular file", entry.Name())
		}
		body, err := confined.ReadFile(filepath.Join(target, entry.Name()))
		if err != nil {
			return nil, err
		}
		owned := strings.HasPrefix(string(body), profileMarker)
		if !owned && entry.Name() != "runnable-bindings.env" {
			return nil, fmt.Errorf("refusing to overwrite authored configuration %s", entry.Name())
		}
		if entry.Name() != configurations.ProfileDerivationFile && owned {
			retired = append(retired, path)
		}
	}
	return retired, nil
}
