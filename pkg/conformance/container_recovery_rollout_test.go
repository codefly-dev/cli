package conformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/sourceworkspace"
)

// rolloutDocumentPath locates the human-readable view of the inventory.
func rolloutDocumentPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), "docs", "container-recovery-rollout.md")
}

// rolloutTableRows returns every data row of the inventory table in the
// document, as its raw cells.
//
// The table is located by its header and read to the first line that is not a
// row, rather than by matching the shape a row is expected to have. Those are
// not the same test: a pattern that recognizes well-formed rows cannot see a
// malformed one, so counting what it matched proves nothing about what it
// missed. Every line inside the block must account for itself here.
func rolloutTableRows(t *testing.T) [][]string {
	t.Helper()
	lines := strings.Split(readFile(t, rolloutDocumentPath(t)), "\n")
	header := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "| Agent | Kind | Repository |") {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatal("docs/container-recovery-rollout.md has no inventory table header")
	}
	var rows [][]string
	for _, line := range lines[header+2:] {
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) != 6 {
			t.Fatalf("inventory row %q has %d cells, want 6", line, len(cells))
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		t.Fatal("docs/container-recovery-rollout.md lists no agents")
	}
	return rows
}

// documentedRollout renders a row the way the document spells it, so the
// comparison below is against the exact text a reader sees.
func documentedRollout(agent *RolloutAgent) []string {
	backends := "—"
	if len(agent.Backends) > 0 {
		backends = strings.Join(agent.Backends, ", ")
	}
	return []string{
		"`" + agent.Identity() + "`",
		agent.Kind,
		"`" + agent.Repository + "`",
		string(agent.Creates),
		backends,
		string(agent.Rebuild),
	}
}

// TestRolloutDocumentMatchesTheInventory keeps the published table and the
// machine-readable claim from drifting apart, in both directions: a row the
// document invents, drops, or contradicts fails here rather than sending a
// release off the wrong list.
func TestRolloutDocumentMatchesTheInventory(t *testing.T) {
	rows := rolloutTableRows(t)
	rollout := Rollout()
	agents := rollout.Agents
	if len(rows) != len(agents) {
		t.Fatalf("docs/container-recovery-rollout.md lists %d agents, %s lists %d",
			len(rows), ContainerRecoveryRolloutRelativePath, len(agents))
	}
	for i := range agents {
		want := documentedRollout(&agents[i])
		for cell := range want {
			if rows[i][cell] != want[cell] {
				t.Errorf("row %d cell %d = %q, inventory says %q", i+1, cell+1, rows[i][cell], want[cell])
			}
		}
	}
}

// TestRolloutOnDiskMatchesEmbeddedRollout keeps the published document and the
// compiled claim from drifting apart.
func TestRolloutOnDiskMatchesEmbeddedRollout(t *testing.T) {
	onDisk := readFile(t, filepath.Join(repositoryRoot(t), filepath.FromSlash(ContainerRecoveryRolloutRelativePath)))
	if onDisk != string(embeddedContainerRecoveryRollout) {
		t.Fatal("container_recovery.json on disk differs from the embedded copy")
	}
}

// TestRolloutCoversEveryPinnedAgent keeps the inventory from going stale in the
// one direction that matters: an agent this repository pins but never
// classified would be published into the fleet without anyone deciding whether
// its Core understands the marker.
//
// A roster plugin is a service agent by construction, so it resolves by kind. A
// matrix pin carries no kind at all, so an identity two kinds share cannot be
// resolved from one — ByIdentity reports that rather than answering about
// whichever binary it found first.
func TestRolloutCoversEveryPinnedAgent(t *testing.T) {
	rollout := Rollout()
	for _, plugin := range sourceworkspace.Roster().Plugins {
		agent := plugin.Agent()
		if _, err := rollout.ByKindAndIdentity(string(agent.Kind), plugin.Publisher+"/"+plugin.Name); err != nil {
			t.Errorf("source-workspace roster pins an agent the rollout cannot resolve: %v", err)
		}
	}
	for _, row := range Default().Rows {
		for _, agent := range row.Agents {
			if _, err := rollout.ByIdentity(agent.Publisher + "/" + agent.Name); err != nil {
				t.Errorf("conformance row %s drives an agent the rollout cannot resolve: %v", row.ID, err)
			}
		}
	}
}

// markerProjection matches a call that writes the process marker an agent
// inherits.
var markerProjection = regexp.MustCompile(`dockerrun\.SetContainerRecoveryScope\(`)

// scopeResolution matches an assembly of the ownership hash inputs — the
// recipe, as opposed to markerProjection's act of publishing its result.
var scopeResolution = regexp.MustCompile(`dockerrun\.NewContainerRecoveryScope\(`)

// projectionSites returns every non-test package that projects the marker.
func projectionSites(t *testing.T) map[string]bool {
	t.Helper()
	return sitesMatching(t, markerProjection)
}

// sitesMatching returns every non-test package containing a call the pattern
// matches.
func sitesMatching(t *testing.T, pattern *regexp.Regexp) map[string]bool {
	t.Helper()
	root := repositoryRoot(t)
	sites := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		if !pattern.MatchString(readFile(t, path)) {
			return nil
		}
		pkg, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		sites[filepath.ToSlash(pkg)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sites
}

// TestMarkerProjectionSitesMatchTheInventory holds the rollout's most load-
// bearing claim to the code. An agent resolves an empty scope wherever nothing
// projected a marker, and creates its containers with no recovery label and no
// error — so the set of commands that project one is the set of commands a
// fleet rebuild can actually fix. If a command starts or stops projecting, the
// inventory's qualification list is wrong until it is updated here.
func TestMarkerProjectionSitesMatchTheInventory(t *testing.T) {
	found := projectionSites(t)
	declared := map[string]bool{}
	for _, pkg := range Rollout().MarkerProjectedBy {
		declared[pkg] = true
		if !found[pkg] {
			t.Errorf("rollout says %s projects the container recovery marker, but no call site there does", pkg)
		}
	}
	for pkg := range found {
		if !declared[pkg] {
			t.Errorf("%s projects the container recovery marker but the rollout does not list it; agents spawned elsewhere create unlabeled containers, so the qualification list depends on this set", pkg)
		}
	}
}

// TestContainerRecoveryScopeHasOneResolver keeps the ownership recipe to a
// single implementation. The identity is a hash of home, workspace and naming
// scope, and every projecting site has to produce a byte-identical one. A label
// that is well-formed but not the hash the sweep compares is collected by
// nothing and fails silently — no error, no diagnostic, just containers piling
// up. So a second assembly of those inputs is the bug itself, not a duplication
// smell: callers outside pkg/orchestration resolve through
// orchestration.ContainerRecoveryScopeFor rather than rebuilding the triple.
func TestContainerRecoveryScopeHasOneResolver(t *testing.T) {
	const resolver = "pkg/orchestration"
	found := sitesMatching(t, scopeResolution)
	if !found[resolver] {
		t.Errorf("%s no longer resolves the container recovery scope; the single-resolver guarantee has moved or been lost", resolver)
	}
	for pkg := range found {
		if pkg != resolver {
			t.Errorf("%s assembles the container recovery scope itself; resolve through orchestration.ContainerRecoveryScopeFor instead, or the two recipes drift into hashes that match nothing", pkg)
		}
	}
}
