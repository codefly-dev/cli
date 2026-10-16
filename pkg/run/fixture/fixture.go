// Package fixture reads what a native run takes from the host module's
// fixture and the workspace's environment: who administers the platform for
// this run, and which module grants the envelope carries.
package fixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Administrator is the fixture's platform administrator: the one identity that
// writes the module envelope.
type Administrator struct {
	Provider string
	Token    string
}

// ReadAdministrator reads the super_admin of fixture under moduleDir/fixtures.
func ReadAdministrator(moduleDir, fixture string) (Administrator, error) {
	path := filepath.Join(moduleDir, "fixtures", fixture+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return Administrator{}, fmt.Errorf("the envelope is written by the fixture's platform administrator, and the host declares no fixture %q: %w", fixture, err)
	}
	var document struct {
		Users []struct {
			Role         string `yaml:"role"`
			Provider     string `yaml:"provider"`
			ProviderID   string `yaml:"provider_id"`
			FixtureToken string `yaml:"fixture_token"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Administrator{}, fmt.Errorf("read %s: %w", path, err)
	}
	for _, user := range document.Users {
		if user.Role != "super_admin" {
			continue
		}
		token := user.FixtureToken
		if token == "" {
			token = user.ProviderID
		}
		return Administrator{Provider: user.Provider, Token: token}, nil
	}
	return Administrator{}, fmt.Errorf("fixture %q seeds no super_admin, so nobody can write the envelope", fixture)
}

// ReadGrants reads configurations/<env>/module-envelope.json: each principal's
// grant, raw. No file is an empty envelope.
func ReadGrants(workspaceDir, environment string) (map[string]json.RawMessage, error) {
	path := filepath.Join(workspaceDir, "configurations", environment, "module-envelope.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var document struct {
		Principals map[string]json.RawMessage `json:"principals"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return document.Principals, nil
}
