package builder

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The daemon is the authority on how a registry is reached, because the push
// went through it. A registry it reaches over plain HTTP but that nothing else
// can tell apart from a TLS one is pushed to successfully and then unreachable.
func TestInsecureMatcherFollowsTheDaemonConfiguration(t *testing.T) {
	matcher := insecureMatcher(registryConfig{
		InsecureRegistryCIDRs: []string{"127.0.0.0/8", "10.0.0.0/8"},
		IndexConfigs: map[string]registryIndex{
			"docker.io":          {Secure: true},
			"registry.corp:5000": {Secure: false},
			"secure.corp:5000":   {Secure: true},
		},
	})

	for registry, insecure := range map[string]bool{
		"registry.corp:5000": true,  // declared insecure by hostname
		"secure.corp:5000":   false, // declared, but over TLS
		"docker.io":          false,
		"ghcr.io":            false, // never declared
		"10.4.2.1:5000":      true,  // inside a declared CIDR
		"203.0.113.5:5000":   false, // routable, outside every declared CIDR
	} {
		require.Equal(t, insecure, matcher(registry), "wrong scheme decision for %s", registry)
	}
}

// An empty configuration must not mark anything insecure: downgrading a registry
// nobody declared would send credentials over plain HTTP.
func TestInsecureMatcherDefaultsToTLS(t *testing.T) {
	matcher := insecureMatcher(registryConfig{})

	require.False(t, matcher("registry.corp:5000"))
	require.False(t, matcher("10.4.2.1:5000"))
}
