package orchestration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
)

func TestAppendEnvironmentVariablesToFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := AppendEnvironmentVariablesToFile(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

func TestAppendEnvironmentVariablesToFileRejectsSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.env")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := AppendEnvironmentVariablesToFile(context.Background(), link, nil); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestAppendRuntimeEnvironmentToFileIncludesSDKDiscoveryCapabilities(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.env")
	identity := &basev0.ServiceIdentity{
		Workspace: "warden-platform",
		Module:    "platform",
		Name:      "warden",
		Version:   "0.0.0",
	}
	providedMapping := &basev0.NetworkMapping{
		Endpoint: &basev0.Endpoint{
			Module:  "platform",
			Service: "warden",
			Name:    "rest",
			Api:     "rest",
		},
		Instances: []*basev0.NetworkInstance{{
			Address: "http://localhost:18982",
			Access:  resources.NewNativeNetworkAccess(),
		}},
	}
	dependencyMapping := &basev0.NetworkMapping{
		Endpoint: &basev0.Endpoint{
			Module:  "saas",
			Service: "accounts",
			Name:    "connect",
			Api:     "connect",
		},
		Instances: []*basev0.NetworkInstance{{
			Address: "http://localhost:10650",
			Access:  resources.NewNativeNetworkAccess(),
		}},
	}

	if err := AppendRuntimeEnvironmentToFile(
		context.Background(),
		path,
		identity,
		resources.NewRuntimeContextNative(),
		"codefly",
		nil,
		[]*basev0.NetworkMapping{providedMapping, dependencyMapping},
	); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"CODEFLY__MODULE=platform\n",
		"CODEFLY__SERVICE=warden\n",
		"CODEFLY__FIXTURE=codefly\n",
		"CODEFLY__ENDPOINT__PLATFORM__WARDEN__REST__REST=http://localhost:18982\n",
		"CODEFLY__ENDPOINT__SAAS__ACCOUNTS__CONNECT__CONNECT=http://localhost:10650\n",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("runtime environment is missing %q:\n%s", expected, text)
		}
	}
}

func TestAppendServiceProcessConfigurationsToFileIncludesSecretsAndFiltersDependencies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.env")
	native := resources.NewRuntimeContextNative()
	configuration := func(origin, name, key, value string, secret bool, runtimeContext *basev0.RuntimeContext) *basev0.Configuration {
		return &basev0.Configuration{
			Origin: origin, RuntimeContext: runtimeContext,
			Infos: []*basev0.ConfigurationInformation{{
				Name: name,
				ConfigurationValues: []*basev0.ConfigurationValue{{
					Key: key, Value: value, Secret: secret,
				}},
			}},
		}
	}
	service := configuration("mind/mind", "llm", "openai_api_key", "provider-token", true, native)
	workspace := configuration(resources.ConfigurationWorkspace, "dogfood", "campaign", "headless", false, native)
	postgres := configuration("infra/postgres", "postgres", "read-write-connection", "postgres://runtime", true, native)
	containerOnly := configuration(
		"infra/redis", "redis", "connection", "redis://container", true,
		&basev0.RuntimeContext{Kind: resources.RuntimeContextContainer},
	)
	runtime := configuration("mind/mind", "runtime", "session_id", "session-123", false, native)

	if err := AppendServiceProcessConfigurationsToFile(
		context.Background(), path, native, service,
		[]*basev0.Configuration{workspace},
		[]*basev0.Configuration{postgres, containerOnly},
		[]*basev0.Configuration{runtime},
	); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__MIND__MIND__LLM__OPENAI_API_KEY=provider-token\n",
		"CODEFLY__WORKSPACE_CONFIGURATION__DOGFOOD__CAMPAIGN=headless\n",
		"CODEFLY__SERVICE_SECRET_CONFIGURATION__INFRA__POSTGRES__POSTGRES__READ_WRITE_CONNECTION=postgres://runtime\n",
		"CODEFLY__SERVICE_CONFIGURATION__MIND__MIND__RUNTIME__SESSION_ID=session-123\n",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("service process environment is missing %q; keys=%v", expected, environmentKeys(text))
		}
	}
	if strings.Contains(text, "REDIS") || strings.Contains(text, "redis://container") {
		t.Fatalf("container-only dependency leaked into native environment; keys=%v", environmentKeys(text))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
}

// TestServiceProcessOutputEnvWritesDependencyConfigsOncePerRun locks the
// Init/Start source partitioning the runner relies on. The runner writes its
// own, workspace, and runtime configurations at Init (final then) and writes
// dependency configurations once at Start (after the dependency barrier). A
// dependency configuration must appear exactly once in the owner-only file:
// including it at Init too — as the runner originally did — duplicated every
// dependency secret. This mirrors the two writer calls the runner makes.
func TestServiceProcessOutputEnvWritesDependencyConfigsOncePerRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.env")
	native := resources.NewRuntimeContextNative()
	configuration := func(origin, name, key, value string) *basev0.Configuration {
		return &basev0.Configuration{
			Origin: origin, RuntimeContext: native,
			Infos: []*basev0.ConfigurationInformation{{
				Name: name,
				ConfigurationValues: []*basev0.ConfigurationValue{{
					Key: key, Value: value, Secret: true,
				}},
			}},
		}
	}
	own := configuration("mind/mind", "llm", "openai_api_key", "provider-token")
	runtime := configuration("mind/mind", "runtime", "session_id", "session-123")
	dependency := configuration("infra/postgres", "postgres", "read-write-connection", "postgres://runtime")

	// Init phase: own + runtime only, NO dependency configurations.
	if err := AppendServiceProcessConfigurationsToFile(
		context.Background(), path, native, own, nil, nil, []*basev0.Configuration{runtime},
	); err != nil {
		t.Fatal(err)
	}
	// Start phase: refreshed dependency configurations only.
	if err := AppendServiceProcessConfigurationsToFile(
		context.Background(), path, native, nil, nil, []*basev0.Configuration{dependency}, nil,
	); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const dependencyKey = "CODEFLY__SERVICE_SECRET_CONFIGURATION__INFRA__POSTGRES__POSTGRES__READ_WRITE_CONNECTION="
	if got := strings.Count(string(body), dependencyKey); got != 1 {
		t.Fatalf("dependency connection written %d times, want exactly once; keys=%v", got, environmentKeys(string(body)))
	}
}

func environmentKeys(body string) []string {
	lines := strings.Split(strings.TrimSpace(body), "\n")
	keys := make([]string, 0, len(lines))
	for _, line := range lines {
		key, _, _ := strings.Cut(line, "=")
		keys = append(keys, key)
	}
	return keys
}
