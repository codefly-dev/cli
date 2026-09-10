package integrity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// generatedFileMarker matches the leading-comment marker that machine-generated
// files carry ("# Code generated ... DO NOT EDIT."); generatedFileSource pulls
// out the "from <source>" clause when the marker names what it was rendered
// from. This is the single ownership signal for machine-generated files, shared
// by base sync and `codefly update`.
var (
	generatedFileMarker = regexp.MustCompile(`Code generated .* DO NOT EDIT\.`)
	generatedFileSource = regexp.MustCompile(`Code generated from (.+?)\. DO NOT EDIT\.`)
)

const baseManifestRelativePath = "tools/base-manifest.json"

// moduleInterfaceKey is the generated section of module.codefly.yaml: the
// module's contract, rendered from the base-owned topology bindings.
const moduleInterfaceKey = "interface"

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

	Unchanged []string
	Create    []string
	Update    []string
	Remove    []string
	Released  []string
	Omitted   []string
	Allowed   []string
	// AllowedUpstreamChanged is the subset of Allowed whose pinned upstream digest
	// changed since the target's recorded base. The divergence entry is masking a
	// real upstream update that this sync would otherwise keep local — surfaced,
	// and blocking by default, so the drop is never invisible and never unattended.
	AllowedUpstreamChanged []string
	// AllowedUpstreamRemoved is the set of allow-listed paths upstream retired
	// (present in the recorded base, absent from the pinned source) whose local
	// file still exists. The divergence masks an upstream removal; it is the
	// deletion sibling of AllowedUpstreamChanged and is treated identically.
	AllowedUpstreamRemoved []string
	Modified               []string
	Collisions             []string
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
	// divergencesReaffirmed records the operator's explicit --keep-local-divergences
	// decision. It is the only thing that lets a masked upstream change or removal
	// stop blocking, so an unattended apply cannot silently drop one.
	divergencesReaffirmed bool
}

// MaskedUpstreamBlocked reports whether this plan is withheld solely because an
// allow-listed divergence is masking an upstream change or removal that the
// operator has not yet re-affirmed. It exists so the plan printer can explain
// the block (and its remedy) without duplicating the Applicable() policy.
func (plan *BaseSyncPlan) MaskedUpstreamBlocked() bool {
	return !plan.divergencesReaffirmed &&
		(len(plan.AllowedUpstreamChanged) > 0 || len(plan.AllowedUpstreamRemoved) > 0)
}

type blockerGroup struct {
	label string
	paths []string
}

// blockerGroups is the single source of truth for what withholds an apply, so
// Applicable's error string and WithheldPaths' count can never drift apart. Each
// group carries its paths, not just a count, so WithheldPaths can dedup a path
// that lands in more than one group (a manifest path can be both an invalid
// source and an invalid target).
func (plan *BaseSyncPlan) blockerGroups() []blockerGroup {
	invalidSource := make([]string, len(plan.SourceInvalid))
	for i, entry := range plan.SourceInvalid {
		invalidSource[i] = entry.Path
	}
	groups := []blockerGroup{
		{"invalid-source", invalidSource},
		{"invalid-target", plan.TargetInvalid},
		{"modified-base", plan.Modified},
		{"overlay-collisions", plan.Collisions},
		{"modified-upstream-deletions", plan.StaleModified},
		{"missing-required-overlays", plan.MissingRequiredAdditions},
		{"invalid-required-overlays", plan.InvalidRequiredAdditions},
	}
	// A masked upstream change/removal blocks by default: leaving it non-blocking
	// is exactly how an unattended --apply shipped stale allow-listed code with a
	// zero exit. --keep-local-divergences (divergencesReaffirmed) is the explicit,
	// once-per-change acknowledgement that lets an intentional divergence proceed.
	if !plan.divergencesReaffirmed {
		groups = append(groups,
			blockerGroup{"allowed-upstream-changed", plan.AllowedUpstreamChanged},
			blockerGroup{"allowed-upstream-removed", plan.AllowedUpstreamRemoved},
		)
	}
	return groups
}

