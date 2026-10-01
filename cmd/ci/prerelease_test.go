package ci

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// runPrerelease drives the real command, so the flag wiring and the exit-code
// contract are exercised rather than the package underneath.
func runPrerelease(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := PrereleaseCmd
	out := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(out)
	t.Cleanup(func() {
		command.SetOut(nil)
		command.SetErr(nil)
		_ = command.Flags().Set("dir", "")
		_ = command.Flags().Set("format", "text")
		_ = command.Flags().Set("release", "false")
		_ = command.Flags().Set("go-modules", "false")
		_ = command.Flags().Set("include-testdata", "false")
	})
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), err
}

// TestPrereleaseIsRegisteredUnderCI keeps the verb reachable: the gate is a
// sibling of the other `codefly ci` gates because the module and composition
// repositories that need it already call that family on every pull request.
func TestPrereleaseIsRegisteredUnderCI(t *testing.T) {
	if PrereleaseCmd.Use != "prerelease" {
		t.Fatalf("command is %q", PrereleaseCmd.Use)
	}
	for _, flag := range []string{"dir", "format", "release", "go-modules", "first-party", "include-testdata"} {
		if PrereleaseCmd.Flags().Lookup(flag) == nil {
			t.Errorf("flag --%s is not bound", flag)
		}
	}
	// Help has to stand alone with no network, per docs/commands.md.
	if !strings.Contains(PrereleaseCmd.Long, "agent-overrides") || PrereleaseCmd.Example == "" {
		t.Error("help text does not explain the exception or show an example")
	}
}

// TestPrereleaseFailsOnATaggedModuleWithDevPins is the end-to-end contract CI
// relies on: a non-zero exit, and output a reader can act on without opening the
// repository.
func TestPrereleaseFailsOnATaggedModuleWithDevPins(t *testing.T) {
	out, err := runPrerelease(t, "--dir", "../../pkg/prerelease/testdata/module-with-dev-pins")
	if err == nil {
		t.Fatal("the module that shipped dev agent pins passed the gate")
	}
	if !strings.Contains(err.Error(), "must not reach the default branch") {
		t.Errorf("error does not say what happened: %v", err)
	}
	for _, fragment := range []string{
		"module/services/frontend/service.codefly.yaml:6",
		"agent.version = 0.0.159-dev.1ed5001fd8b4",
	} {
		if !strings.Contains(out, fragment) {
			t.Errorf("output does not name %q:\n%s", fragment, out)
		}
	}
}

// TestPrereleasePassesOnACleanModule, including the first-party pseudo-versions
// a clean module really carries.
func TestPrereleasePassesOnACleanModule(t *testing.T) {
	out, err := runPrerelease(t, "--dir", "../../pkg/prerelease/testdata/clean-module")
	if err != nil {
		t.Fatalf("a clean module failed the gate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "no prerelease version reaches the default branch") {
		t.Errorf("output does not say it passed:\n%s", out)
	}
}

// TestPrereleaseReleaseScopeRefusesALabelledOverride is the design decision as a
// command-level contract: the documented dev loop keeps working on the default
// branch, and --release is what guarantees a tag carries none of it.
func TestPrereleaseReleaseScopeRefusesALabelledOverride(t *testing.T) {
	const dir = "../../pkg/prerelease/testdata/composition"
	out, err := runPrerelease(t, "--dir", dir)
	if err == nil {
		t.Fatal("the unlabelled overrides passed on the default branch")
	}
	if !strings.Contains(out, "labelled dev override, permitted on the default branch") {
		t.Errorf("a labelled override was not permitted on the default branch:\n%s", out)
	}

	releaseOut, err := runPrerelease(t, "--dir", dir, "--release")
	if err == nil {
		t.Fatal("release scope permitted a dev override")
	}
	if strings.Contains(releaseOut, "permitted on the default branch") {
		t.Errorf("release scope still permitted something:\n%s", releaseOut)
	}
	if !strings.Contains(releaseOut, "must not be in a release") {
		t.Errorf("release scope did not say so:\n%s", releaseOut)
	}
}

// TestPrereleaseJSONIsTheCompleteOutput: with --format json the payload is
// everything a CI job needs, and the error only carries the exit code so the root
// does not print a second rendering on top of it.
func TestPrereleaseJSONIsTheCompleteOutput(t *testing.T) {
	out, err := runPrerelease(t, "--dir", "../../pkg/prerelease/testdata/composition", "--format", "json")
	if err == nil {
		t.Fatal("the composition passed")
	}
	marker, ok := err.(interface{ MachineReadable() bool })
	if !ok || !marker.MachineReadable() {
		t.Errorf("error is not marked machine-readable, so the root would print it on top of the JSON: %T", err)
	}
	if strings.Contains(out, "must not reach the default branch") {
		t.Errorf("JSON mode printed the text report too:\n%s", out)
	}
	var report struct {
		SchemaVersion int    `json:"schema_version"`
		Scope         string `json:"scope"`
		Status        string `json:"status"`
		FilesScanned  int    `json:"files_scanned"`
		Blocking      []struct {
			File     string   `json:"file"`
			Line     int      `json:"line"`
			Key      string   `json:"key"`
			Version  string   `json:"version"`
			Carrier  string   `json:"carrier"`
			Labelled bool     `json:"labelled"`
			Remedy   []string `json:"remedy"`
		} `json:"blocking"`
		Allowed []struct {
			Key      string `json:"key"`
			Labelled bool   `json:"labelled"`
		} `json:"allowed"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("payload is not the report: %v\n%s", err, out)
	}
	if report.SchemaVersion != 1 || report.Status != "fail" || report.Scope != "default-branch" {
		t.Errorf("header is %+v", report)
	}
	if len(report.Blocking) == 0 || len(report.Allowed) == 0 {
		t.Fatalf("a CI job reading this sees %d blocking and %d allowed", len(report.Blocking), len(report.Allowed))
	}
	first := report.Blocking[0]
	if first.File == "" || first.Line == 0 || first.Key == "" || first.Version == "" || len(first.Remedy) == 0 {
		t.Errorf("a blocking finding is missing the file, key, version or remedy: %+v", first)
	}
	for _, allowed := range report.Allowed {
		if !allowed.Labelled {
			t.Errorf("%s was permitted without a label", allowed.Key)
		}
	}
}

// TestPrereleaseRejectsAnUnknownFormat before doing any work.
func TestPrereleaseRejectsAnUnknownFormat(t *testing.T) {
	if _, err := runPrerelease(t, "--format", "yaml"); err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Errorf("got %v", err)
	}
}

// TestPrereleaseDocumentsItsJSONMode so the machine-readable surface is
// discoverable from --help rather than from the source.
func TestPrereleaseDocumentsItsJSONMode(t *testing.T) {
	if !strings.Contains(PrereleaseCmd.Example, "--format json") {
		t.Error("the json format is not shown in the examples")
	}
}
