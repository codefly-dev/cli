package publish

import (
	"os"
	"path/filepath"
	"testing"
)

// mkRepo creates a fake git repo at <root>/<rel> with a manifest at
// <rel>/<manifestRel> carrying the given version.
func mkRepo(t *testing.T, root, rel, manifestRel, version string) {
	t.Helper()
	repo := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mPath := filepath.Join(repo, manifestRel)
	if err := os.MkdirAll(filepath.Dir(mPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mPath, []byte("version: "+version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverRepos(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "core", "version/info.codefly.yaml", "0.2.0")
	mkRepo(t, root, "cli", "pkg/cli/info.yaml", "0.1.0")
	mkRepo(t, root, "sdk-go", "info.codefly.yaml", "0.1.0")
	mkRepo(t, root, "agents/services/go-grpc", "agent.codefly.yaml", "0.1.4")
	mkRepo(t, root, "agents/toolboxes/nix", "agent.codefly.yaml", "0.0.7")
	// Decoys that MUST NOT be discovered:
	mkRepo(t, root, "examples/demo-proj", "info.codefly.yaml", "9.9.9") // under scanSkip "examples"
	mkRepo(t, root, "node_modules/pkg", "info.codefly.yaml", "9.9.9")   // build junk
	// A git repo with no codefly manifest — present but not a target.
	if err := os.MkdirAll(filepath.Join(root, "googleapis", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	targets, err := discoverRepos(root)
	if err != nil {
		t.Fatalf("discoverRepos: %v", err)
	}

	got := map[string]Mode{}
	for _, tg := range targets {
		got[relOrBase(root, tg.Dir)] = tg.Manifest.Mode
	}
	want := map[string]Mode{
		"core":                    ModeCoreModule,
		"cli":                     ModeCLI,
		"sdk-go":                  ModeStandaloneModule,
		"agents/services/go-grpc": ModeAgent,
		"agents/toolboxes/nix":    ModeAgent,
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %d repos %v; want %d %v", len(got), got, len(want), want)
	}
	for path, mode := range want {
		if got[path] != mode {
			t.Errorf("repo %s: mode %q, want %q", path, got[path], mode)
		}
	}
	for _, bad := range []string{"examples/demo-proj", "node_modules/pkg", "googleapis"} {
		if _, found := got[bad]; found {
			t.Errorf("decoy %s was wrongly discovered", bad)
		}
	}
}

func TestSortTargetsOrder(t *testing.T) {
	mk := func(mode Mode, dir string) *repoTarget {
		return &repoTarget{Dir: dir, Manifest: &Manifest{Mode: mode}}
	}
	ts := []*repoTarget{
		mk(ModeAgent, "agents/services/rust"),
		mk(ModeAgent, "agents/services/go"),
		mk(ModeCLI, "cli"),
		mk(ModeStandaloneModule, "sdk-go"),
		mk(ModeCoreModule, "core"),
	}
	sortTargets(ts)

	want := []string{"core", "cli", "sdk-go", "agents/services/go", "agents/services/rust"}
	for i, w := range want {
		if ts[i].Dir != w {
			t.Errorf("position %d: got %s, want %s", i, ts[i].Dir, w)
		}
	}
}
