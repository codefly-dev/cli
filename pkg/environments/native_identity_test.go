package environments

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const nativeEnvironment = `name: local
host:
  coordinate: example/local/dev
  component: host
  domain: example
  audience: host
  trust_domain: example.test
  delivery: host/api/rest
native-identity:
  projections:
    - service: example/api
      audiences: [host, second/audience]
  verifiers: [example/verifier]
`

func TestNativeIdentityDeclarationRoundTrip(t *testing.T) {
	var runtime resources.Environment
	require.NoError(t, yaml.Unmarshal([]byte(nativeEnvironment), &runtime))
	env, err := FromRuntime(&runtime)
	require.NoError(t, err)
	require.Equal(t, []string{"host", "second/audience"}, env.NativeIdentity.Projections[0].Audiences)
	resource, err := env.Resource()
	require.NoError(t, err)
	reloaded, err := FromRuntime(resource)
	require.NoError(t, err)
	require.Equal(t, env.NativeIdentity, reloaded.NativeIdentity)
	require.Empty(t, env.Runtime().Extensions, "native declarations never extend the Core wire")
	for _, runtime := range []string{resources.RuntimeContextNative, resources.RuntimeContextNix} {
		require.NoError(t, env.ValidateNativeExecution(runtime))
	}
	for _, runtime := range []string{resources.RuntimeContextContainer, resources.RuntimeContextFree, "deployment", ""} {
		require.ErrorContains(t, env.ValidateNativeExecution(runtime), "native-identity requires")
	}
}

func TestNativeIdentityDeclarationRefusesInvalidSettings(t *testing.T) {
	cases := map[string]string{
		"unknown block key":      strings.Replace(nativeEnvironment, "verifiers:", "verifers:", 1),
		"unknown projection key": strings.Replace(nativeEnvironment, "audiences:", "audience:", 1),
		"duplicate block key":    nativeEnvironment + "  verifiers: [example/api]\n",
		"duplicate service":      strings.Replace(nativeEnvironment, "  verifiers:", "    - service: example/api\n      audiences: [third]\n  verifiers:", 1),
		"duplicate audience":     strings.Replace(nativeEnvironment, "host, second/audience", "host, host", 1),
		"duplicate verifier":     strings.Replace(nativeEnvironment, "[example/verifier]", "[example/verifier, example/verifier]", 1),
		"bare service":           strings.Replace(nativeEnvironment, "service: example/api", "service: api", 1),
		"path traversal":         strings.Replace(nativeEnvironment, "service: example/api", "service: ../api", 1),
		"no audience":            strings.Replace(nativeEnvironment, "[host, second/audience]", "[]", 1),
		"blank audience":         strings.Replace(nativeEnvironment, "[host, second/audience]", "[' ']", 1),
		"control audience":       strings.Replace(nativeEnvironment, "[host, second/audience]", `["second\nline"]`, 1),
		"carrier conflict":       nativeEnvironment + "service-config:\n  services:\n    api:\n      values:\n        CODEFLY_NATIVE_IDENTITY_CREDENTIALS: authored\n",
		"cluster conflict":       nativeEnvironment + "cluster: {kind: k3d, context: k3d-local}\n",
		"gitops conflict":        nativeEnvironment + "gitops: {repo-url: example}\n",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var runtime resources.Environment
			err := yaml.Unmarshal([]byte(input), &runtime)
			if err == nil {
				_, err = FromRuntime(&runtime)
			}
			require.Error(t, err)
		})
	}
}

func TestNativeIdentityRefusesPodExecution(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "cluster.example")
	env := &Environment{Host: &EnvironmentHost{}, NativeIdentity: &EnvironmentNativeIdentity{Verifiers: []string{"example/api"}}}
	require.ErrorContains(t, env.ValidateNativeExecution(resources.RuntimeContextNative), "pod proves its identity")
}
