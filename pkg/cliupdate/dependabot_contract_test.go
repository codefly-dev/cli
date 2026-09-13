package cliupdate

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The supply-chain guarantee this repository makes is that every third-party
// action runs at a commit the maintainers chose, and that Dependabot is what
// moves those choices forward. That is two invariants, and both are easy to
// break silently: a new action added without a SHA, or an action manifest in a
// directory no Dependabot entry reaches. The tests below hold each one.

type dependabotConfig struct {
	Updates []dependabotUpdate `yaml:"updates"`
}

type dependabotUpdate struct {
	Ecosystem   string                     `yaml:"package-ecosystem"`
	Directory   string                     `yaml:"directory"`
	Directories []string                   `yaml:"directories"`
	Limit       int                        `yaml:"open-pull-requests-limit"`
	Ignore      []dependabotIgnore         `yaml:"ignore"`
	Groups      map[string]dependabotGroup `yaml:"groups"`
}

type dependabotIgnore struct {
	DependencyName string `yaml:"dependency-name"`
}

type dependabotGroup struct {
	Patterns    []string `yaml:"patterns"`
	UpdateTypes []string `yaml:"update-types"`
}

// dirs is every directory the entry covers, whether it spelled one with
// `directory` or several with `directories`.
func (u dependabotUpdate) dirs() []string {
	if len(u.Directories) > 0 {
		return u.Directories
	}
	return []string{u.Directory}
}

func readDependabotConfig(t *testing.T) dependabotConfig {
	t.Helper()
	var config dependabotConfig
	readRepositoryYAML(t, ".github/dependabot.yml", &config)
	if len(config.Updates) == 0 {
		t.Fatal("dependabot config declares no updates")
	}
	return config
}

func dependabotEntry(t *testing.T, config dependabotConfig, ecosystem string) dependabotUpdate {
	t.Helper()
	for _, update := range config.Updates {
		if update.Ecosystem == ecosystem {
			return update
		}
	}
	t.Fatalf("dependabot config has no %s entry", ecosystem)
	return dependabotUpdate{}
}

