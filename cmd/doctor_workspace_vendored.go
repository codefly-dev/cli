package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

const (
	codeVendoredPackageBehindPin    = "vendored_package_behind_pin"
	codeVendoredPackageDigest       = "vendored_package_digest_mismatch"
	codeVendoredPackageUncomparable = "vendored_package_uncomparable"
)

// vendorProvenance is the record a module keeps beside a package archive it
// vendors (<dir>/*.provenance.json): which repository and commit the archive
// was built from. The repository may be stated once for every package or per
// package.
type vendorProvenance struct {
	Repository string                    `json:"repository"`
	Packages   []vendorProvenancePackage `json:"packages"`
}

type vendorProvenancePackage struct {
	Repository string `json:"repository"`
	Package    string `json:"package"`
	Commit     string `json:"commit"`
	Tag        string `json:"tag"`
	Directory  string `json:"directory"`
	PackageDir string `json:"package_directory"`
	Artifact   string `json:"artifact"`
	SHA256     string `json:"sha256"`
}

// vendoredArchive is one archive a package.json of a composed module selects
// (`file:…tgz`), with the provenance entry that names it.
type vendoredArchive struct {
	consumer string // the composed module that vendors it
	manifest string // the package.json selecting it, module-relative
	archive  string // absolute path of the archive
	recorded vendorProvenancePackage
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkVendoredPackages compares every package archive a composed module
// vendors with the module the workspace composes from the same repository. A
// module that bundles another module's package as an archive ships the build
// it vendored, not the version the workspace pins, so moving the pin moves
// the service and leaves the bundled interface where it was — a fix that
// deploys and changes nothing on screen. The archive is compared by the commit
// its provenance records against the pin; and its bytes against the digest the
// provenance records, since a commit vouches only for the bytes built from it.
//
// Only archives a package.json selects are read: a provenance file may keep
// older builds as history, which nothing installs.
//
// Scoped to one module (a verb acting on exactly one), only the archives that
// module vendors are judged.
func checkVendoredPackages(ctx context.Context, ws *resources.Workspace, scope string, report *workspaceReadinessReport) {
	var archives []vendoredArchive
	sources := make([]*resources.ModuleReference, 0, len(ws.Modules))
	overlay, err := resources.LoadLocalOverlay(ctx, ws.Dir())
	if err != nil {
		report.add(codeVendoredPackageUncomparable, "vendored package sources", "warn", err.Error(), "")
		return
	}
	for _, ref := range ws.Modules {
		source := *ref
		sources = append(sources, &source)
		resolution, err := ws.ResolveModule(ctx, ref)
		if err != nil || resolution.Dir == "" {
			// Materialization and resolution are reported by their own checks.
			continue
		}
		if overlay != nil && overlay.Resolve[ref.Name] != nil && (resolution.Kind == resources.ResolutionLocalPath || resolution.Kind == resources.ResolutionWorktree) && ref.Source != "" {
			// An explicit checkout overrides the committed pin. Without local
			// revision evidence the archive can only be reported as uncomparable.
			revision, _ := exec.CommandContext(ctx, "git", "-C", resolution.Dir, "rev-parse", "--verify", "HEAD").Output()
			source.Version = strings.TrimSpace(string(revision))
		}
		if scope != "" && ref.Name != scope {
			continue
		}
		found, err := vendoredArchives(ref.Name, resolution.Dir)
		if err != nil {
			report.add(codeVendoredPackageUncomparable, "vendored packages of "+ref.Name, checkStatusWarn,
				fmt.Sprintf("the package archives module %q vendors cannot be read: %v", ref.Name, err), "")
			continue
		}
		archives = append(archives, found...)
	}
	for _, finding := range judgeVendoredArchives(sources, archives) {
		report.add(finding.Code, finding.Name, finding.Status, finding.Message, finding.Remediation)
	}
}

// vendoredArchives lists the archives a module's package.json files select by
// a `file:` specifier, each with the provenance entry that names it. An archive
// no provenance names is not compared: nothing says where it came from.
func vendoredArchives(module, dir string) ([]vendoredArchive, error) {
	var out []vendoredArchive
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); name == "node_modules" || name == ".git" || name == ".codefly" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "package.json" {
			return nil
		}
		selected, err := selectedArchives(path)
		if err != nil {
			return err
		}
		manifest, _ := filepath.Rel(dir, path)
		for _, archive := range selected {
			recorded, ok, err := provenanceOf(archive)
			if err != nil {
				return err
			}
			if ok {
				out = append(out, vendoredArchive{consumer: module, manifest: filepath.ToSlash(manifest), archive: archive, recorded: recorded})
			}
		}
		return nil
	})
	return out, err
}