func (plan *BaseSyncPlan) Applicable() error {
	var blockers []string
	for _, group := range plan.blockerGroups() {
		if len(group.paths) > 0 {
			blockers = append(blockers, fmt.Sprintf("%s=%d", group.label, len(group.paths)))
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	return fmt.Errorf("base sync requires reconciliation: %s", strings.Join(blockers, ", "))
}

// WithheldPaths counts the distinct files that block advancement — the set
// Applicable() refuses on, deduplicated so a path flagged in two groups counts
// once. When non-zero, an --apply is withheld and the recorded base does not
// advance to the target tag.
func (plan *BaseSyncPlan) WithheldPaths() int {
	withheld := make(map[string]struct{})
	for _, group := range plan.blockerGroups() {
		for _, path := range group.paths {
			withheld[path] = struct{}{}
		}
	}
	return len(withheld)
}

// ReachesTarget reports whether applying this plan lands a tree that fully
// matches the pinned target tag. It is false when a blocker withholds the apply
// or when an allow-listed divergence masks an upstream change or removal and
// keeps the local version — so even a re-affirmed apply that advances the
// recorded base does not "reach" the tag byte-for-byte.
func (plan *BaseSyncPlan) ReachesTarget() bool {
	return plan.WithheldPaths() == 0 &&
		len(plan.AllowedUpstreamChanged) == 0 &&
		len(plan.AllowedUpstreamRemoved) == 0
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
	targetManifest, targetManifestPresent, err := readTargetBaseManifest(filepath.Join(targetRoot, baseManifestRelativePath))
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
		switch inspectModulePath(sourceRoot, relative, true) {
		case modulePathMissing:
			plan.SourceInvalid = append(plan.SourceInvalid, InvalidSource{Path: relative, Reason: SourceUnreadable})
			sourceInvalid[relative] = true
		case modulePathUnsafe:
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
			// An allow-listed divergence is kept local unconditionally. If the
			// pinned upstream digest moved since the recorded base, this sync is
			// dropping a real upstream change — record it so it is reported, not
			// folded silently into the allowed=N count.
			if base, wasBase := targetManifest.Files[relative]; wasBase && sourceManifest.Files[relative] != base {
				plan.AllowedUpstreamChanged = append(plan.AllowedUpstreamChanged, relative)
			}
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
			// Upstream retired this base path (it was in the recorded base but the
			// pinned source dropped it) while an allow-listed divergence keeps a
			// local copy. If that copy still exists, the sync is masking an upstream
			// removal — the deletion sibling of AllowedUpstreamChanged. Record it so
			// it is surfaced and blocks by default rather than vanishing here.
			if _, statErr := os.Stat(filepath.Join(targetRoot, filepath.FromSlash(relative))); statErr == nil {
				plan.AllowedUpstreamRemoved = append(plan.AllowedUpstreamRemoved, relative)
			}
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

	// A synthesized (missing) target manifest is only safe for a never-populated
	// scaffold, where every base path is a create. If base-owned files already
	// exist and differ from the pinned source, the manifest was lost from a
	// composed module rather than absent from a fresh one: without it the plan
	// cannot tell a user-modified base file from an overlay collision, so it
	// would offer --accept-upstream on paths it should protect. Fail closed and
	// point at the missing manifest instead.
	if !targetManifestPresent && len(plan.Collisions) > 0 {
		return BaseSyncPlan{}, fmt.Errorf(
			"target base manifest %s is missing while %d base-owned file(s) already exist and differ from the pinned source (%s); this is a composed module whose manifest was lost, not a fresh scaffold. Restore %s from version control before syncing",
			baseManifestRelativePath, len(plan.Collisions), strings.Join(plan.Collisions, ", "), baseManifestRelativePath)
	}

	return plan, nil
}

// PlanBaseSyncWithResolutions applies explicit, path-by-path reconciliation
// decisions to a base sync plan. Only source-owned modified paths and overlay
// collisions can be replaced. A source-equal path is also accepted so the same
// command can safely resume after an interruption.
func PlanBaseSyncWithResolutions(sourceRoot, targetRoot string, acceptUpstream []string, keepLocalDivergences bool) (BaseSyncPlan, error) {
	plan, err := PlanBaseSync(sourceRoot, targetRoot)
	if err != nil {
		return BaseSyncPlan{}, err
	}
	plan.divergencesReaffirmed = keepLocalDivergences
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
func ApplyBaseSync(sourceRoot, targetRoot string, keepLocalDivergences bool) (BaseSyncPlan, error) {
	plan, err := PlanBaseSync(sourceRoot, targetRoot)
	if err != nil {
		return BaseSyncPlan{}, err
	}
	plan.divergencesReaffirmed = keepLocalDivergences
	if err := plan.Applicable(); err != nil {
		return plan, err
	}
	sourceManifest, err := readBaseManifest(filepath.Join(plan.SourceRoot, baseManifestRelativePath))
	if err != nil {
		return plan, fmt.Errorf("re-read source base manifest: %w", err)
	}
	targetManifest, _, err := readTargetBaseManifest(filepath.Join(plan.TargetRoot, baseManifestRelativePath))
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
	finalPlan.divergencesReaffirmed = keepLocalDivergences
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
func ApplyBaseSyncWithResolutions(sourceRoot, targetRoot string, acceptUpstream []string, keepLocalDivergences bool) (BaseSyncPlan, error) {
	plan, err := PlanBaseSyncWithResolutions(sourceRoot, targetRoot, acceptUpstream, keepLocalDivergences)
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
	if _, err := ApplyBaseSync(plan.SourceRoot, plan.TargetRoot, keepLocalDivergences); err != nil {
		return plan, err
	}
	return plan, nil
}

// RestoreMissingServiceCode recreates absent base-owned service code without
// changing any existing base file, consumer overlay, or manifest.
func RestoreMissingServiceCode(sourceRoot, targetRoot string) ([]string, error) {
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve source module: %w", err)
	}
	targetRoot, err = filepath.Abs(targetRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve target module: %w", err)
	}
	if sourceRoot == targetRoot {
		return nil, fmt.Errorf("source and target module must differ")
	}
	if validationErr := ValidateServiceCodeSource(sourceRoot, targetRoot); validationErr != nil {
		return nil, validationErr
	}
	targetManifest, err := readBaseManifest(filepath.Join(targetRoot, baseManifestRelativePath))
	if err != nil {
		return nil, fmt.Errorf("re-read target base manifest: %w", err)
	}
	allow, err := loadBaseIntegrityAllow(filepath.Join(targetRoot, "tools", "base-integrity-allow.json"))
	if err != nil {
		return nil, fmt.Errorf("read target base integrity policy: %w", err)
	}
	composed, err := composedServiceNames(targetRoot)
	if err != nil {
		return nil, err
	}
	restored := make([]string, 0)
	for _, relative := range sortedManifestPaths(targetManifest) {
		if !selectedServiceCodePath(relative, composed) {
			continue
		}
		if _, allowed := allow.Divergences[relative]; allowed {
			continue
		}
		target := filepath.Join(targetRoot, filepath.FromSlash(relative))
		switch inspectModulePath(targetRoot, relative, true) {
		case modulePathSafe:
			continue
		case modulePathUnsafe:
			return restored, fmt.Errorf("cannot restore unsafe target path %s", relative)
		}
		source := filepath.Join(sourceRoot, filepath.FromSlash(relative))
		if digest, digestErr := sha256File(source); digestErr != nil || digest != targetManifest.Files[relative] {
			return restored, fmt.Errorf("source base path %s changed before restore", relative)
		}
		if err := atomicCreateFile(source, target); err != nil {
			return restored, fmt.Errorf("restore base file %s: %w", relative, err)
		}
		restored = append(restored, relative)
	}
	return restored, nil
}

// PlanServiceManifestRefresh lists the composed-service generated manifests a
// refresh would rewrite from the pinned source, sorted, without mutating.
func PlanServiceManifestRefresh(sourceRoot, targetRoot string) ([]string, error) {
	sourceRoot, targetRoot, err := refreshRoots(sourceRoot, targetRoot)
	if err != nil {
		return nil, err
	}
	return serviceManifestRefreshCandidates(sourceRoot, targetRoot)
}

// RefreshServiceManifests overwrites each composed service's generated
// service.codefly.yaml with the pinned source's copy, returning the rewritten
// paths sorted. A per-service service.codefly.yaml is a generated overlay
// (marked "DO NOT EDIT") absent from the base manifest, so an ordinary base sync
// never touches it and its agent pins drift stale against the module release.
// The pinned source carries the canonical regenerated manifest for each service
// at that version, so copying it re-aligns the consumer's manifests — agent pins
// included — with the version being synced. Only a manifest that still declares
// itself generated is refreshed; a consumer manifest without that marker is
// hand-authored product content and is left untouched.
func RefreshServiceManifests(sourceRoot, targetRoot string) ([]string, error) {
	sourceRoot, targetRoot, err := refreshRoots(sourceRoot, targetRoot)
	if err != nil {
		return nil, err
	}
	candidates, err := serviceManifestRefreshCandidates(sourceRoot, targetRoot)
	if err != nil {
		return nil, err
	}
	for _, relative := range candidates {
		if err := atomicCopyFile(
			filepath.Join(sourceRoot, filepath.FromSlash(relative)),
			filepath.Join(targetRoot, filepath.FromSlash(relative)),
		); err != nil {
			return nil, fmt.Errorf("refresh service manifest %s: %w", relative, err)
		}
	}
	return candidates, nil
}

func serviceManifestRefreshCandidates(sourceRoot, targetRoot string) ([]string, error) {
	composed, err := composedServiceNames(targetRoot)
	if err != nil {
		return nil, err
	}
	candidates := make([]string, 0, len(composed))
	for service := range composed {
		relative := "services/" + service + "/" + resources.ServiceConfigurationName
		// The pinned source only generates manifests for services it defines; a
		// symlinked or otherwise unsafe target path is never rewritten.
		if !safeModulePath(sourceRoot, relative, true) || !safeModulePath(targetRoot, relative, false) {
			continue
		}
		sourceContent, srcErr := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(relative)))
		if srcErr != nil {
			return nil, fmt.Errorf("read source service manifest %s: %w", relative, srcErr)
		}
		// The pinned source is the authority on whether this manifest is a
		// generated projection. A source manifest that does not declare itself
		// generated is not a file the sync owns; copying it over the consumer
		// would strip the consumer's marker and freeze the service out of every
		// future refresh, so it is skipped.
		if !carriesGeneratedMarker(sourceContent) {
			continue
		}
		targetContent, readErr := os.ReadFile(filepath.Join(targetRoot, filepath.FromSlash(relative)))
		if os.IsNotExist(readErr) {
			// The consumer composes this source-defined service but has no manifest
			// yet; there is no consumer-owned content to lose, so materialize the
			// pinned source's copy.
			candidates = append(candidates, relative)
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read target service manifest %s: %w", relative, readErr)
		}
		// A consumer manifest without the generated marker is hand-authored
		// product content that the sync must never overwrite — the same ownership
		// boundary `codefly update` honors.
		if !carriesGeneratedMarker(targetContent) {
			continue
		}
		if sha256Bytes(targetContent) != sha256Bytes(sourceContent) {
			candidates = append(candidates, relative)
		}
	}
	sort.Strings(candidates)
	return candidates, nil
}

// PlanModuleInterfaceRefresh reports whether a refresh would rewrite the
// module's generated interface block from the pinned source, without mutating.
func PlanModuleInterfaceRefresh(sourceRoot, targetRoot string) (bool, error) {
	sourceRoot, targetRoot, err := refreshRoots(sourceRoot, targetRoot)
	if err != nil {
		return false, err
	}
	refreshed, current, err := plannedModuleInterface(sourceRoot, targetRoot)
	if err != nil {
		return false, err
	}
	return refreshed != nil && !bytes.Equal(refreshed, current), nil
}

// RefreshModuleInterface rewrites the interface block of the consumer's
// generated module.codefly.yaml from the pinned source's copy, reporting whether
// it changed. The module manifest is generated from
// deployment/topology.bindings.codefly.yaml — a base-owned file the sync
// updates — yet it is also where the consumer keeps product-owned content (its
// own name, description, and added services), so it cannot be copied wholesale
// like a service manifest. Refreshing only the generated interface keeps the
// module's declared contract from contradicting the bindings it was rendered
// from, which is what fails the base's own composition gate in the consumer.
// Every other byte of the manifest — consumer-owned keys, comments, formatting —
// is left exactly as written. Only a manifest that still declares itself
// generated on both sides is refreshed.
func RefreshModuleInterface(sourceRoot, targetRoot string) (bool, error) {
	sourceRoot, targetRoot, err := refreshRoots(sourceRoot, targetRoot)
	if err != nil {
		return false, err
	}
	refreshed, current, err := plannedModuleInterface(sourceRoot, targetRoot)
	if err != nil {
		return false, err
	}
	if refreshed == nil || bytes.Equal(refreshed, current) {
		return false, nil
	}
	target := filepath.Join(targetRoot, resources.ModuleConfigurationName)
	info, err := os.Stat(target)
	if err != nil {
		return false, fmt.Errorf("stat target module manifest: %w", err)
	}
	if err := atomicWriteFile(target, refreshed, info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("refresh module interface in %s: %w", resources.ModuleConfigurationName, err)
	}
	// An interface endpoint must resolve to a non-private endpoint on a composed
	// service, so a service whose manifest the consumer took over as hand-authored
	// content — and which the service refresh therefore left behind — can leave
	// the module exposing an endpoint it no longer declares. Writing that would
	// make the module unloadable for every later command, so the refresh is undone
	// and the operator is told what to reconcile.
	if _, err := resources.LoadModuleFromDir(context.Background(), targetRoot); err != nil {
		if restoreErr := atomicWriteFile(target, current, info.Mode().Perm()); restoreErr != nil {
			return false, errors.Join(err, fmt.Errorf("restore %s: %w", resources.ModuleConfigurationName, restoreErr))
		}
		return false, fmt.Errorf("the interface the pinned source declares does not validate against this module's services, so %s was left as it was: %w", resources.ModuleConfigurationName, err)
	}
	return true, nil
}

// plannedModuleInterface returns the target module manifest with its interface
// block reconciled against the pinned source, alongside the manifest as it is on
// disk. A nil reconciliation means the manifest is outside what the sync owns.
func plannedModuleInterface(sourceRoot, targetRoot string) ([]byte, []byte, error) {
	relative := resources.ModuleConfigurationName
	if !safeModulePath(sourceRoot, relative, true) || !safeModulePath(targetRoot, relative, true) {
		return nil, nil, nil
	}
	sourceContent, err := os.ReadFile(filepath.Join(sourceRoot, relative))
	if err != nil {
		return nil, nil, fmt.Errorf("read source module manifest: %w", err)
	}
	targetContent, err := os.ReadFile(filepath.Join(targetRoot, relative))
	if err != nil {
		return nil, nil, fmt.Errorf("read target module manifest: %w", err)
	}
	// The generated marker is the ownership signal on both sides: a source that
	// does not declare its manifest generated is not rendering an interface the
	// sync owns, and a consumer manifest without the marker is hand-authored
	// product content the sync must never rewrite.
	if !carriesGeneratedMarker(sourceContent) || !carriesGeneratedMarker(targetContent) {
		return nil, nil, nil
	}
	refreshed, err := rewriteModuleInterface(sourceContent, targetContent)
	if err != nil {
		return nil, nil, fmt.Errorf("reconcile the interface of %s: %w", relative, err)
	}
	return refreshed, targetContent, nil
}

// rewriteModuleInterface splices the source manifest's interface block into the
// target manifest's text, editing only the lines that block occupies. A source
// without an interface block drops the target's — the module exposes nothing at
// that version — and a target without one gains it at the position the source's
// key order implies.
func rewriteModuleInterface(sourceContent, targetContent []byte) ([]byte, error) {
	sourceDocument, err := documentMapping(sourceContent)
	if err != nil {
		return nil, fmt.Errorf("parse source manifest: %w", err)
	}
	targetDocument, err := documentMapping(targetContent)
	if err != nil {
		return nil, fmt.Errorf("parse target manifest: %w", err)
	}
	sourceLines, _ := yamlLines(sourceContent)
	targetLines, targetTerminated := yamlLines(targetContent)

	var block []string
	if start, end, ok := yamlKeyBlock(sourceDocument, moduleInterfaceKey); ok {
		block = sourceLines[start:end]
	}
	start, end, ok := yamlKeyBlock(targetDocument, moduleInterfaceKey)
	switch {
	case ok:
	case len(block) == 0:
		return targetContent, nil
	default:
		start = moduleInterfaceInsertion(sourceDocument, targetDocument, len(targetLines))
		end = start
	}
	rewritten := slices.Concat(targetLines[:start], block, targetLines[end:])
	return joinYAMLLines(rewritten, targetTerminated), nil
}

// moduleInterfaceInsertion picks where an absent interface block belongs: before
// the first target key the source orders after its own interface, so the
// refreshed manifest keeps the generator's layout instead of appending a block
// the next regeneration would move.
func moduleInterfaceInsertion(sourceDocument, targetDocument *yaml.Node, targetLines int) int {
	sourceKeys := yamlKeys(sourceDocument)
	after := make(map[string]bool, len(sourceKeys))
	for index := len(sourceKeys) - 1; index >= 0 && sourceKeys[index] != moduleInterfaceKey; index-- {
		after[sourceKeys[index]] = true
	}
	for _, key := range yamlKeys(targetDocument) {
		if !after[key] {
			continue
		}
		if start, _, ok := yamlKeyBlock(targetDocument, key); ok {
			return start
		}
	}
	return targetLines
}

// documentMapping returns the root mapping of a single-document YAML file.
func documentMapping(content []byte) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a YAML mapping")
	}
	return document.Content[0], nil
}

