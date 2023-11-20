package cli

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"text/template"

	"github.com/c-bata/go-prompt"
)

// ApplyTemplate takes a YAML template as []byte, populates it using data, and returns the result as a string.
func ApplyTemplate(t string, data any) (string, error) {
	tmpl, err := template.New("template").Parse(t)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("cannot execute template: %w", err)
	}

	return buf.String(), nil
}

func LoadTemplate(local embed.FS, f string, obj any) (string, error) {
	// Read the file from the embedded file system
	data, err := fs.ReadFile(local, fmt.Sprintf("templates/%s", f))
	if err != nil {
		return "", fmt.Errorf("could not read file: %v", err)
	}
	return ApplyTemplate(string(data), obj)
}

func CopyTemplateFile(local embed.FS, f string, destination string, obj any) error {
	if _, err := os.Stat(destination); err == nil {
		selected := prompt.Input(
			fmt.Sprintf("Do you want to override the file %s? ", destination),
			func(d prompt.Document) []prompt.Suggest {
				return []prompt.Suggest{}
			}, prompt.OptionInitialBufferText("Yes"))

		if !slices.Contains([]string{"Yes", "yes", "Y", "y"}, selected) {
			return nil
		}
	}
	// Read the file from the embedded file system
	data, err := fs.ReadFile(local, fmt.Sprintf("templates/%s", f))
	if err != nil {
		return fmt.Errorf("could not read file: %v", err)
	}
	out, err := ApplyTemplate(string(data), obj)
	if err != nil {
		return fmt.Errorf("cannot apply template: %v", err)
	}
	err = os.WriteFile(destination, []byte(out), 0o644)
	if err != nil {
		return fmt.Errorf("cannot write template to file: %v", err)
	}
	return nil
}
