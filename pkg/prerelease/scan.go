package prerelease

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Carrier is the kind of declaration a version was found in. It is what decides
// whether the gate refuses the finding; see the package comment.
type Carrier string

const (
	// CarrierConfig is a `version:` key in a *.codefly.yaml — an agent pin, a
	// module or solution version, a library's own version.
	CarrierConfig Carrier = "config"
	// CarrierAgentOverride is an entry of workspace.codefly.yaml's top-level
	// agent-overrides block, the one sanctioned prerelease carrier.
	CarrierAgentOverride Carrier = "agent-override"
	// CarrierGoModule is a first-party require in a go.mod.
	CarrierGoModule Carrier = "go-module"
)

// DefaultFirstParty are the module path prefixes whose pseudo-versions are
// first-party to this fleet: a prerelease of something the fleet itself
// publishes and could instead release. A third-party pseudo-version is somebody
// else's release cadence and is none of this gate's business.
var DefaultFirstParty = []string{
	"github.com/codefly-dev/",
	"github.com/obin-ai/",
}

// Options configures a scan.
type Options struct {
	// Release scopes the scan to a release rather than to the default branch:
	// the agent-overrides exception does not apply, so a tag cannot be cut over
	// a dev override however well labelled. This is the scope that guarantees the
	// property that actually broke.
	Release bool
	// GoModules makes first-party go.mod pseudo-versions refuse rather than
	// report. Off by default — see the package comment.
	GoModules bool
	// IncludeTestdata scans testdata/ trees, which are skipped by default.
	//
	// A test fixture's whole job can be to carry a bad pin: this package's own
	// fixtures do, and the CLI repository tracks 138 *.codefly.yaml and go.mod
	// files under testdata against one real go.mod, so a gate that read them
	// could not be run on the repository that implements it. No real
	// configuration is lost to this — none of the seven fleet repositories the
	// gate was validated against keeps a *.codefly.yaml or a go.mod under
	// testdata — and the flag exists so the exclusion is a choice rather than a
	// blind spot.
	IncludeTestdata bool
	// FirstParty overrides DefaultFirstParty.
	FirstParty []string
}

func (options Options) firstParty() []string {
	if len(options.FirstParty) > 0 {
		return options.FirstParty
	}
	return DefaultFirstParty
}

// Finding is one prerelease version in one declaration.
type Finding struct {
	// File is the path relative to the scanned directory, as the repository
	// spells it.
	File string `json:"file"`
	// Line is the line the version is written on, 1-based.
	Line int `json:"line"`
	// Key names the declaration: "agent.version", "modules[2].version",
	// "agent-overrides.codefly.dev/go", "require github.com/codefly-dev/core".
	Key string `json:"key"`
	// Version is the offending version, verbatim.
	Version string  `json:"version"`
	Kind    Kind    `json:"kind"`
	Carrier Carrier `json:"carrier"`
	// Label is the comment on an agent-overrides entry, when it has one.
	Label string `json:"label,omitempty"`
	// Labelled reports whether Label names the issue the override stands in for.
	Labelled bool `json:"labelled"`
	// Blocking is whether this finding fails the scan under the options it was
	// produced with.
	Blocking bool `json:"blocking"`
	// Why states the policy applied, so a report never has to re-derive it.
	Why string `json:"why"`
	// Remedy is what to do about it, in the order a person would try it, decided
	// where the policy is rather than re-derived by whatever renders the report —
	// under Options.Release, labelling an override is no longer a way out, and a
	// formatter has no business knowing that.
	Remedy []string `json:"remedy,omitempty"`
}

// Location is the "file:line" prefix a failure report leads each finding with.
func (finding *Finding) Location() string {
	return fmt.Sprintf("%s:%d", finding.File, finding.Line)
}

// Result is a completed scan.
type Result struct {
	// Dir is the directory scanned.
	Dir string
	// Files are the tracked files examined, relative to Dir, sorted.
	Files []string
	// Findings are every prerelease found, blocking or not, ordered by file then
	// line.
	Findings []Finding
	// Tracked reports whether the file set came from git. When false the
	// directory is not a git repository and the scan walked it instead, so a
	// gitignored file may have been read.
	Tracked bool
	// Options are the options the scan ran with.
	Options Options
}

// Blocking is the findings that fail the scan.
func (result *Result) Blocking() []Finding {
	var blocking []Finding
	for i := range result.Findings {
		if result.Findings[i].Blocking {
			blocking = append(blocking, result.Findings[i])
		}
	}
	return blocking
}