func yamlKeys(mapping *yaml.Node) []string {
	keys := make([]string, 0, len(mapping.Content)/2)
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		keys = append(keys, mapping.Content[index].Value)
	}
	return keys
}

// yamlKeyBlock returns the half-open range of 0-based line indexes that key and
// its value occupy in a mapping. The value's extent is the deepest line any of
// its descendants sits on, so a blank line or a comment introducing the next key
// stays outside the block.
func yamlKeyBlock(mapping *yaml.Node, key string) (int, int, bool) {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != key {
			continue
		}
		return mapping.Content[index].Line - 1,
			max(deepestLine(mapping.Content[index+1]), mapping.Content[index].Line),
			true
	}
	return 0, 0, false
}

func deepestLine(node *yaml.Node) int {
	line := node.Line
	for _, child := range node.Content {
		line = max(line, deepestLine(child))
	}
	return line
}

// yamlLines splits a document into lines, reporting whether it was newline
// terminated so a rewrite can restore the file's exact framing.
func yamlLines(content []byte) ([]string, bool) {
	text := string(content)
	if terminated := strings.HasSuffix(text, "\n"); terminated {
		return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), true
	}
	return strings.Split(text, "\n"), false
}

func joinYAMLLines(lines []string, terminated bool) []byte {
	joined := strings.Join(lines, "\n")
	if terminated {
		joined += "\n"
	}
	return []byte(joined)
}

