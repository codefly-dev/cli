package cmd

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	buildinfo "runtime/debug"
	"time"
)

// configurationEvidence identifies the collection and producer, not the full
// resolver input closure. Consumers must not treat it as per-key provenance or
// match a saved report to current source using the workspace name alone.
type configurationEvidence struct {
	StartedAt        string `json:"started_at"`
	CompletedAt      string `json:"completed_at"`
	ExecutableSHA256 string `json:"executable_sha256,omitempty"`
	CoreVersion      string `json:"core_version,omitempty"`
	CoreReplaced     bool   `json:"core_replaced"`
	InputBinding     string `json:"input_binding"`
	Coverage         string `json:"coverage"`
}

func executableDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func newConfigurationEvidence() (*configurationEvidence, error) {
	e := &configurationEvidence{
		StartedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		InputBinding: "unavailable",
		Coverage:     "composed-group-key-inventory",
	}
	if info, ok := buildinfo.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/codefly-dev/core" {
				e.CoreVersion = dep.Version
				if dep.Replace != nil {
					e.CoreReplaced = true
					e.CoreVersion = dep.Replace.Version
				}
				break
			}
		}
	}
	path, err := os.Executable()
	if err != nil {
		return e, err
	}
	e.ExecutableSHA256, err = executableDigest(path)
	return e, err
}
