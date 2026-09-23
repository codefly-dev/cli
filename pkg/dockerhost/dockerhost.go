// Package dockerhost resolves the Docker endpoint the way the docker CLI does,
// so child processes that talk to Docker without reading docker contexts reach
// the same engine the CLI does.
//
// syft is the motivating consumer: it honors DOCKER_HOST and otherwise dials
// unix:///var/run/docker.sock, never the active `docker context`. On OrbStack,
// colima and Docker Desktop the engine is reachable only through the context
// endpoint, so a syft scan of a local image fails there while every other
// Docker operation in the same run succeeds.
//
// The resolution order is core's runners/dockerrun resolveDockerHost, which is
// unexported; this is the same algorithm over the same on-disk context store.
// Once core exports its resolver, this package should delegate to it.
package dockerhost

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvironmentVariable is the variable every Docker client — syft included —
// reads for an explicit endpoint.
const EnvironmentVariable = "DOCKER_HOST"

// FromContext returns the endpoint of the active, non-default docker context,
// and the context's name. It returns empty values when DOCKER_HOST is already
// set (an explicit endpoint wins and needs no help), when the active context is
// the default one (whose endpoint is the socket every client already dials),
// or when the context cannot be read.
func FromContext() (host, contextName string) {
	if strings.TrimSpace(os.Getenv(EnvironmentVariable)) != "" {
		return "", ""
	}
	name := activeContextName()
	if name == "" || name == "default" {
		return "", ""
	}
	host, err := contextHost(name)
	if err != nil {
		return "", name
	}
	return strings.TrimSpace(host), name
}

// Bind returns environment with DOCKER_HOST set to the active docker context's
// endpoint, when environment does not already name one and a non-default
// context resolves. An environment that already carries DOCKER_HOST is returned
// unchanged: an explicit endpoint is the operator's, never the CLI's to replace.
func Bind(environment []string) []string {
	for _, entry := range environment {
		if name, value, ok := strings.Cut(entry, "="); ok && name == EnvironmentVariable && strings.TrimSpace(value) != "" {
			return environment
		}
	}
	host, _ := FromContext()
	if host == "" {
		return environment
	}
	bound := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if name, _, ok := strings.Cut(entry, "="); ok && name == EnvironmentVariable {
			continue
		}
		bound = append(bound, entry)
	}
	return append(bound, EnvironmentVariable+"="+host)
}

func activeContextName() string {
	if name := strings.TrimSpace(os.Getenv("DOCKER_CONTEXT")); name != "" {
		return name
	}
	data, err := os.ReadFile(filepath.Join(configDir(), "config.json"))
	if err != nil {
		return ""
	}
	var parsed struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.CurrentContext)
}

// contextHost reads the docker endpoint of a named context. The docker CLI keys
// the context's metadata directory by the hex SHA-256 of its name.
func contextHost(name string) (string, error) {
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	data, err := os.ReadFile(filepath.Join(configDir(), "contexts", "meta", id, "meta.json"))
	if err != nil {
		return "", err
	}
	var parsed struct {
		Endpoints struct {
			Docker struct {
				Host string `json:"Host"`
			} `json:"docker"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", err
	}
	return parsed.Endpoints.Docker.Host, nil
}

func configDir() string {
	if dir := strings.TrimSpace(os.Getenv("DOCKER_CONFIG")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker")
}