// GeneratedFileMarker reports the source named by a leading
// "Code generated from <source>. DO NOT EDIT." marker and whether any generated
// marker is present in the file's leading comment block. It is the single
// ownership signal used to decide whether a file is machine-generated — and thus
// owned by its source — rather than hand-authored.
func GeneratedFileMarker(content []byte) (source string, generated bool) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return "", false
		}
		if !generatedFileMarker.MatchString(line) {
			continue
		}
		if m := generatedFileSource.FindStringSubmatch(line); m != nil {
			return m[1], true
		}
		return "", true
	}
	return "", false
}

func carriesGeneratedMarker(content []byte) bool {
	_, generated := GeneratedFileMarker(content)
	return generated
}

func refreshRoots(sourceRoot, targetRoot string) (string, string, error) {
	source, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve source module: %w", err)
	}
	target, err := filepath.Abs(targetRoot)
	if err != nil {
		return "", "", fmt.Errorf("resolve target module: %w", err)
	}
	if source == target {
		return "", "", fmt.Errorf("source and target module must differ")
	}
	return source, target, nil
}

// ValidateServiceCodeSource proves that a source checkout is the exact origin
// of the target manifest's owned service code. Generated scaffold metadata may
// legitimately differ, so provenance is checked at the repair ownership boundary.
func ValidateServiceCodeSource(sourceRoot, targetRoot string) error {
	sourceManifest, err := readBaseManifest(filepath.Join(sourceRoot, baseManifestRelativePath))
	if err != nil {
		return fmt.Errorf("read source base manifest: %w", err)
	}
	targetManifest, err := readBaseManifest(filepath.Join(targetRoot, baseManifestRelativePath))
	if err != nil {
		return fmt.Errorf("read target base manifest: %w", err)
	}
	allow, err := loadBaseIntegrityAllow(filepath.Join(targetRoot, "tools", "base-integrity-allow.json"))
	if err != nil {
		return fmt.Errorf("read target base integrity policy: %w", err)
	}
	composed, err := composedServiceNames(targetRoot)
	if err != nil {
		return err
	}

	sourceCode := serviceCodeManifest(sourceManifest, composed, allow.Divergences)
	targetCode := serviceCodeManifest(targetManifest, composed, allow.Divergences)
	for _, relative := range sortedManifestPaths(baseManifest{Files: sourceCode}) {
		if !safeModulePath(sourceRoot, relative, true) {
			return fmt.Errorf("pinned source contains unsafe or missing service code path %s", relative)
		}
		digest, digestErr := sha256File(filepath.Join(sourceRoot, filepath.FromSlash(relative)))
		if digestErr != nil || digest != sourceCode[relative] {
			return fmt.Errorf("pinned source service code %s does not match its manifest", relative)
		}
		if targetCode[relative] != sourceCode[relative] {
			return fmt.Errorf("pinned source does not match target-owned service code at %s", relative)
		}
	}
	for _, relative := range sortedManifestPaths(baseManifest{Files: targetCode}) {
		if sourceCode[relative] != targetCode[relative] {
			return fmt.Errorf("pinned source does not match target-owned service code at %s", relative)
		}
		if !safeModulePath(targetRoot, relative, false) {
			return fmt.Errorf("target manifest contains unsafe service code path %s", relative)
		}
	}
	return nil
}

