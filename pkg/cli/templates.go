package cli

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"text/template"
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
