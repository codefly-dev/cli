package cliupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/releaseupdate"
	"golang.org/x/mod/modfile"
)

const installKindEnvironment = "CODEFLY_INSTALL_KIND"

type InstallKind string

const (
	InstallKindUnknown     InstallKind = "unknown"
	InstallKindDirect      InstallKind = "direct"
	InstallKindHomebrew    InstallKind = "homebrew"
	InstallKindDevelopment InstallKind = "development"
	InstallKindManaged     InstallKind = "managed"
)

type Installation struct {
	Kind         InstallKind
	Path         string
	ResolvedPath string
}

func DetectInstallation() (Installation, error) {
	executable, err := os.Executable()
	if err != nil {
		return Installation{}, fmt.Errorf("resolve running Codefly executable: %w", err)
	}
	return detectInstallation(executable, CurrentBuildInfo(), os.Getenv(installKindEnvironment), runningInContainer())
}

func detectInstallation(executable string, info BuildInfo, override string, container bool) (Installation, error) {
	path, err := filepath.Abs(executable)
	if err != nil {
		return Installation{}, fmt.Errorf("resolve executable path: %w", err)
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Installation{}, fmt.Errorf("resolve executable symlinks: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return Installation{}, fmt.Errorf("resolve physical executable path: %w", err)
	}
	installation := Installation{Path: path, ResolvedPath: filepath.Clean(resolved)}

	switch strings.ToLower(strings.TrimSpace(override)) {
	case "":
	case "managed", "container":
		installation.Kind = InstallKindManaged
		return installation, nil
	default:
		return Installation{}, fmt.Errorf("%s must be managed or container", installKindEnvironment)
	}
	if container {
		installation.Kind = InstallKindManaged
		return installation, nil
	}
	if isHomebrewPath(path) || isHomebrewPath(resolved) {
		installation.Kind = InstallKindHomebrew
		return installation, nil
	}

	linkInfo, err := os.Lstat(path)
	if err != nil {
		return Installation{}, fmt.Errorf("inspect executable: %w", err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		installation.Kind = InstallKindDevelopment
		return installation, nil
	}
	if isManagedPath(resolved) {
		installation.Kind = InstallKindManaged
		return installation, nil
	}
	if inCLICheckout(resolved) || !info.Released() {
		installation.Kind = InstallKindDevelopment
		return installation, nil
	}

	resolvedInfo, err := os.Lstat(resolved)
	if err != nil {
		return Installation{}, fmt.Errorf("inspect physical executable: %w", err)
	}
	if !resolvedInfo.Mode().IsRegular() || resolvedInfo.Mode().Perm()&0o111 == 0 {
		installation.Kind = InstallKindUnknown
		return installation, nil
	}
	installation.Kind = InstallKindDirect
	return installation, nil
}

func (installation Installation) CoreKind() releaseupdate.InstallKind {
	switch installation.Kind {
	case InstallKindDirect:
		return releaseupdate.InstallKindDirect
	case InstallKindHomebrew:
		return releaseupdate.InstallKindHomebrew
	case InstallKindManaged:
		return releaseupdate.InstallKindManaged
	default:
		return releaseupdate.InstallKindUnknown
	}
}

func (installation Installation) Action() string {
	switch installation.Kind {
	case InstallKindDirect:
		return "Run `codefly self update`."
	case InstallKindHomebrew:
		if strings.Contains(filepath.ToSlash(installation.ResolvedPath), "/Cellar/codefly/") {
			return "Migrate the legacy formula with `brew uninstall codefly && brew install --cask codefly-dev/cli/codefly`."
		}
		return "Run `brew upgrade --cask codefly-dev/cli/codefly`."
	case InstallKindDevelopment:
		return "Run `codefly self build` from the CLI source checkout."
	case InstallKindManaged:
		return "Rebuild and redeploy the image or managed installation that provides Codefly."
	default:
		return "Replace Codefly through the tool that installed it."
	}
}

func isHomebrewPath(path string) bool {
	slashed := filepath.ToSlash(path)
	return strings.Contains(slashed, "/Cellar/codefly/") ||
		strings.Contains(slashed, "/Caskroom/codefly/")
}

func isManagedPath(path string) bool {
	slashed := filepath.ToSlash(path)
	for _, prefix := range []string{"/nix/store/", "/snap/", "/app/", "/usr/bin/", "/bin/", "/opt/local/"} {
		if strings.HasPrefix(slashed, prefix) {
			return true
		}
	}
	return strings.Contains(slashed, ".app/Contents/MacOS/")
}

func inCLICheckout(executable string) bool {
	for directory := filepath.Dir(executable); ; directory = filepath.Dir(directory) {
		data, err := os.ReadFile(filepath.Join(directory, "go.mod"))
		if err == nil && modfile.ModulePath(data) == "github.com/codefly-dev/cli" {
			return true
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return false
		}
	}
}

func runningInContainer() bool {
	return runningInContainerWith(func(path string) error {
		_, err := os.Stat(path)
		return err
	}, os.Getenv)
}

func runningInContainerWith(stat func(string) error, getenv func(string) string) bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if stat(marker) == nil {
			return true
		}
	}
	return getenv("KUBERNETES_SERVICE_HOST") != ""
}
