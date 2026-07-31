package integrity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
)

const baseManifestRelativePath = "tools/base-manifest.json"

// SourceInvalidReason distinguishes the three failures that all block a base
// sync but demand different remedies: a malformed manifest path, a missing or
// unreadable source file, and a source file whose content no longer matches the
// upstream manifest (a stale upstream release the consumer cannot fix).
type SourceInvalidReason string

const (
	SourceUnsafePath     SourceInvalidReason = "unsafe-path"
	SourceUnreadable     SourceInvalidReason = "unreadable"
	SourceDigestMismatch SourceInvalidReason = "digest-mismatch"
)

// InvalidSource is a source-owned path that cannot be applied, tagged with the
// reason it was rejected. For a digest mismatch it also carries the manifest
// and on-disk digests so the operator can see expected versus actual.
type InvalidSource struct {
	Path           string
	Reason         SourceInvalidReason
	ManifestDigest string
	ActualDigest   string
}

// BaseSyncPlan classifies every path before a composed module is changed.
// Source files are immutable base ownership; target-only files are product
// overlays and are deliberately absent from this plan.
type BaseSyncPlan struct {
	SourceRoot string
	TargetRoot string

	Unchanged  []string
	Create     []string
	Update     []string
	Remove     []string
	Released   []string
	Omitted    []string
	Allowed    []string
	Modified   []string
	Collisions []string
	// ResolveUpstream contains explicitly reviewed conflicts that will be
	// replaced by the immutable upstream version. ReconciledUpstream contains
	// requested paths that already match upstream, which makes an interrupted
	// reconciliation safe to resume with the same command.
	ResolveUpstream    []string
	ReconciledUpstream []string
	StaleModified      []string
	SourceInvalid      []InvalidSource
	TargetInvalid      []string

	MissingRequiredAdditions []string
	InvalidRequiredAdditions []string

	resolutionTargetDigests map[string]string
}

func (plan BaseSyncPlan) Applicable() error {
	var blockers []string
	add := func(label string, count int) {
		if count > 0 {
			blockers = append(blockers, fmt.Sprintf("%s=%d", label, count))
		}
	}
	add("invalid-source", len(plan.SourceInvalid))
	add("invalid-target", len(plan.TargetInvalid))
	add("modified-base", len(plan.Modified))
	add("overlay-collisions", len(plan.Collisions))
	add("modified-upstream-deletions", len(plan.StaleModified))
	add("missing-required-overlays", len(plan.MissingRequiredAdditions))
	add("invalid-required-overlays", len(plan.InvalidRequiredAdditions))
	if len(blockers) == 0 {
		return nil
	}
	return fmt.Errorf("base sync requires reconciliation: %s", strings.Join(blockers, ", "))
}