// selectedArchives are the archives one package.json depends on by path.
func selectedArchives(manifest string) ([]string, error) {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	var pkg map[string]json.RawMessage
	if err = json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("%s: %w", manifest, err)
	}
	var archives []string
	for _, field := range []string{"dependencies", "devDependencies", "optionalDependencies", "peerDependencies"} {
		var deps map[string]string
		if raw, ok := pkg[field]; !ok || json.Unmarshal(raw, &deps) != nil {
			continue
		}
		for _, spec := range deps {
			target, ok := strings.CutPrefix(spec, "file:")
			if !ok || !strings.HasSuffix(target, ".tgz") && !strings.HasSuffix(target, ".tar.gz") {
				continue
			}
			archives = append(archives, filepath.Join(filepath.Dir(manifest), filepath.FromSlash(target)))
		}
	}
	sort.Strings(archives)
	return archives, nil
}

// provenanceOf finds the provenance entry, beside the archive, whose artifact
// is the archive.
func provenanceOf(archive string) (vendorProvenancePackage, bool, error) {
	records, err := filepath.Glob(filepath.Join(filepath.Dir(archive), "*.provenance.json"))
	if err != nil {
		return vendorProvenancePackage{}, false, err
	}
	sort.Strings(records)
	for _, record := range records {
		data, err := os.ReadFile(record)
		if err != nil {
			return vendorProvenancePackage{}, false, err
		}
		var provenance vendorProvenance
		if err = json.Unmarshal(data, &provenance); err != nil {
			return vendorProvenancePackage{}, false, fmt.Errorf("%s: %w", record, err)
		}
		for i := range provenance.Packages {
			entry := &provenance.Packages[i]
			if entry.Artifact != filepath.Base(archive) {
				continue
			}
			if entry.Repository == "" {
				entry.Repository = provenance.Repository
			}
			return *entry, true, nil
		}
	}
	return vendorProvenancePackage{}, false, nil
}

