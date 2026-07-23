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
		Module:    "saas",
		Name:      "frontend",
		Version:   "0.0.0",
	}
	mapping := &basev0.NetworkMapping{
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

	if err := AppendRuntimeEnvironmentToFile(
		context.Background(),
		path,
		identity,
		resources.NewRuntimeContextNative(),
		"codefly",
		nil,
		[]*basev0.NetworkMapping{mapping},
	); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"CODEFLY__MODULE=saas\n",
		"CODEFLY__SERVICE=frontend\n",
		"CODEFLY__FIXTURE=codefly\n",
		"CODEFLY__ENDPOINT__PLATFORM__WARDEN__REST__REST=http://localhost:18982\n",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("runtime environment is missing %q:\n%s", expected, text)
		}
	}
}
