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
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("configuration source %s must be a regular file", entry.Name())
			}
			if entry.Name() == "runnable-bindings.env" {
				return fmt.Errorf("configuration-source must not contain environment-specific runnable bindings")
			}
			if strings.HasSuffix(entry.Name(), ".secret.env") {
				body, err := os.ReadFile(filepath.Join(layer, entry.Name()))
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
	if info, err := os.Lstat(destination); err == nil && !info.IsDir() {
		return fmt.Errorf("generated profile must be an ordinary directory")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	output := filepath.Join(destination, configurations.ProfileDerivationFile)
	var retired []string
	previous, err := os.ReadDir(destination)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range previous {
		if !strings.HasSuffix(entry.Name(), ".env") && entry.Name() != configurations.ProfileDerivationFile {
			continue
		}
		path := filepath.Join(destination, entry.Name())
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("generated configuration %s must be a regular file", entry.Name())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		owned := strings.HasPrefix(string(body), profileMarker)
		if !owned && entry.Name() != "runnable-bindings.env" {
			return fmt.Errorf("refusing to overwrite authored configuration %s", entry.Name())
		}
		if path != output && owned {
			retired = append(retired, path)
		}
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