func PlanBaseSync(sourceRoot, targetRoot string) (BaseSyncPlan, error) {
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("resolve source module: %w", err)
	}
	targetRoot, err = filepath.Abs(targetRoot)
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("resolve target module: %w", err)
	}
	if sourceRoot == targetRoot {
		return BaseSyncPlan{}, fmt.Errorf("source and target module must differ")
	}

	sourceManifest, err := readBaseManifest(filepath.Join(sourceRoot, baseManifestRelativePath))
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("read source base manifest: %w", err)
	}
	targetManifest, err := readBaseManifest(filepath.Join(targetRoot, baseManifestRelativePath))
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("read target base manifest: %w", err)
	}
	allow, err := loadBaseIntegrityAllow(filepath.Join(targetRoot, "tools", "base-integrity-allow.json"))
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("read target base integrity policy: %w", err)
	}
	composed, err := composedServiceNames(targetRoot)
	if err != nil {
		return BaseSyncPlan{}, err
	}

	plan := BaseSyncPlan{SourceRoot: sourceRoot, TargetRoot: targetRoot}
	plan.MissingRequiredAdditions, plan.InvalidRequiredAdditions = validateRequiredAdditions(targetRoot, allow.RequiredAdditions)
	sourceInvalid := make(map[string]bool)
	targetInvalid := make(map[string]bool)
	for _, relative := range sortedManifestPaths(sourceManifest) {
		if !safeModulePath(sourceRoot, relative, true) {
			plan.SourceInvalid = append(plan.SourceInvalid, InvalidSource{Path: relative, Reason: SourceUnsafePath})
			sourceInvalid[relative] = true
		}
		if !safeModulePath(targetRoot, relative, false) {
			plan.TargetInvalid = append(plan.TargetInvalid, relative)
			targetInvalid[relative] = true
		}
	}
	for _, relative := range sortedManifestPaths(targetManifest) {
		if !safeModulePath(targetRoot, relative, false) && !targetInvalid[relative] {
			plan.TargetInvalid = append(plan.TargetInvalid, relative)
			targetInvalid[relative] = true
		}
	}

	for _, relative := range sortedManifestPaths(sourceManifest) {
		if sourceInvalid[relative] || targetInvalid[relative] {
			continue
		}
		if service := serviceOf(relative); service != "" && len(composed) > 0 && !composed[service] {
			plan.Omitted = append(plan.Omitted, relative)
			continue
		}
		if _, ok := allow.Divergences[relative]; ok {
			plan.Allowed = append(plan.Allowed, relative)
			continue
		}
		source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
		target := filepath.Join(targetRoot, filepath.FromSlash(relative))
		sourceDigest, digestErr := sha256File(source)
		if digestErr != nil {
			plan.SourceInvalid = append(plan.SourceInvalid, InvalidSource{Path: relative, Reason: SourceUnreadable})
			continue
		}
		if sourceDigest != sourceManifest.Files[relative] {
			plan.SourceInvalid = append(plan.SourceInvalid, InvalidSource{
				Path:           relative,
				Reason:         SourceDigestMismatch,
				ManifestDigest: sourceManifest.Files[relative],
				ActualDigest:   sourceDigest,
			})
			continue
		}
		targetDigest, targetErr := sha256File(target)
		if os.IsNotExist(targetErr) {
			plan.Create = append(plan.Create, relative)
			continue
		}
		if targetErr != nil {
			return BaseSyncPlan{}, fmt.Errorf("hash target %s: %w", relative, targetErr)
		}
		if targetDigest == sourceDigest {
			plan.Unchanged = append(plan.Unchanged, relative)
			continue
		}
		oldDigest, wasBase := targetManifest.Files[relative]
		if !wasBase {
			plan.Collisions = append(plan.Collisions, relative)
		} else if targetDigest == oldDigest {
			plan.Update = append(plan.Update, relative)
		} else {
			plan.Modified = append(plan.Modified, relative)
		}
	}

	for _, relative := range sortedManifestPaths(targetManifest) {
		if targetInvalid[relative] {
			continue
		}
		if _, stillBase := sourceManifest.Files[relative]; stillBase {
			continue
		}
		if _, ok := allow.Divergences[relative]; ok {
			continue
		}
		if service := serviceOf(relative); service != "" && len(composed) > 0 && !composed[service] {
			continue
		}
		target := filepath.Join(targetRoot, filepath.FromSlash(relative))
		if _, err := os.Stat(filepath.Join(sourceRoot, filepath.FromSlash(relative))); err == nil {
			plan.Released = append(plan.Released, relative)
			continue
		}
		targetDigest, targetErr := sha256File(target)
		if os.IsNotExist(targetErr) {
			continue
		}
		if targetErr != nil {
			return BaseSyncPlan{}, fmt.Errorf("hash retired target %s: %w", relative, targetErr)
		}
		if targetDigest == targetManifest.Files[relative] {
			plan.Remove = append(plan.Remove, relative)
		} else {
			plan.StaleModified = append(plan.StaleModified, relative)
		}
	}

	return plan, nil
}

// PlanBaseSyncWithResolutions applies explicit, path-by-path reconciliation
// decisions to a base sync plan. Only source-owned modified paths and overlay
// collisions can be replaced. A source-equal path is also accepted so the same
// command can safely resume after an interruption.
func PlanBaseSyncWithResolutions(sourceRoot, targetRoot string, acceptUpstream []string) (BaseSyncPlan, error) {
	plan, err := PlanBaseSync(sourceRoot, targetRoot)
	if err != nil {
		return BaseSyncPlan{}, err
	}
	if len(acceptUpstream) == 0 {
		return plan, nil
	}
	sourceManifest, err := readBaseManifest(filepath.Join(plan.SourceRoot, baseManifestRelativePath))
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("re-read source base manifest: %w", err)
	}
	allow, err := loadBaseIntegrityAllow(filepath.Join(plan.TargetRoot, "tools", "base-integrity-allow.json"))
	if err != nil {
		return BaseSyncPlan{}, fmt.Errorf("re-read target base integrity policy: %w", err)
	}

	plan.resolutionTargetDigests = make(map[string]string)
	seen := make(map[string]bool, len(acceptUpstream))
	for _, requested := range acceptUpstream {
		relative := strings.TrimSpace(requested)
		if _, ok := canonicalModulePath(relative); !ok {
			return BaseSyncPlan{}, fmt.Errorf("accepted upstream path %q is not a canonical module-relative path", requested)
		}
		if seen[relative] {
			return BaseSyncPlan{}, fmt.Errorf("accepted upstream path %q was provided more than once", relative)
		}
		seen[relative] = true
		if _, protected := allow.RequiredAdditions[relative]; protected {
			return BaseSyncPlan{}, fmt.Errorf("accepted upstream path %q is a required product addition and cannot be overwritten", relative)
		}
		if _, sourceOwned := sourceManifest.Files[relative]; !sourceOwned {
			return BaseSyncPlan{}, fmt.Errorf("accepted upstream path %q is not owned by the source base manifest", relative)
		}

		switch {
		case removeSortedPath(&plan.Modified, relative), removeSortedPath(&plan.Collisions, relative):
			digest, digestErr := sha256File(filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)))
			if digestErr != nil {
				return BaseSyncPlan{}, fmt.Errorf("hash accepted upstream target %s: %w", relative, digestErr)
			}
			plan.ResolveUpstream = append(plan.ResolveUpstream, relative)
			plan.resolutionTargetDigests[relative] = digest
		case containsSortedPath(plan.Unchanged, relative):
			plan.ReconciledUpstream = append(plan.ReconciledUpstream, relative)
		default:
			return BaseSyncPlan{}, fmt.Errorf("accepted upstream path %q is not a modified base path or overlay collision", relative)
		}
	}
	sort.Strings(plan.ResolveUpstream)
	sort.Strings(plan.ReconciledUpstream)
	return plan, nil
}

