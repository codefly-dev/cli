package builder

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"strings"
)

// registryIndex is one registry's entry in the daemon's index configuration.
type registryIndex struct {
	Secure bool `json:"Secure"`
}

// registryConfig is the part of `docker info` that says which registries the
// daemon reaches without TLS.
type registryConfig struct {
	InsecureRegistryCIDRs []string                 `json:"InsecureRegistryCIDRs"`
	IndexConfigs          map[string]registryIndex `json:"IndexConfigs"`
}

// InsecureRegistry reports which registries the Docker daemon reaches without
// TLS, as a predicate over a registry host.
//
// A push runs through that daemon, so anything the CLI publishes about a pushed
// image has to resolve the registry the same way the push did. A registry
// declared in the daemon's insecure-registries and addressed by hostname is
// reached over plain HTTP by the push and over HTTPS by everything that resolves
// it independently, which turns a successful push into a failure one step later.
func InsecureRegistry(ctx context.Context) (func(registry string) bool, error) {
	output, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .RegistryConfig}}").Output()
	if err != nil {
		return nil, fmt.Errorf("cannot read the docker registry configuration: %w", err)
	}
	var config registryConfig
	if err := json.Unmarshal(output, &config); err != nil {
		return nil, fmt.Errorf("cannot decode the docker registry configuration: %w", err)
	}
	return insecureMatcher(config), nil
}

// insecureMatcher answers the question over a configuration already read, so the
// matching rules are testable without a daemon.
//
// A registry named in the daemon's index configuration answers from it; one
// addressed by IP answers from the declared CIDRs, which is how a whole private
// range is marked insecure without naming each host.
func insecureMatcher(config registryConfig) func(registry string) bool {
	blocks := make([]*net.IPNet, 0, len(config.InsecureRegistryCIDRs))
	for _, cidr := range config.InsecureRegistryCIDRs {
		if _, block, err := net.ParseCIDR(cidr); err == nil {
			blocks = append(blocks, block)
		}
	}
	return func(registry string) bool {
		if index, declared := config.IndexConfigs[registry]; declared {
			return !index.Secure
		}
		host := registry
		if trimmed, _, err := net.SplitHostPort(registry); err == nil {
			host = trimmed
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil {
			return false
		}
		for _, block := range blocks {
			if block.Contains(ip) {
				return true
			}
		}
		return false
	}
}
