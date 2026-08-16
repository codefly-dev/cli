package add

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"github.com/spf13/cobra"
)

func TestModuleCommandRequiresExactlyOneName(t *testing.T) {
	for _, args := range [][]string{nil, {"one", "two"}} {
		if err := ModuleCmd.Args(ModuleCmd, args); err == nil {
			t.Fatalf("Args(%q) unexpectedly succeeded", args)
		}
	}
	if err := ModuleCmd.Args(ModuleCmd, []string{"billing"}); err != nil {
		t.Fatalf("valid module name rejected: %v", err)
	}
}

func TestAddExistingModuleReturnsError(t *testing.T) {
	dir := t.TempDir()
	workspace := &resources.Workspace{
		Name:    "test",
		Layout:  resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{{Name: "billing"}},
	}
	if err := workspace.SaveToDirUnsafe(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := addModule("billing"); err == nil {
		t.Fatal("adding an existing module returned success")
	}
}

func TestAddModuleKeepsInventoryOnlyAgentScaffoldForFirstSync(t *testing.T) {
	repository := t.TempDir()
	runAddTestGit(t, repository, "init", "--quiet")
	runAddTestGit(t, repository, "config", "user.email", "add-module@example.invalid")
	runAddTestGit(t, repository, "config", "user.name", "Add Module Test")
	codePath := "services/api/code/main.go"
	canonicalCode := "package canonical\n"
	writeAddTestFile(t, filepath.Join(repository, "module", codePath), canonicalCode)
	writeAddTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+addTestDigest(canonicalCode)+`"}}`)
	runAddTestGit(t, repository, "add", ".")
	runAddTestGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runAddTestGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.0.0")

	root := t.TempDir()
	workspace := &resources.Workspace{Name: "test", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	agent := &resources.Agent{Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "fixture", Version: "1.0.0"}
	agentPath, err := agent.Path(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nset -eu\n" +
		"mkdir -p \"$1/services/api\"\n" +
		"printf '%s' 'kind: module\nname: billing\nservices:\n  - name: api\n' > \"$1/module.codefly.yaml\"\n" +
		"printf '%s' 'name: api\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  name: fixture\n  publisher: codefly.dev\n  version: 1.0.0\nendpoints: []\n' > \"$1/services/api/service.codefly.yaml\"\n"
	writeAddTestFile(t, agentPath, script)
	if err := os.Chmod(agentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+remote+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/codefly-dev/module-fixture.git")
	previousAgent, previousDefault := moduleAgentInput, moduleWithDefault
	moduleAgentInput, moduleWithDefault = "fixture:1.0.0", true
	defer func() {
		moduleAgentInput, moduleWithDefault = previousAgent, previousDefault
	}()

	if err := addModule("billing"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := resources.FindWorkspaceUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.ExistsModule("billing") {
		t.Fatal("inventory-only scaffold was not registered")
	}
	moduleRoot := filepath.Join(root, "modules", "billing")
	if _, err := os.Stat(filepath.Join(moduleRoot, codePath)); !os.IsNotExist(err) {
		t.Fatalf("agent unexpectedly materialized base code: %v", err)
	}
	lock, err := os.ReadFile(filepath.Join(moduleRoot, "tools", "base-source.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lock), `"ref": "v1.0.0"`) {
		t.Fatalf("source lock = %s", lock)
	}
}

func TestAddModuleRollsBackScaffoldWhoseBytesDoNotMatchPinnedSource(t *testing.T) {
	repository := t.TempDir()
	runAddTestGit(t, repository, "init", "--quiet")
	runAddTestGit(t, repository, "config", "user.email", "add-module@example.invalid")
	runAddTestGit(t, repository, "config", "user.name", "Add Module Test")
	canonicalCode := "package canonical\n"
	codePath := "services/api/code/main.go"
	writeAddTestFile(t, filepath.Join(repository, "module", "module.codefly.yaml"), "kind: module\nname: billing\nservices:\n  - name: api\n")
	writeAddTestFile(t, filepath.Join(repository, "module", codePath), canonicalCode)
	writeAddTestFile(t, filepath.Join(repository, "module", "tools", "base-manifest.json"),
		`{"files":{"`+codePath+`":"`+addTestDigest(canonicalCode)+`"}}`)
	runAddTestGit(t, repository, "add", ".")
	runAddTestGit(t, repository, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "base")
	runAddTestGit(t, repository, "-c", "tag.gpgSign=false", "tag", "v1.0.0")

	root := t.TempDir()
	workspace := &resources.Workspace{Name: "test", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	home := t.TempDir()
	t.Setenv(resources.CodeflyHomeEnv, home)
	agent := &resources.Agent{Kind: resources.ModuleAgent, Publisher: "codefly.dev", Name: "fixture", Version: "1.0.0"}
	agentPath, err := agent.Path(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	localCode := "package local\n"
	script := "#!/bin/sh\nset -eu\n" +
		"mkdir -p \"$1/services/api/code\" \"$1/tools\"\n" +
		"printf '%s' 'kind: module\nname: billing\nservices:\n  - name: api\n' > \"$1/module.codefly.yaml\"\n" +
		"printf '%s' '" + localCode + "' > \"$1/" + codePath + "\"\n" +
		"printf '%s' '{\"files\":{\"" + codePath + "\":\"" + addTestDigest(localCode) + "\"}}' > \"$1/tools/base-manifest.json\"\n"
	writeAddTestFile(t, agentPath, script)
	if err := os.Chmod(agentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	remote := (&url.URL{Scheme: "file", Path: repository}).String()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+remote+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/codefly-dev/module-fixture.git")
	previousAgent, previousDefault := moduleAgentInput, moduleWithDefault
	moduleAgentInput, moduleWithDefault = "fixture:1.0.0", true
	defer func() {
		moduleAgentInput, moduleWithDefault = previousAgent, previousDefault
	}()

	err = addModule("billing")
	if err == nil || !strings.Contains(err.Error(), "pin module scaffold source") {
		t.Fatalf("addModule error = %v, want provenance failure", err)
	}
	reloaded, err := resources.FindWorkspaceUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExistsModule("billing") {
		t.Fatal("failed scaffold remained in workspace references")
	}
	if _, err := os.Stat(filepath.Join(root, "modules", "billing")); !os.IsNotExist(err) {
		t.Fatalf("failed scaffold directory remained: %v", err)
	}
}

func TestResourceCommandsReturnErrorsThroughCobra(t *testing.T) {
	for _, command := range []*cobra.Command{
		ModuleCmd,
		ServiceCmd,
		ApplicationCmd,
		ServiceDependencyCmd,
		ApplicationDependencyCmd,
		JobCmd,
		LibraryCmd,
		LibraryDependencyCmd,
	} {
		if command.RunE == nil {
			t.Errorf("%s has no RunE handler", command.Name())
		}
		if command.Run != nil {
			t.Errorf("%s still has a Run handler", command.Name())
		}
	}
}

func writeAddTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runAddTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(bytes.TrimSpace(output))
}

func addTestDigest(contents string) string {
	digest := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(digest[:])
}
