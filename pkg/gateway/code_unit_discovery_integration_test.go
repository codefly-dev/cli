package gateway

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	gatewayv1 "github.com/codefly-dev/core/generated/go/mind/gateway/v1"
)

// TestGatewayDiscoversCodeUnitsThroughRootedSource proves the real Gateway
// delegates structural discovery to its language-neutral source behavior. No
// service agent, runtime command, or replacement filesystem client is used.
func TestGatewayDiscoversCodeUnitsThroughRootedSource(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"src/api/go.mod",
		"src/ads/build.gradle",
		"src/cart/cart.sln",
		"src/cart/src/cart.csproj",
		"src/worker/pyproject.toml",
		"src/worker/requirements.txt",
		"vendor/ignored/go.mod",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("declaration\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	server, err := NewServer(Config{WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	response, err := server.DiscoverCodeUnits(t.Context(), &gatewayv1.DiscoverCodeUnitsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetError() != "" {
		t.Fatalf("discovery error = %q", response.GetError())
	}
	units := response.GetCodeUnits()
	if len(units) != 4 {
		t.Fatalf("code units = %d (%+v), want all four boundaries", len(units), units)
	}
	want := map[string]struct {
		language  string
		agent     string
		manifests []string
	}{
		"src/ads":    {language: "jvm", agent: "generic", manifests: []string{"src/ads/build.gradle"}},
		"src/api":    {language: "go", agent: "go", manifests: []string{"src/api/go.mod"}},
		"src/cart":   {language: "dotnet", agent: "generic", manifests: []string{"src/cart/cart.sln", "src/cart/src/cart.csproj"}},
		"src/worker": {language: "python", agent: "python", manifests: []string{"src/worker/pyproject.toml", "src/worker/requirements.txt"}},
	}
	for _, unit := range units {
		expected, ok := want[unit.GetPath()]
		if !ok {
			t.Fatalf("unexpected code unit %+v", unit)
		}
		if unit.GetPrimaryLanguage() != expected.language || unit.GetRuntimeAgent() != expected.agent ||
			!reflect.DeepEqual(unit.GetLanguages(), []string{expected.language}) ||
			!reflect.DeepEqual(unit.GetManifestPaths(), expected.manifests) {
			t.Fatalf("code unit %s = %+v, want %+v", unit.GetPath(), unit, expected)
		}
		delete(want, unit.GetPath())
	}
	if len(want) != 0 {
		t.Fatalf("missing code units: %+v", want)
	}
}
