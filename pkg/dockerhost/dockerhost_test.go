package dockerhost

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeContext lays out a docker config directory the way the docker CLI does:
// config.json names the current context, and the context's endpoint lives under
// contexts/meta/<sha256(name)>/meta.json.
func writeContext(t *testing.T, current, name, host string) string {
	t.Helper()
	dir := t.TempDir()
	if current != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"`+current+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	meta := filepath.Join(dir, "contexts", "meta", fmt.Sprintf("%x", sha256.Sum256([]byte(name))))
	if err := os.MkdirAll(meta, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `"}}}`
	if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(payload), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func isolate(t *testing.T, configDir string) {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", configDir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
}

func TestBindCarriesTheActiveContextEndpoint(t *testing.T) {
	isolate(t, writeContext(t, "orbstack", "orbstack", "unix:///Users/example/.orbstack/run/docker.sock"))

	bound := Bind([]string{"PATH=/usr/bin", "DOCKER_HOST="})

	if !slices.Contains(bound, "DOCKER_HOST=unix:///Users/example/.orbstack/run/docker.sock") {
		t.Fatalf("Bind did not carry the active context endpoint: %v", bound)
	}
	if slices.Contains(bound, "DOCKER_HOST=") {
		t.Fatalf("Bind kept an empty DOCKER_HOST beside the resolved one: %v", bound)
	}
	if !slices.Contains(bound, "PATH=/usr/bin") {
		t.Fatalf("Bind dropped an unrelated variable: %v", bound)
	}
}

func TestBindHonorsDockerContextVariable(t *testing.T) {
	dir := writeContext(t, "", "colima", "unix:///Users/example/.colima/default/docker.sock")
	isolate(t, dir)
	t.Setenv("DOCKER_CONTEXT", "colima")

	host, name := FromContext()
	if host != "unix:///Users/example/.colima/default/docker.sock" || name != "colima" {
		t.Fatalf("FromContext = %q, %q", host, name)
	}
}

func TestBindNeverReplacesAnExplicitEndpoint(t *testing.T) {
	isolate(t, writeContext(t, "orbstack", "orbstack", "unix:///orbstack.sock"))

	environment := []string{"DOCKER_HOST=tcp://build.example.com:2376"}
	if got := Bind(environment); !slices.Equal(got, environment) {
		t.Fatalf("Bind replaced an explicit endpoint: %v", got)
	}
}

func TestBindLeavesTheDefaultContextAlone(t *testing.T) {
	isolate(t, writeContext(t, "default", "default", "unix:///var/run/docker.sock"))

	environment := []string{"PATH=/usr/bin"}
	if got := Bind(environment); !slices.Equal(got, environment) {
		t.Fatalf("Bind added an endpoint for the default context: %v", got)
	}
}

func TestBindLeavesAnUnreadableContextAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"missing"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	isolate(t, dir)

	environment := []string{"PATH=/usr/bin"}
	if got := Bind(environment); !slices.Equal(got, environment) {
		t.Fatalf("Bind invented an endpoint for an unreadable context: %v", got)
	}
}
