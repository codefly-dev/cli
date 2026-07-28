package cliupdate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/codefly-dev/core/releaseupdate"
)

var releasedBuild = BuildInfo{
	Version:   "1.2.3",
	Commit:    "abc123",
	BuildDate: "2026-07-28T16:23:31Z",
}

func TestDetectInstallationKinds(t *testing.T) {
	direct := executableFile(t, filepath.Join(t.TempDir(), "codefly"))
	homebrew := executableFile(t, filepath.Join(t.TempDir(), "Cellar", "codefly", "1.2.3", "bin", "codefly"))
	managed := executableFile(t, filepath.Join(t.TempDir(), "codefly-managed"))
	notExecutable := filepath.Join(t.TempDir(), "codefly")
	if err := os.WriteFile(notExecutable, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "codefly")
	if err := os.Symlink(direct, symlink); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/codefly-dev/cli\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checkoutBinary := executableFile(t, filepath.Join(checkout, "dist", "codefly"))

	tests := []struct {
		name      string
		path      string
		info      BuildInfo
		override  string
		container bool
		want      InstallKind
	}{
		{name: "direct release", path: direct, info: releasedBuild, want: InstallKindDirect},
		{name: "development build", path: direct, info: BuildInfo{Version: "development"}, want: InstallKindDevelopment},
		{name: "symlink", path: symlink, info: releasedBuild, want: InstallKindDevelopment},
		{name: "homebrew", path: homebrew, info: releasedBuild, want: InstallKindHomebrew},
		{name: "checkout", path: checkoutBinary, info: releasedBuild, want: InstallKindDevelopment},
		{name: "managed override", path: managed, info: releasedBuild, override: "managed", want: InstallKindManaged},
		{name: "container", path: managed, info: releasedBuild, container: true, want: InstallKindManaged},
		{name: "not executable", path: notExecutable, info: releasedBuild, want: InstallKindUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := detectInstallation(test.path, test.info, test.override, test.container)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.want {
				t.Fatalf("kind = %q, want %q", got.Kind, test.want)
			}
		})
	}
}

func TestDetectInstallationRejectsOwnershipOverride(t *testing.T) {
	path := executableFile(t, filepath.Join(t.TempDir(), "codefly"))
	if _, err := detectInstallation(path, releasedBuild, "direct", false); err == nil {
		t.Fatal("expected direct ownership override to be rejected")
	}
}

func TestRunningInContainerRecognizesRuntimeMarkers(t *testing.T) {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		t.Run(marker, func(t *testing.T) {
			stat := func(path string) error {
				if path == marker {
					return nil
				}
				return os.ErrNotExist
			}
			if !runningInContainerWith(stat, func(string) string { return "" }) {
				t.Fatalf("container marker %q was ignored", marker)
			}
		})
	}

	if runningInContainerWith(func(string) error { return errors.New("unavailable") }, func(string) string { return "" }) {
		t.Fatal("filesystem errors were treated as container markers")
	}
}

func TestInstallationCoreKindAndAction(t *testing.T) {
	homebrew := Installation{
		Kind:         InstallKindHomebrew,
		ResolvedPath: "/opt/homebrew/Cellar/codefly/0.1.46/bin/codefly",
	}
	if got := homebrew.CoreKind(); got != releaseupdate.InstallKindHomebrew {
		t.Fatalf("CoreKind() = %q", got)
	}
	if got := homebrew.Action(); got != "Migrate the legacy formula with `brew uninstall codefly && brew install --cask codefly-dev/cli/codefly`." {
		t.Fatalf("Action() = %q", got)
	}
}

func executableFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