// ApplyBaseSync re-plans immediately before mutation and writes the new
// manifest last. Each file replacement is atomic. If the process is
// interrupted, rerunning converges safely because already-updated files match
// the new manifest while the old manifest remains the verification authority.
func ApplyBaseSync(sourceRoot, targetRoot string) (BaseSyncPlan, error) {
	plan, err := PlanBaseSync(sourceRoot, targetRoot)
	if err != nil {
		return BaseSyncPlan{}, err
	}
	if err := plan.Applicable(); err != nil {
		return plan, err
	}
	sourceManifest, err := readBaseManifest(filepath.Join(plan.SourceRoot, baseManifestRelativePath))
	if err != nil {
		return plan, fmt.Errorf("re-read source base manifest: %w", err)
	}
	targetManifest, err := readBaseManifest(filepath.Join(plan.TargetRoot, baseManifestRelativePath))
	if err != nil {
		return plan, fmt.Errorf("re-read target base manifest: %w", err)
	}
	for _, relative := range plan.Create {
		if _, err := os.Lstat(filepath.Join(plan.TargetRoot, filepath.FromSlash(relative))); !os.IsNotExist(err) {
			return plan, fmt.Errorf("base path %s changed after preview; re-run the dry-run", relative)
		}
		if digest, digestErr := sha256File(filepath.Join(plan.SourceRoot, filepath.FromSlash(relative))); digestErr != nil || digest != sourceManifest.Files[relative] {
			return plan, fmt.Errorf("source base path %s changed after preview", relative)
		}
		if err := atomicCopyFile(
			filepath.Join(plan.SourceRoot, filepath.FromSlash(relative)),
			filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)),
		); err != nil {
			return plan, fmt.Errorf("copy base file %s: %w", relative, err)
		}
	}
	for _, relative := range plan.Update {
		digest, digestErr := sha256File(filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)))
		if digestErr != nil || digest != targetManifest.Files[relative] {
			return plan, fmt.Errorf("base path %s changed after preview; re-run the dry-run", relative)
		}
		if digest, digestErr = sha256File(filepath.Join(plan.SourceRoot, filepath.FromSlash(relative))); digestErr != nil || digest != sourceManifest.Files[relative] {
			return plan, fmt.Errorf("source base path %s changed after preview", relative)
		}
		if err := atomicCopyFile(
			filepath.Join(plan.SourceRoot, filepath.FromSlash(relative)),
			filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)),
		); err != nil {
			return plan, fmt.Errorf("copy base file %s: %w", relative, err)
		}
	}
	for _, relative := range plan.Remove {
		digest, digestErr := sha256File(filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)))
		if digestErr != nil || digest != targetManifest.Files[relative] {
			return plan, fmt.Errorf("retired base path %s changed after preview; re-run the dry-run", relative)
		}
		if err := os.Remove(filepath.Join(plan.TargetRoot, filepath.FromSlash(relative))); err != nil && !os.IsNotExist(err) {
			return plan, fmt.Errorf("remove retired base file %s: %w", relative, err)
		}
	}
	finalPlan, err := PlanBaseSync(plan.SourceRoot, plan.TargetRoot)
	if err != nil {
		return plan, fmt.Errorf("verify updated base before commit: %w", err)
	}
	if err := finalPlan.Applicable(); err != nil {
		return plan, fmt.Errorf("base changed while applying update: %w", err)
	}
	if err := atomicCopyFile(
		filepath.Join(plan.SourceRoot, filepath.FromSlash(baseManifestRelativePath)),
		filepath.Join(plan.TargetRoot, filepath.FromSlash(baseManifestRelativePath)),
	); err != nil {
		return plan, fmt.Errorf("commit base manifest: %w", err)
	}
	return plan, nil
}