// ValidateInventoryOnlyScaffold guards the manifest-less scaffold that
// `add module --agent` pins. A conforming inventory-only agent generates
// consumer inventory and no base code, leaving the first sync to populate the
// whole base. This rejects a scaffold that instead materialized base-owned code
// diverging from the pinned source without recording a base manifest: that
// scaffold is inconsistent, because its first sync plans the missing manifest as
// an empty base and the divergent file then surfaces as an unresolvable
// collision with no manifest to explain it. Catching it here, at pin time, keeps
// a broken module out of the workspace and keeps the sync-time "manifest was
// lost" diagnosis accurate. No base code, or byte-identical base code, passes;
// only a divergent copy is a contract violation.
func ValidateInventoryOnlyScaffold(sourceRoot, targetRoot string) error {
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return fmt.Errorf("resolve source module: %w", err)
	}
	targetRoot, err = filepath.Abs(targetRoot)
	if err != nil {
		return fmt.Errorf("resolve target module: %w", err)
	}
	sourceManifest, err := readBaseManifest(filepath.Join(sourceRoot, baseManifestRelativePath))
	if err != nil {
		return fmt.Errorf("read source base manifest: %w", err)
	}
	composed, err := composedServiceNames(targetRoot)
	if err != nil {
		return err
	}
	for _, relative := range sortedManifestPaths(sourceManifest) {
		if service := serviceOf(relative); service != "" && len(composed) > 0 && !composed[service] {
			continue
		}
		// Mirror PlanBaseSync's collision detection: re-hash the actual source
		// file and skip any path whose source is unsafe, missing, or stale against
		// its own manifest. Those are upstream problems the sync surfaces as an
		// invalid source, not scaffold inconsistencies, so pin must not attribute
		// them to the agent by comparing the target to a stale manifest digest.
		if !safeModulePath(sourceRoot, relative, true) {
			continue
		}
		sourceDigest, srcErr := sha256File(filepath.Join(sourceRoot, filepath.FromSlash(relative)))
		if srcErr != nil || sourceDigest != sourceManifest.Files[relative] {
			continue
		}
		if !safeModulePath(targetRoot, relative, false) {
			continue
		}
		targetDigest, digestErr := sha256File(filepath.Join(targetRoot, filepath.FromSlash(relative)))
		if os.IsNotExist(digestErr) {
			continue
		}
		if digestErr != nil {
			return fmt.Errorf("hash scaffold base path %s: %w", relative, digestErr)
		}
		if targetDigest != sourceDigest {
			return fmt.Errorf("module agent produced base-owned file %s without a base manifest; the scaffold is inconsistent and cannot be pinned", relative)
		}
	}
	return nil
}

