package cliupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseWorkflow struct {
	Jobs map[string]releaseWorkflowJob `yaml:"jobs"`
}

type releaseWorkflowJob struct {
	Needs string                `yaml:"needs"`
	Steps []releaseWorkflowStep `yaml:"steps"`
}

type releaseWorkflowStep struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

func TestReleaseWorkflowKeepsPostPublicationWorkRerunnable(t *testing.T) {
	var workflow releaseWorkflow
	readRepositoryYAML(t, ".github/workflows/release.yaml", &workflow)

	release, found := workflow.Jobs["release"]
	if !found {
		t.Fatal("release job is missing")
	}
	if len(release.Steps) == 0 || release.Steps[len(release.Steps)-1].Name != "Publish the immutable release and Homebrew cask" {
		t.Fatal("immutable publication must be the final step of the release job")
	}
	attest, found := workflow.Jobs["attest"]
	if !found || attest.Needs != "release" {
		t.Fatal("attestation must be a rerunnable job that depends on release")
	}
	verify, found := workflow.Jobs["verify"]
	if !found || verify.Needs != "attest" {
		t.Fatal("verification must be a rerunnable job that depends on attestation")
	}
	var verifiesHomebrew bool
	for _, step := range verify.Steps {
		if strings.Contains(step.Run, "verify-homebrew-cask.sh") {
			verifiesHomebrew = true
			break
		}
	}
	if !verifiesHomebrew {
		t.Fatal("published-release verification does not apply the prerelease-aware Homebrew policy")
	}
}

func TestReleaseWorkflowRequiresMacOSPublisherCredentials(t *testing.T) {
	var workflow releaseWorkflow
	readRepositoryYAML(t, ".github/workflows/release.yaml", &workflow)

	release := workflow.Jobs["release"]
	var publish releaseWorkflowStep
	for _, step := range release.Steps {
		if step.Name == "Publish the immutable release and Homebrew cask" {
			publish = step
			break
		}
	}
	for _, name := range []string{
		"MACOS_SIGN_P12",
		"MACOS_SIGN_PASSWORD",
		"MACOS_NOTARY_ISSUER_ID",
		"MACOS_NOTARY_KEY_ID",
		"MACOS_NOTARY_KEY",
	} {
		if strings.TrimSpace(publish.Env[name]) == "" {
			t.Fatalf("publish step does not require %s", name)
		}
	}
}

func TestGoReleaserNotarizesPublishedMacOSBinaries(t *testing.T) {
	var configuration struct {
		Notarize struct {
			MacOS []struct {
				Enabled string   `yaml:"enabled"`
				IDs     []string `yaml:"ids"`
				Sign    struct {
					Certificate string `yaml:"certificate"`
					Password    string `yaml:"password"`
				} `yaml:"sign"`
				Notarize struct {
					IssuerID string `yaml:"issuer_id"`
					KeyID    string `yaml:"key_id"`
					Key      string `yaml:"key"`
				} `yaml:"notarize"`
			} `yaml:"macos"`
		} `yaml:"notarize"`
	}
	readRepositoryYAML(t, ".goreleaser.yaml", &configuration)
	if len(configuration.Notarize.MacOS) != 1 {
		t.Fatalf("macOS notarization configurations = %d, want 1", len(configuration.Notarize.MacOS))
	}
	notarization := configuration.Notarize.MacOS[0]
	if strings.TrimSpace(notarization.Enabled) != "{{ not .IsSnapshot }}" ||
		len(notarization.IDs) != 1 || notarization.IDs[0] != "codefly" ||
		!strings.Contains(notarization.Sign.Certificate, "MACOS_SIGN_P12") ||
		!strings.Contains(notarization.Sign.Password, "MACOS_SIGN_PASSWORD") ||
		!strings.Contains(notarization.Notarize.IssuerID, "MACOS_NOTARY_ISSUER_ID") ||
		!strings.Contains(notarization.Notarize.KeyID, "MACOS_NOTARY_KEY_ID") ||
		!strings.Contains(notarization.Notarize.Key, "MACOS_NOTARY_KEY") {
		t.Fatalf("macOS notarization contract is incomplete: %#v", notarization)
	}
}

func TestHomebrewVerificationSkipsPrereleaseCask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release verification uses POSIX shell")
	}
	fakeBin := t.TempDir()
	called := filepath.Join(t.TempDir(), "gh-called")
	writeExecutable(t, filepath.Join(fakeBin, "gh"), "#!/bin/sh\n: > \"$GH_CALLED\"\nexit 1\n")

	command := exec.Command("sh", repositoryPath(".github/scripts/verify-homebrew-cask.sh"))
	command.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_CALLED="+called,
		"RELEASE_PRERELEASE=true",
		"RELEASE_TAG=v1.2.3-beta.1",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify prerelease cask: %v\n%s", err, output)
	}
	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("prerelease verification queried the stable cask: %v", err)
	}
}

func TestHomebrewVerificationRequiresStableCaskTag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release verification uses POSIX shell")
	}
	fakeBin := t.TempDir()
	writeExecutable(t, filepath.Join(fakeBin, "gh"), "#!/bin/sh\nprintf '%s' \"$TEST_CASK\"\n")
	writeExecutable(t, filepath.Join(fakeBin, "base64"), "#!/bin/sh\ncat\n")

	run := func(cask string) error {
		command := exec.Command("sh", repositoryPath(".github/scripts/verify-homebrew-cask.sh"))
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"RELEASE_PRERELEASE=false",
			"RELEASE_TAG=v1.2.3",
			"TEST_CASK="+cask,
		)
		return command.Run()
	}
	if err := run("cask \"codefly\"\nurl \"https://github.com/codefly-dev/cli/releases/download/v1.2.3/codefly.tar.gz\"\n"); err != nil {
		t.Fatalf("matching stable cask was rejected: %v", err)
	}
	if err := run("cask \"codefly\"\nurl \"https://github.com/codefly-dev/cli/releases/download/v1.2.2/codefly.tar.gz\"\n"); err == nil {
		t.Fatal("stale stable cask was accepted")
	}
}

func readRepositoryYAML(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile(repositoryPath(name))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func repositoryPath(name string) string {
	return filepath.Join("..", "..", filepath.FromSlash(name))
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