// ApplyBaseSyncWithResolutions replaces only explicitly reviewed conflicts,
// then applies the ordinary fail-closed base update. Source and target digests
// are checked again immediately before every accepted replacement.
func ApplyBaseSyncWithResolutions(sourceRoot, targetRoot string, acceptUpstream []string) (BaseSyncPlan, error) {
	plan, err := PlanBaseSyncWithResolutions(sourceRoot, targetRoot, acceptUpstream)
	if err != nil {
		return BaseSyncPlan{}, err
	}
	if err := plan.Applicable(); err != nil {
		return plan, err
	}
	sourceManifest, err := readBaseManifest(filepath.Join(plan.SourceRoot, baseManifestRelativePath))
	if err != nil {
		return plan, fmt.Errorf("re-read source base manifest: %w", err)
	}
	for _, relative := range plan.ResolveUpstream {
		targetDigest, digestErr := sha256File(filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)))
		if digestErr != nil || targetDigest != plan.resolutionTargetDigests[relative] {
			return plan, fmt.Errorf("accepted upstream target %s changed after preview; re-run the dry-run", relative)
		}
		sourceDigest, digestErr := sha256File(filepath.Join(plan.SourceRoot, filepath.FromSlash(relative)))
		if digestErr != nil || sourceDigest != sourceManifest.Files[relative] {
			return plan, fmt.Errorf("source base path %s changed after preview", relative)
		}
		if err := atomicCopyFile(
			filepath.Join(plan.SourceRoot, filepath.FromSlash(relative)),
			filepath.Join(plan.TargetRoot, filepath.FromSlash(relative)),
		); err != nil {
			return plan, fmt.Errorf("resolve base file %s from upstream: %w", relative, err)
		}
	}
	if _, err := ApplyBaseSync(plan.SourceRoot, plan.TargetRoot); err != nil {
		return plan, err
	}
	return plan, nil
}

func readBaseManifest(path string) (baseManifest, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return baseManifest{}, err
	}
	var manifest baseManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		return baseManifest{}, err
	}
	if manifest.Files == nil {
		return baseManifest{}, fmt.Errorf("manifest has no files map")
	}
	return manifest, nil
}

func sortedManifestPaths(manifest baseManifest) []string {
	paths := make([]string, 0, len(manifest.Files))
	for relative := range manifest.Files {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	return paths
}

func containsSortedPath(paths []string, relative string) bool {
	index := sort.SearchStrings(paths, relative)
	return index < len(paths) && paths[index] == relative
}

func removeSortedPath(paths *[]string, relative string) bool {
	index := sort.SearchStrings(*paths, relative)
	if index >= len(*paths) || (*paths)[index] != relative {
		return false
	}
	*paths = append((*paths)[:index], (*paths)[index+1:]...)
	return true
}

func composedServiceNames(targetRoot string) (map[string]bool, error) {
	module, err := resources.LoadModuleFromDir(context.Background(), targetRoot)
	if err != nil {
		return nil, fmt.Errorf("load target module composition: %w", err)
	}
	services := make(map[string]bool, len(module.ServiceReferences))
	for _, reference := range module.ServiceReferences {
		services[reference.Name] = true
	}
	return services, nil
}

func validateRequiredAdditions(root string, required map[string]string) (missing, invalid []string) {
	for relative, reason := range required {
		if strings.TrimSpace(reason) == "" || !safeModulePath(root, relative, false) {
			invalid = append(invalid, relative)
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative)))
		if os.IsNotExist(err) {
			missing = append(missing, relative)
			continue
		}
		if err != nil || !info.Mode().IsRegular() {
			invalid = append(invalid, relative)
		}
	}
	sort.Strings(missing)
	sort.Strings(invalid)
	return missing, invalid
}

func atomicCopyFile(source, target string) error {
	payload, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".codefly-base-sync-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, target)
}

// safeModulePath rejects path traversal and symlink components. A canonical
// manifest is data from outside the consumer trust boundary; it must never be
// able to read, replace, or remove a path outside the selected module.
func safeModulePath(root, relative string, requireExistingFile bool) bool {
	clean, ok := canonicalModulePath(relative)
	if !ok {
		return false
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return !requireExistingFile
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if index < len(parts)-1 && !info.IsDir() {
			return false
		}
		if index == len(parts)-1 && requireExistingFile && !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func canonicalModulePath(relative string) (string, bool) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != relative {
		return "", false
	}
	return clean, true
}
