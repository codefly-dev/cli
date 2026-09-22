package agents

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func writeDependencyFixture(t *testing.T, candidateVersion, dependencyVersion string) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "workspace.codefly.yaml"), "name: fixture\nlayout: flat\nservices:\n  - name: subject\n  - name: store\n  - name: second-store\n")
	for _, service := range []struct{ name, agent, version string }{
		{"subject", "candidate", candidateVersion},
		{"store", "database", dependencyVersion},
		{"second-store", "database", dependencyVersion},
	} {
		dir := filepath.Join(root, "services", service.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "service.codefly.yaml"), "name: "+service.name+"\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  publisher: example.com\n  name: "+service.agent+"\n  version: "+service.version+"\n")
	}
	return root
}

func TestFixtureDependenciesKeepCandidateAndDeduplicateExactPins(t *testing.T) {
	manifest := agentYAML{Publisher: "example.com", Name: "candidate", Version: "1.2.3"}
	for _, version := range []string{"latest", manifest.Version} {
		root := writeDependencyFixture(t, version, "2.3.4")
		dependencies, err := fixtureDependencies(t.Context(), root, &manifest)
		if err != nil {
			t.Fatal(err)
		}
		if len(dependencies) != 1 || dependencies[0].Identifier() != "example.com/database:2.3.4" {
			t.Fatalf("dependencies = %#v, want only the exact external pin", dependencies)
		}
	}
}

func TestFixtureDependenciesRejectFloatingOrWrongCandidate(t *testing.T) {
	manifest := agentYAML{Publisher: "example.com", Name: "candidate", Version: "1.2.3"}
	for _, tc := range []struct{ candidate, dependency, want string }{
		{"latest", "latest", "exact canonical version"},
		{"latest", "v2.3.4", "exact canonical version"},
		{"0.1.0", "2.3.4", "must select latest or 1.2.3"},
	} {
		root := writeDependencyFixture(t, tc.candidate, tc.dependency)
		_, err := fixtureDependencies(t.Context(), root, &manifest)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("fixture error = %v, want %q", err, tc.want)
		}
	}
}

func TestFixtureDependencyInstallUsesNormalCommandAndPrivateHome(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, t.TempDir())
	command := fixtureDependencyCommand(t.Context(), "codefly", root, home, &resources.Agent{
		Kind: resources.ServiceAgent, Publisher: "example.com", Name: "database", Version: "2.3.4",
	})
	want := []string{"codefly", "--timestamps=false", "agent", "install", "example.com/database:2.3.4", "--kind", "codefly:service"}
	if !reflect.DeepEqual(command.Args, want) || command.Dir != root {
		t.Fatalf("install command = %v at %s", command.Args, command.Dir)
	}
	var homes []string
	for _, entry := range command.Env {
		if value, ok := strings.CutPrefix(entry, resources.CodeflyHomeEnv+"="); ok {
			homes = append(homes, value)
		}
	}
	if !reflect.DeepEqual(homes, []string{home}) {
		t.Fatalf("install homes = %v, want only %s", homes, home)
	}
}
