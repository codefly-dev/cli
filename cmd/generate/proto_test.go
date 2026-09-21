package generate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProtoGenerationPathArgs(t *testing.T) {
	previous := protoPaths
	t.Cleanup(func() { protoPaths = previous })
	protoPaths = []string{"mind/gateway/v1/gateway.proto", "mind/v1/mind.proto"}

	if got, want := protoGenerationPathArgs("/workspace/proto", false), []string{
		"--path", "mind/gateway/v1/gateway.proto", "--path", "mind/v1/mind.proto",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("relative path args = %v, want %v", got, want)
	}
	if got, want := protoGenerationPathArgs("/workspace/proto", true), []string{
		"--path", "/workspace/proto/mind/gateway/v1/gateway.proto",
		"--path", "/workspace/proto/mind/v1/mind.proto",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("absolute path args = %v, want %v", got, want)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
