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

func TestAddComposedModuleSourceWritesOverlayNotCommittedPath(t *testing.T) {
	source := t.TempDir()
	writeAddTestFile(t, filepath.Join(source, "module.codefly.yaml"), "kind: module\nname: host\nservices:\n  - name: api\n")
	writeAddTestFile(t, filepath.Join(source, "services", "api", "service.codefly.yaml"),
		"name: api\nversion: 0.0.0\nagent:\n  kind: codefly:service\n  name: fixture\n  publisher: codefly.dev\n  version: 1.0.0\nendpoints: []\n")

	root := t.TempDir()
	workspace := &resources.Workspace{Name: "solution", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	previousSource := moduleSource
	moduleSource = source
	defer func() { moduleSource = previousSource }()

	if err := addComposedModule("host"); err != nil {
		t.Fatal(err)
	}

	// No vendored copy is materialized.
	if _, err := os.Stat(filepath.Join(root, "modules", "host")); !os.IsNotExist(err) {
		t.Fatalf("composed module was vendored: %v", err)
	}

	// The committed workspace file carries NO machine-specific path — the whole
	// point of moving the location into the overlay.
	committed, err := os.ReadFile(filepath.Join(root, resources.WorkspaceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(committed), source) {
		t.Fatalf("committed workspace leaked the machine path %q:\n%s", source, committed)
	}

	// The location lands in the gitignored overlay as an explicit path.
	overlay, err := resources.LoadLocalOverlay(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if overlay == nil || overlay.Resolve["host"] == nil || overlay.Resolve["host"].Path != source {
		t.Fatalf("overlay path = %+v, want path %s", overlay, source)
	}

	// The overlay is kept out of git so it never dirties `git status`.
	gitignore, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(gitignore), resources.LocalOverlayConfigurationName) {
		t.Fatalf(".gitignore does not ignore the overlay:\n%s", gitignore)
	}

	// The module still resolves and boots, now through the overlay.
	reloaded, err := resources.FindWorkspaceUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.ExistsModule("host") {
		t.Fatal("composed module was not registered")
	}
	mod, err := reloaded.LoadModuleFromName(context.Background(), "host")
	if err != nil {
		t.Fatalf("composed module does not resolve: %v", err)
	}
	if mod.Dir() != source {
		t.Fatalf("resolved dir = %s, want %s", mod.Dir(), source)
	}
}

func TestAddComposedModuleSourceDerivesIdentityFromGitRemote(t *testing.T) {
	source := t.TempDir()
	runAddTestGit(t, source, "init", "--quiet")
	runAddTestGit(t, source, "remote", "add", "origin", "git@github.com:obin-ai/module-host.git")
	writeAddTestFile(t, filepath.Join(source, "module.codefly.yaml"), "kind: module\nname: host\nservices:\n  - name: api\n")

	root := t.TempDir()
	workspace := &resources.Workspace{Name: "solution", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	previousSource, previousVersion := moduleSource, moduleVersion
	moduleSource, moduleVersion = source, "latest"
	defer func() { moduleSource, moduleVersion = previousSource, previousVersion }()

	if err := addComposedModule("host"); err != nil {
		t.Fatal(err)
	}

	reloaded, err := resources.FindWorkspaceUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ref *resources.ModuleReference
	for _, r := range reloaded.Modules {
		if r.Name == "host" {
			ref = r
		}
	}
	if ref == nil {
		t.Fatal("composed module was not registered")
	}
	if ref.PathOverride != nil {
		t.Fatalf("committed reference carries a path: %v", *ref.PathOverride)
	}
	if ref.Source != "obin-ai/module-host" || ref.Version != "latest" {
		t.Fatalf("committed identity = source %q version %q, want obin-ai/module-host / latest", ref.Source, ref.Version)
	}
}

func TestAddComposedModuleWorktreeWritesIdentityAndOverlay(t *testing.T) {
	root := t.TempDir()
	workspace := &resources.Workspace{Name: "solution", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	previousWorktree, previousVersion := moduleWorktree, moduleVersion
	moduleWorktree, moduleVersion = "obin-ai/module-document-store@main", "latest"
	defer func() { moduleWorktree, moduleVersion = previousWorktree, previousVersion }()

	if err := addComposedModule("documents"); err != nil {
		t.Fatal(err)
	}

	overlay, err := resources.LoadLocalOverlay(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if overlay == nil || overlay.Resolve["documents"] == nil ||
		overlay.Resolve["documents"].Worktree != "obin-ai/module-document-store@main" {
		t.Fatalf("overlay worktree = %+v, want the worktree spec", overlay)
	}

	reloaded, err := resources.FindWorkspaceUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ref *resources.ModuleReference
	for _, r := range reloaded.Modules {
		if r.Name == "documents" {
			ref = r
		}
	}
	if ref == nil || ref.Source != "obin-ai/module-document-store" || ref.Version != "latest" {
		t.Fatalf("committed identity = %+v, want obin-ai/module-document-store / latest", ref)
	}
	if ref.PathOverride != nil {
		t.Fatalf("committed reference carries a path: %v", *ref.PathOverride)
	}
}

func TestAddComposedModuleLeavesCommittedConfigUntouchedWhenAlreadyDeclared(t *testing.T) {
	root := t.TempDir()
	workspace := &resources.Workspace{
		Name:   "solution",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "documents", Source: "obin-ai/module-document-store", Version: "latest"},
		},
	}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	committedBefore, err := os.ReadFile(filepath.Join(root, resources.WorkspaceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}

	previousWorktree, previousVersion := moduleWorktree, moduleVersion
	moduleWorktree, moduleVersion = "obin-ai/module-document-store@main", "latest"
	defer func() { moduleWorktree, moduleVersion = previousWorktree, previousVersion }()

	if composeErr := addComposedModule("documents"); composeErr != nil {
		t.Fatal(composeErr)
	}

	// Choosing a local source for an already-declared module must not churn the
	// committed file — `git status` stays clean; only the overlay changes.
	committedAfter, err := os.ReadFile(filepath.Join(root, resources.WorkspaceConfigurationName))
	if err != nil {
		t.Fatal(err)
	}
	if string(committedBefore) != string(committedAfter) {
		t.Fatalf("committed workspace changed:\nbefore:\n%s\nafter:\n%s", committedBefore, committedAfter)
	}
	overlay, err := resources.LoadLocalOverlay(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if overlay == nil || overlay.Resolve["documents"] == nil {
		t.Fatal("overlay was not written")
	}
}

func TestAddComposedModuleRejectsNameMismatch(t *testing.T) {
	source := t.TempDir()
	writeAddTestFile(t, filepath.Join(source, "module.codefly.yaml"), "kind: module\nname: host\nservices: []\n")

	root := t.TempDir()
	workspace := &resources.Workspace{Name: "solution", Layout: resources.LayoutKindModules}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	previousSource := moduleSource
	moduleSource = source
	defer func() { moduleSource = previousSource }()

	if err := addComposedModule("runtime"); err == nil {
		t.Fatal("composing a module under a mismatched name returned success")
	}
	reloaded, err := resources.FindWorkspaceUp(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ExistsModule("runtime") {
		t.Fatal("mismatched reference was registered")
	}
	// The name check fires before any write, so no overlay is left behind.
	if _, err := os.Stat(filepath.Join(root, resources.LocalOverlayConfigurationName)); !os.IsNotExist(err) {
		t.Fatalf("overlay written despite name mismatch: %v", err)
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

func TestNormalizeRemoteToOwnerRepo(t *testing.T) {
	cases := map[string]string{
		"git@github.com:obin-ai/module-host.git":       "obin-ai/module-host",
		"https://github.com/obin-ai/module-host.git":   "obin-ai/module-host",
		"https://user@github.com/Obin-AI/Module-Host":  "obin-ai/module-host",
		"git@gitlab.com:group/proj.git":                "group/proj",
		"ssh://git@github.com/obin-ai/module-host.git": "obin-ai/module-host",
		// A look-alike host must not smuggle its way into the slug: the whole
		// host is stripped, leaving the real owner/repo.
		"https://github.com.example/foo/bar": "foo/bar",
	}
	for remote, want := range cases {
		if got := normalizeRemoteToOwnerRepo(remote); got != want {
			t.Errorf("normalizeRemoteToOwnerRepo(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestAddModuleRejectsVersionWithoutComposeFlag(t *testing.T) {
	versionFlag := ModuleCmd.Flags().Lookup("version")
	prevChanged, prevValue := versionFlag.Changed, moduleVersion
	defer func() {
		versionFlag.Changed, moduleVersion = prevChanged, prevValue
		ModuleCmd.SetArgs(nil)
	}()

	ModuleCmd.SetArgs([]string{"billing", "--version", "1.2.3"})
	err := ModuleCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--version only applies") {
		t.Fatalf("plain add module --version error = %v, want a --version guard error", err)
	}
}

func TestAddComposedModuleAlreadyDeclaredKeepsGitStatusClean(t *testing.T) {
	root := t.TempDir()
	runAddTestGit(t, root, "init", "--quiet")
	runAddTestGit(t, root, "config", "user.email", "add-module@example.invalid")
	runAddTestGit(t, root, "config", "user.name", "Add Module Test")

	workspace := &resources.Workspace{
		Name:   "solution",
		Layout: resources.LayoutKindModules,
		Modules: []*resources.ModuleReference{
			{Name: "documents", Source: "obin-ai/module-document-store", Version: "latest"},
		},
	}
	if err := workspace.SaveToDirUnsafe(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	// The overlay ignore rule is committed once (steady state); after that a
	// local source choice must leave `git status` completely clean.
	writeAddTestFile(t, filepath.Join(root, ".gitignore"), resources.LocalOverlayConfigurationName+"\n")
	runAddTestGit(t, root, "add", "-A")
	runAddTestGit(t, root, "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "init")
	t.Chdir(root)

	previousWorktree, previousVersion := moduleWorktree, moduleVersion
	moduleWorktree, moduleVersion = "obin-ai/module-document-store@main", "latest"
	defer func() { moduleWorktree, moduleVersion = previousWorktree, previousVersion }()

	if composeErr := addComposedModule("documents"); composeErr != nil {
		t.Fatal(composeErr)
	}

	if status := runAddTestGit(t, root, "status", "--porcelain"); status != "" {
		t.Fatalf("git status not clean after composing an already-declared module:\n%s", status)
	}
	// The overlay was still written — it is just git-ignored.
	overlay, err := resources.LoadLocalOverlay(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if overlay == nil || overlay.Resolve["documents"] == nil {
		t.Fatal("overlay was not written")
	}
}