func serviceCodeManifest(manifest baseManifest, composed map[string]bool, allowed map[string]string) map[string]string {
	files := make(map[string]string)
	for relative, digest := range manifest.Files {
		if !selectedServiceCodePath(relative, composed) {
			continue
		}
		if _, ok := allowed[relative]; ok {
			continue
		}
		files[relative] = digest
	}
	return files
}

func selectedServiceCodePath(relative string, composed map[string]bool) bool {
	if !isServiceCodePath(relative) {
		return false
	}
	return len(composed) == 0 || composed[serviceOf(relative)]
}

func isServiceCodePath(relative string) bool {
	parts := strings.Split(relative, "/")
	return len(parts) >= 4 && parts[0] == "services" && parts[1] != "" && parts[2] == "code" && parts[3] != ""
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

// readTargetBaseManifest reads the target's base manifest, reporting whether it
// was actually present. A missing manifest is synthesized as an empty base so a
// never-populated scaffold plans its whole upstream base as new files, but the
// caller must know it was synthesized: a *composed* module whose manifest was
// lost must not be treated as a fresh scaffold, because that erases the
// base/overlay provenance the plan relies on to fail closed.
func readTargetBaseManifest(path string) (baseManifest, bool, error) {
	manifest, err := readBaseManifest(path)
	if os.IsNotExist(err) {
		return baseManifest{Files: map[string]string{}}, false, nil
	}
	return manifest, err == nil, err
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

func prepareAtomicCopy(source, target string) (string, func(), error) {
	payload, err := os.ReadFile(source)
	if err != nil {
		return "", func() {}, err
	}
	info, err := os.Stat(source)
	if err != nil {
		return "", func() {}, err
	}
	return prepareAtomicWrite(target, payload, info.Mode().Perm())
}

func prepareAtomicWrite(target string, payload []byte, mode os.FileMode) (string, func(), error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", func() {}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(target), ".codefly-base-sync-*")
	if err != nil {
		return "", func() {}, err
	}
	temporaryPath := temporary.Name()
	cleanup := func() { _ = os.Remove(temporaryPath) }
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		cleanup()
		return "", func() {}, err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return temporaryPath, cleanup, nil
}

func atomicCopyFile(source, target string) error {
	temporaryPath, cleanup, err := prepareAtomicCopy(source, target)
	if err != nil {
		return err
	}
	defer cleanup()
	return os.Rename(temporaryPath, target)
}

func atomicWriteFile(target string, payload []byte, mode os.FileMode) error {
	temporaryPath, cleanup, err := prepareAtomicWrite(target, payload, mode)
	if err != nil {
		return err
	}
	defer cleanup()
	return os.Rename(temporaryPath, target)
}

func atomicCreateFile(source, target string) error {
	temporaryPath, cleanup, err := prepareAtomicCopy(source, target)
	if err != nil {
		return err
	}
	defer cleanup()
	return os.Link(temporaryPath, target)
}

// modulePathStatus separates a structurally unsafe manifest path from one that
// is safe but simply absent from a checkout. The two are indistinguishable to a
// boolean predicate yet demand different operator remedies.
type modulePathStatus int

const (
	modulePathSafe modulePathStatus = iota
	modulePathMissing
	modulePathUnsafe
)

// inspectModulePath walks a canonical manifest path under root, rejecting path
// traversal and symlink components. A canonical manifest is data from outside
// the consumer trust boundary; it must never be able to read, replace, or
// remove a path outside the selected module. requireRegularFile additionally
// demands that an existing final component be a regular file.
func inspectModulePath(root, relative string, requireRegularFile bool) modulePathStatus {
	clean, ok := canonicalModulePath(relative)
	if !ok {
		return modulePathUnsafe
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return modulePathMissing
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return modulePathUnsafe
		}
		if index < len(parts)-1 && !info.IsDir() {
			return modulePathUnsafe
		}
		if index == len(parts)-1 && requireRegularFile && !info.Mode().IsRegular() {
			return modulePathUnsafe
		}
	}
	return modulePathSafe
}

// safeModulePath reports whether a manifest path may be operated on. A missing
// path is safe only when the caller does not require the file to already exist.
func safeModulePath(root, relative string, requireExistingFile bool) bool {
	switch inspectModulePath(root, relative, requireExistingFile) {
	case modulePathSafe:
		return true
	case modulePathMissing:
		return !requireExistingFile
	default:
		return false
	}
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