// Allowed is the findings that were found and permitted: a labelled dev override
// on the default branch, or a first-party pseudo-version while go.mod is only
// reported. They are worth printing — an allowed prerelease is still a prerelease
// somebody has to remove — but they do not fail.
func (result *Result) Allowed() []Finding {
	var allowed []Finding
	for i := range result.Findings {
		if !result.Findings[i].Blocking {
			allowed = append(allowed, result.Findings[i])
		}
	}
	return allowed
}

// OK reports whether the scan found nothing that fails.
func (result *Result) OK() bool { return len(result.Blocking()) == 0 }

// Scan walks dir for prerelease version pins. It needs no workspace, no agent,
// and no network: it reads committed text, so it runs in a fresh clone in
// milliseconds, which is what makes it usable on every pull request.
func Scan(dir string, options Options) (*Result, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve scan directory: %w", err)
	}
	files, tracked, err := Discover(absolute)
	if err != nil {
		return nil, err
	}
	if !options.IncludeTestdata {
		files = withoutTestdata(files)
	}
	result := &Result{Dir: absolute, Files: files, Tracked: tracked, Options: options}
	for _, file := range files {
		content, err := os.ReadFile(filepath.Join(absolute, filepath.FromSlash(file)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Tracked but deleted in the working tree; the commit that
				// deletes it carries no version.
				continue
			}
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		findings, err := scanFile(file, content, options)
		if err != nil {
			return nil, err
		}
		result.Findings = append(result.Findings, findings...)
	}
	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].File != result.Findings[j].File {
			return result.Findings[i].File < result.Findings[j].File
		}
		if result.Findings[i].Line != result.Findings[j].Line {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].Key < result.Findings[j].Key
	})
	return result, nil
}

func scanFile(file string, content []byte, options Options) ([]Finding, error) {
	switch {
	case path.Base(file) == "go.mod":
		return scanGoMod(file, content, options)
	case isCodeflyConfig(file):
		return scanCodeflyConfig(file, content, options)
	}
	return nil, nil
}

// isCodeflyConfig reports whether a path is a codefly configuration document.
// Both spellings of the YAML extension are accepted because both are written.
func isCodeflyConfig(file string) bool {
	base := path.Base(file)
	return strings.HasSuffix(base, ".codefly.yaml") || strings.HasSuffix(base, ".codefly.yml")
}

// Discover is the file set a scan examines: every *.codefly.yaml and every
// go.mod under dir.
//
// Enumerated from git when dir is a repository, which is the point rather than an
// optimisation. The rule is about what reaches the default branch, and what
// reaches it is what git tracks: an untracked or ignored file cannot. That is
// also how the local dev loop stays free — codefly.local.yaml is gitignored, so
// it is outside the gate by construction and needs no exception written into it.
//
// Outside a repository (a release archive, a test fixture) it walks instead, and
// says so through Result.Tracked.
//
// This is the raw set; Scan drops testdata trees from it unless
// Options.IncludeTestdata asks for them.
func Discover(dir string) (files []string, tracked bool, err error) {
	if listed, ok := gitTrackedFiles(dir); ok {
		return keepScannable(listed), true, nil
	}
	err = filepath.WalkDir(dir, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(dir, full)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "." && skipDirectory(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		files = append(files, relative)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("walk %s: %w", dir, err)
	}
	return keepScannable(files), false, nil
}

// skipDirectory are the trees a committed version pin never lives in, skipped so
// a scan of a repository with installed dependencies or build output stays fast
// and quiet. They are also normally untracked, so this only matters on the walk
// fallback.
func skipDirectory(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".codefly", ".venv", "dist", "build", "target", ".next":
		return true
	}
	return false
}

// withoutTestdata drops every path under a testdata directory. See
// Options.IncludeTestdata.
func withoutTestdata(files []string) []string {
	kept := make([]string, 0, len(files))
	for _, file := range files {
		if !underTestdata(file) {
			kept = append(kept, file)
		}
	}
	return kept
}

func underTestdata(file string) bool {
	for _, segment := range strings.Split(path.Dir(file), "/") {
		if segment == "testdata" {
			return true
		}
	}
	return false
}

func keepScannable(files []string) []string {
	var kept []string
	for _, file := range files {
		if path.Base(file) == "go.mod" || isCodeflyConfig(file) {
			kept = append(kept, file)
		}
	}
	sort.Strings(kept)
	return kept
}

// gitTrackedFiles lists the files git tracks under dir, relative to dir. Not ok
// when dir is not inside a work tree, or git is unavailable.
func gitTrackedFiles(dir string) ([]string, bool) {
	inside, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(inside)) != "true" {
		return nil, false
	}
	out, err := exec.Command("git", "-C", dir, "ls-files", "-z").Output()
	if err != nil {
		return nil, false
	}
	var files []string
	for _, entry := range bytes.Split(out, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		files = append(files, filepath.ToSlash(string(entry)))
	}
	return files, true
}
