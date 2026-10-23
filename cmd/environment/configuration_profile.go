package environment

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/core/shared"
)

const profileMarker = "# GENERATED — do not edit.\n# codefly environment import: "

// materializeConfigurationProfile keeps authored inputs separate from an
// environment's generated configuration, including its own runnable bindings.
// It never copies credential values, follows symlinks, or overwrites authored files.
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
	source := filepath.Join(configurationRoot, env.ConfigurationSource)
	destination := filepath.Join(configurationRoot, target)
	for _, path := range []string{source, destination} {
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) && path == destination {
			continue
		}
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("configuration profile %s must be an ordinary directory", filepath.Base(path))
		}
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	files := make(map[string][]byte)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".env") {
			continue
		}
		if entry.Name() == "runnable-bindings.env" {
			return fmt.Errorf("configuration-source must not contain environment-specific runnable bindings")
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("configuration source %s must be a regular file", entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".secret.env") {
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
		output := filepath.Join(destination, entry.Name())
		if info, err := os.Lstat(output); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("generated configuration %s must be a regular file", entry.Name())
			}
			existing, err := os.ReadFile(output)
			if err != nil || !strings.HasPrefix(string(existing), profileMarker) {
				return fmt.Errorf("refusing to overwrite authored configuration %s", entry.Name())
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		files[output] = append([]byte(profileMarker+env.ConfigurationSource+"/"+entry.Name()+"\n"), body...)
	}
	var retired []string
	previous, err := os.ReadDir(destination)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range previous {
		path := filepath.Join(destination, entry.Name())
		if _, current := files[path]; current || !strings.HasSuffix(entry.Name(), ".env") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("generated configuration %s must be a regular file", entry.Name())
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(string(body), profileMarker+env.ConfigurationSource+"/") {
			retired = append(retired, path)
		}
	}
	if dryRun {
		return nil
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	for path, body := range files {
		if err := shared.WriteFileAtomic(ctx, path, body, 0o644); err != nil {
			return err
		}
	}
	for _, path := range retired {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}
