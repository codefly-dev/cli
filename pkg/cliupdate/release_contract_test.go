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

func TestReleaseCertificateIsTracked(t *testing.T) {
	command := exec.Command(
		"git",
		"-C",
		repositoryPath("."),
		"ls-files",
		"--error-unmatch",
		"pkg/cliupdate/release-signing-cert.pem",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("release certificate is absent from clean checkouts: %v\n%s", err, output)
	}
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

func TestReleaseWorkflowBuildsWithCGOCrossToolchain(t *testing.T) {
	var workflow releaseWorkflow
	readRepositoryYAML(t, ".github/workflows/release.yaml", &workflow)

	release := workflow.Jobs["release"]
	steps := map[string]releaseWorkflowStep{}
	for _, step := range release.Steps {
		steps[step.Name] = step
	}
	for _, name := range []string{"Qualify the clean tag", "Publish the immutable release and Homebrew cask"} {
		step := steps[name]
		if !strings.Contains(step.Run, "ghcr.io/goreleaser/goreleaser-cross:v1.26.4") ||
			!strings.Contains(step.Run, "docker run --rm") ||
			strings.TrimSpace(step.Env["SYFT_PATH"]) == "" {
			t.Fatalf("%s does not run the CGO-capable release toolchain with Syft: %#v", name, step)
		}
	}
}

func TestGoReleaserBuildsEveryPublishedTargetWithCGO(t *testing.T) {
	var configuration struct {
		Builds []struct {
			ID     string   `yaml:"id"`
			Env    []string `yaml:"env"`
			GoOS   []string `yaml:"goos"`
			GoArch []string `yaml:"goarch"`
		} `yaml:"builds"`
	}
	readRepositoryYAML(t, ".goreleaser.yaml", &configuration)

	expected := map[string]struct {
		goos string
		arch string
		cc   string
		cxx  string
	}{
		"darwin-amd64": {goos: "darwin", arch: "amd64", cc: "CC=o64-clang", cxx: "CXX=o64-clang++"},
		"darwin-arm64": {goos: "darwin", arch: "arm64", cc: "CC=oa64-clang", cxx: "CXX=oa64-clang++"},
		"linux-amd64":  {goos: "linux", arch: "amd64", cc: "CC=x86_64-linux-gnu-gcc", cxx: "CXX=x86_64-linux-gnu-g++"},
		"linux-arm64":  {goos: "linux", arch: "arm64", cc: "CC=aarch64-linux-gnu-gcc", cxx: "CXX=aarch64-linux-gnu-g++"},
	}
	if len(configuration.Builds) != len(expected) {
		t.Fatalf("release builds = %d, want %d", len(configuration.Builds), len(expected))
	}
	for _, build := range configuration.Builds {
		want, found := expected[build.ID]
		if !found {
			t.Fatalf("unexpected release build %q", build.ID)
		}
		if len(build.GoOS) != 1 || build.GoOS[0] != want.goos || len(build.GoArch) != 1 || build.GoArch[0] != want.arch {
			t.Fatalf("release build %q target = %v/%v, want %s/%s", build.ID, build.GoOS, build.GoArch, want.goos, want.arch)
		}
		environment := strings.Join(build.Env, "\n")
		for _, required := range []string{"CGO_ENABLED=1", want.cc, want.cxx} {
			if !strings.Contains(environment, required) {
				t.Fatalf("release build %q environment does not require %q: %v", build.ID, required, build.Env)
			}
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
	enabled := strings.TrimSpace(notarization.Enabled)
	for _, required := range []string{
		"not .IsSnapshot",
		"MACOS_SIGN_P12",
		"MACOS_SIGN_PASSWORD",
		"MACOS_NOTARY_ISSUER_ID",
		"MACOS_NOTARY_KEY_ID",
		"MACOS_NOTARY_KEY",
	} {
		if !strings.Contains(enabled, required) {
			t.Fatalf("macOS notarization enabled condition does not require %s: %q", required, enabled)
		}
	}
	if strings.Join(notarization.IDs, ",") != "darwin-amd64,darwin-arm64" ||
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
	if err := run("cask \"codefly\"\nversion \"1.2.3\"\nurl \"https://github.com/codefly-dev/cli/releases/download/v#{version}/codefly_#{version}.tar.gz\"\n"); err != nil {
		t.Fatalf("matching stable cask was rejected: %v", err)
	}
	if err := run("cask \"codefly\"\nversion \"1.2.2\"\nurl \"https://github.com/codefly-dev/cli/releases/download/v#{version}/codefly_#{version}.tar.gz\"\n"); err == nil {
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