// repositoryKey reduces a repository spelling (https://host/owner/repo.git,
// owner/repo) to owner/repo, lowercased, so a provenance URL meets a module
// reference's source.
func repositoryKey(repository string) string {
	key := strings.TrimSuffix(strings.TrimRight(strings.ToLower(strings.TrimSpace(repository)), "/"), ".git")
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return key
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// sourceModule is the composed module an archive was built from: the module
// of its repository whose directory holds the archive's package directory, or
// the repository's only composed module.
func sourceModule(modules []*resources.ModuleReference, recorded *vendorProvenancePackage) *resources.ModuleReference {
	var candidates []*resources.ModuleReference
	for _, ref := range modules {
		if ref.Source != "" && repositoryKey(ref.Source) == repositoryKey(recorded.Repository) {
			candidates = append(candidates, ref)
		}
	}
	directory := recorded.PackageDir
	if directory == "" {
		directory = recorded.Directory
	}
	for _, ref := range candidates {
		if ref.Module != "" && (directory == ref.Module || strings.HasPrefix(directory, strings.TrimSuffix(ref.Module, "/")+"/")) {
			return ref
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return nil
}

// judgeVendoredArchives returns one finding per archive whose source module
// the workspace composes: behind or beside the pin, digest mismatch, or not
// comparable. Archives of a repository the workspace does not compose are
// another release train's, and are not judged.
func judgeVendoredArchives(modules []*resources.ModuleReference, archives []vendoredArchive) []workspaceDiagnostic {
	var findings []workspaceDiagnostic
	compared := 0
	for i := range archives {
		archive := &archives[i]
		source := sourceModule(modules, &archive.recorded)
		if source == nil || source.Name == archive.consumer {
			continue
		}
		name := fmt.Sprintf("vendored %s in %s", archive.recorded.Package, archive.consumer)
		where := fmt.Sprintf("%s (%s)", archive.manifest, filepath.Base(archive.archive))
		if digest, err := fileSHA256(archive.archive); err != nil || (archive.recorded.SHA256 != "" && digest != archive.recorded.SHA256) {
			message := fmt.Sprintf("module %q selects %s, whose bytes are not the build its provenance records (sha256 %s)", archive.consumer, where, archive.recorded.SHA256)
			if err != nil {
				message = fmt.Sprintf("module %q selects %s, which cannot be read: %v", archive.consumer, where, err)
			}
			findings = append(findings, workspaceDiagnostic{Code: codeVendoredPackageDigest, Name: name, Status: checkStatusFail, Message: message,
				Remediation: "rebuild the archive by its provenance's recipe, or record the digest of the bytes it holds"})
			continue
		}
		pin := strings.ToLower(strings.TrimSpace(source.Version))
		commit := strings.ToLower(strings.TrimSpace(archive.recorded.Commit))
		switch {
		case fullCommit.MatchString(pin) && fullCommit.MatchString(commit):
			compared++
			if pin == commit {
				continue
			}
			findings = append(findings, workspaceDiagnostic{Code: codeVendoredPackageBehindPin, Name: name, Status: checkStatusFail,
				Message: fmt.Sprintf("module %q bundles %s built from %s at %s, but the workspace composes module %q at %s: the deployed interface is not the module it calls",
					archive.consumer, where, source.Source, short(commit), source.Name, short(pin)),
				Remediation: fmt.Sprintf("re-vendor %s from %s at %s and record that commit in its provenance, or move module %q's pin to %s", archive.recorded.Package, source.Source, short(pin), source.Name, short(commit))})
		case pin != "" && archive.recorded.Tag != "" && !fullCommit.MatchString(pin):
			compared++
			if pin == strings.ToLower(archive.recorded.Tag) {
				continue
			}
			findings = append(findings, workspaceDiagnostic{Code: codeVendoredPackageBehindPin, Name: name, Status: checkStatusFail,
				Message:     fmt.Sprintf("module %q bundles %s built from %s tag %s, but the workspace composes module %q at %s", archive.consumer, where, source.Source, archive.recorded.Tag, source.Name, source.Version),
				Remediation: fmt.Sprintf("re-vendor %s from %s at %s, or move module %q's pin to %s", archive.recorded.Package, source.Source, source.Version, source.Name, archive.recorded.Tag)})
		default:
			findings = append(findings, workspaceDiagnostic{Code: codeVendoredPackageUncomparable, Name: name, Status: checkStatusWarn,
				Message:     fmt.Sprintf("module %q bundles %s from %s, recorded at %q, while module %q is pinned at %q: one is not a full commit and no tag joins them, so the two cannot be compared offline", archive.consumer, where, source.Source, archive.recorded.Commit, source.Name, source.Version),
				Remediation: "pin the module by commit, or record the full commit (and tag) the archive was built from in its provenance"})
		}
	}
	if len(findings) == 0 && compared > 0 {
		findings = append(findings, workspaceDiagnostic{Name: "vendored packages", Status: "ok",
			Message: fmt.Sprintf("%d vendored archive(s) built at their source module's pin", compared)})
	}
	return findings
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