// actionManifests returns every file GitHub reads action references out of:
// the workflows, and any composite action's action.yml.
func actionManifests(t *testing.T) []string {
	t.Helper()
	var manifests []string
	root := repositoryPath(".github")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		rel, err := filepath.Rel(repositoryPath("."), path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// dependabot.yml configures the updater; it is not a workflow.
		if rel == ".github/dependabot.yml" {
			return nil
		}
		manifests = append(manifests, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) == 0 {
		t.Fatal("found no action manifests under .github")
	}
	return manifests
}

var (
	usesLine  = regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)`)
	pinnedSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// A mutable tag is a standing instruction to run whatever the upstream owner
// (or whoever compromises them) points it at next. Pinning most of them is not
// the property we want — one unpinned reference is enough, and the composite
// action in .github/actions is the one that is easy to forget because it does
// not live in .github/workflows.
func TestEveryActionReferenceIsPinnedBySHA(t *testing.T) {
	for _, manifest := range actionManifests(t) {
		data, err := os.ReadFile(repositoryPath(manifest))
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range usesLine.FindAllStringSubmatch(string(data), -1) {
			ref := match[1]
			// A local action is this repository's own code at this commit.
			if strings.HasPrefix(ref, "./") {
				continue
			}
			owner, version, found := strings.Cut(ref, "@")
			if !found {
				t.Errorf("%s: `uses: %s` has no ref; pin it to a commit SHA", manifest, ref)
				continue
			}
			if !pinnedSHA.MatchString(version) {
				t.Errorf("%s: %s is pinned to %q, want a 40-character commit SHA with the version in a trailing comment", manifest, owner, version)
			}
		}
	}
}

// Pinning by SHA freezes an action until something moves the pin, and the only
// thing that moves it is Dependabot. An action manifest in a directory the
// github-actions entry does not cover is therefore pinned forever, including
// past the security release that matters: `directory: /` reaches
// .github/workflows and a root action.yml and nothing else, so a composite
// action under .github/actions needs its own directory entry.
func TestDependabotReachesEveryActionManifest(t *testing.T) {
	covered := dependabotEntry(t, readDependabotConfig(t), "github-actions").dirs()

	for _, manifest := range actionManifests(t) {
		dir := filepath.ToSlash(filepath.Dir(manifest))
		// The root entry covers .github/workflows implicitly.
		want := "/" + dir
		if dir == ".github/workflows" {
			want = "/"
		}
		if !slices.Contains(covered, want) {
			t.Errorf("%s is not reachable by Dependabot: no github-actions entry covers %q (covered: %v). Its pins would never move.", manifest, want, covered)
		}
	}
}

// #635 intentionally grouped all version updates, including majors and digest
// updates, to keep one PR per ecosystem. A semver-only update-types filter
// excludes digest updates and recreates ungrouped PRs.
func TestVersionUpdatesUseOneCatchAllGroup(t *testing.T) {
	for _, update := range readDependabotConfig(t).Updates {
		if update.Limit != 1 || len(update.Groups) != 1 {
			t.Errorf("%s: want one catch-all group and one open PR, got %d groups and limit %d", update.Ecosystem, len(update.Groups), update.Limit)
		}
		for name, group := range update.Groups {
			if !slices.Equal(group.Patterns, []string{"*"}) || len(group.UpdateTypes) != 0 {
				t.Errorf("%s: group %q must include every dependency and update type, including digests", update.Ecosystem, name)
			}
		}
	}
}

// The core pin is the CLI's half of a cross-repo release contract:
// docs/runbooks/release-the-fleet.md re-pins it as an ordered step (core tagged
// -> cli -> agents -> composed modules) and pkg/conformance/matrix.json records
// the release line beside it. Letting Dependabot bump it either reds the whole
// gomod pull request against TestMatrixCoreMatchesGoMod or, within a line, goes
// green while moving the CLI off the core the fleet was published against.
func TestGomodUpdatesLeaveTheCorePinToTheFleetRunbook(t *testing.T) {
	gomod := dependabotEntry(t, readDependabotConfig(t), "gomod")

	const core = "github.com/codefly-dev/core"
	for _, ignore := range gomod.Ignore {
		if ignore.DependencyName == core {
			return
		}
	}
	t.Fatalf("gomod updates do not ignore %s; docs/runbooks/release-the-fleet.md owns that pin, not Dependabot", core)
}

// Every declared directory must actually hold a manifest the ecosystem can
// parse. A Dependabot entry pointed at a directory with nothing to update is
// not inert documentation — it reads as coverage the repository does not have.
func TestDependabotDirectoriesHaveAManifest(t *testing.T) {
	manifests := map[string][]string{
		"gomod":          {"go.mod"},
		"npm":            {"package.json"},
		"docker":         {"Dockerfile"},
		"github-actions": {"action.yml", "action.yaml"},
	}

	for _, update := range readDependabotConfig(t).Updates {
		names, known := manifests[update.Ecosystem]
		if !known {
			t.Errorf("no manifest names known for ecosystem %q; teach this test about it", update.Ecosystem)
			continue
		}
		for _, dir := range update.dirs() {
			// The github-actions root entry is satisfied by .github/workflows.
			if update.Ecosystem == "github-actions" && dir == "/" {
				names = append(names, filepath.Join(".github", "workflows"))
			}
			if !anyExists(t, dir, names) {
				t.Errorf("%s entry covers %q, which contains none of %v", update.Ecosystem, dir, names)
			}
		}
	}
}

func anyExists(t *testing.T, dir string, names []string) bool {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(repositoryPath(filepath.Join(strings.TrimPrefix(dir, "/"), name))); err == nil {
			return true
		}
	}
	return false
}

// web/dashboard builds into pkg/web/go-grpc/out, which go:embed ships inside
// the binary, and out/ is committed. No Go gate can see an npm dependency bump:
// TestHandlerServesDashboard asserts against the committed bundle, so it passes
// whether or not that bundle still corresponds to the declared dependencies.
// Managing those dependencies without a job that rebuilds and compares the
// bundle means merging untested changes into a shipped artifact.
func TestDashboardDependenciesAreGatedByARebuildJob(t *testing.T) {
	var managed bool
	for _, update := range readDependabotConfig(t).Updates {
		if update.Ecosystem == "npm" && slices.Contains(update.dirs(), "/web/dashboard") {
			managed = true
		}
	}
	if !managed {
		t.Skip("web/dashboard npm dependencies are not Dependabot-managed")
	}

	var workflow goWorkflow
	readRepositoryYAML(t, ".github/workflows/go.yml", &workflow)
	job, found := workflow.Jobs["dashboard"]
	if !found {
		t.Fatal("go.yml has no dashboard job, so an npm bump under web/dashboard would merge with no signal at all")
	}

	var rebuilds, compares bool
	for _, step := range job.Steps {
		if strings.Contains(step.Run, "npm ci") && strings.Contains(step.Run, "npm run build") {
			rebuilds = true
		}
		if strings.Contains(step.Run, "git diff") && strings.Contains(step.Run, "pkg/web/go-grpc/out") {
			compares = true
		}
	}
	if !rebuilds {
		t.Error("dashboard job never runs `npm ci` and `npm run build`, so it does not exercise the declared dependencies")
	}
	if !compares {
		t.Error("dashboard job never compares pkg/web/go-grpc/out, so a stale committed bundle would pass")
	}
}

// A docker entry only does something if Dependabot can compare the tags it
// finds. dependabot-core skips a FROM carrying neither tag nor digest outright
// (its version is tag || digest), and treats a tag with no version component —
// `golang:alpine`, `alpine` — as permanently up to date. An entry over such a
// Dockerfile is a weekly job that provably cannot ever open a pull request,
// while reading in review as coverage the repository does not have. This
// repository has no docker entry today; if one returns, its bases must carry a
// version Dependabot can order.
func TestDockerEntriesCoverVersionComparableBaseImages(t *testing.T) {
	hasDigit := regexp.MustCompile(`[0-9]`)

	for _, update := range readDependabotConfig(t).Updates {
		if update.Ecosystem != "docker" {
			continue
		}
		for _, dir := range update.dirs() {
			path := repositoryPath(filepath.Join(strings.TrimPrefix(dir, "/"), "Dockerfile"))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("docker entry covers %q but its Dockerfile is unreadable: %v", dir, err)
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
					continue
				}
				image := fields[1]
				if strings.Contains(image, "@sha256:") {
					continue
				}
				_, tag, tagged := strings.Cut(image, ":")
				if !tagged || !hasDigit.MatchString(tag) {
					t.Errorf("%s/Dockerfile: %q has no version-comparable tag, so Dependabot can never update it", dir, image)
				}
			}
		}
	}
}
