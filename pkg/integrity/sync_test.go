package integrity

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codefly-dev/core/resources"
)

func TestBaseSyncPreservesOverlaysAndAppliesOnlyOwnedFiles(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "stable.txt"), "stable")
	writeTestFile(t, filepath.Join(source, "update.txt"), "new")
	writeTestFile(t, filepath.Join(source, "create.txt"), "created")
	writeTestFile(t, filepath.Join(source, "module.codefly.yaml"), "canonical descriptor")
	writeTestFile(t, filepath.Join(target, "stable.txt"), "stable")
	writeTestFile(t, filepath.Join(target, "update.txt"), "old")
	writeTestFile(t, filepath.Join(target, "remove.txt"), "retired")
	writeTestFile(t, filepath.Join(target, "module.codefly.yaml"), targetModuleYAML)
	writeTestFile(t, filepath.Join(target, "product", "plugin.ts"), "warden overlay")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"module.codefly.yaml": "consumer composition root",
		"requiredAdditions":   map[string]string{"product/plugin.ts": "product frontend plugin"},
	})
	writeManifest(t, source, "stable.txt", "update.txt", "create.txt", "module.codefly.yaml")
	writeManifest(t, target, "stable.txt", "update.txt", "remove.txt", "module.codefly.yaml")

	// module.codefly.yaml is an allow-listed divergence whose recorded base
	// differs from the pinned source: a masked upstream change that now blocks by
	// default. Keeping the local composition root is the intent here, so the
	// operator re-affirms with --keep-local-divergences; overlays and owned files
	// must still apply exactly as before.
	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.AllowedUpstreamChanged, []string{"module.codefly.yaml"}) {
		t.Fatalf("AllowedUpstreamChanged = %#v, want [module.codefly.yaml]", plan.AllowedUpstreamChanged)
	}
	if err := plan.Applicable(); err == nil {
		t.Fatal("masked allow-listed upstream change must block by default")
	}
	plan, err = PlanBaseSyncWithResolutions(source, target, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Applicable(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Create, []string{"create.txt"}) ||
		!reflect.DeepEqual(plan.Update, []string{"update.txt"}) ||
		!reflect.DeepEqual(plan.Remove, []string{"remove.txt"}) ||
		!reflect.DeepEqual(plan.Allowed, []string{"module.codefly.yaml"}) {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if _, err := ApplyBaseSync(source, target, true); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(target, "update.txt"), "new")
	assertFileContents(t, filepath.Join(target, "create.txt"), "created")
	assertFileContents(t, filepath.Join(target, "product", "plugin.ts"), "warden overlay")
	assertFileContents(t, filepath.Join(target, "module.codefly.yaml"), targetModuleYAML)
	if _, err := os.Stat(filepath.Join(target, "remove.txt")); !os.IsNotExist(err) {
		t.Fatalf("retired base file still exists: %v", err)
	}
}

func TestBaseSyncFlagsAllowListedUpstreamChange(t *testing.T) {
	source, target := syncFixture(t)
	// Both files are allow-listed divergences (kept local unconditionally).
	// overlay.txt's pinned upstream digest moved since the recorded base;
	// stable.txt's did not. Only the former is a masked upstream change.
	writeTestFile(t, filepath.Join(source, "overlay.txt"), "upstream v2")
	writeTestFile(t, filepath.Join(source, "stable.txt"), "shared")
	writeTestFile(t, filepath.Join(target, "overlay.txt"), "upstream v1")
	writeTestFile(t, filepath.Join(target, "stable.txt"), "shared")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"overlay.txt":       "product overlay",
		"stable.txt":        "product overlay",
		"requiredAdditions": map[string]string{},
	})
	writeManifest(t, source, "overlay.txt", "stable.txt")
	writeManifest(t, target, "overlay.txt", "stable.txt")

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	// Both remain allow-listed (kept local); only overlay.txt is flagged as
	// masking an upstream change, so the drop can never be silent.
	if !reflect.DeepEqual(plan.AllowedUpstreamChanged, []string{"overlay.txt"}) {
		t.Fatalf("AllowedUpstreamChanged = %#v, want [overlay.txt]", plan.AllowedUpstreamChanged)
	}
	// The "still kept local" contract must hold for both files regardless of the
	// masking flag — surfacing the change must not eject either from Allowed.
	if !reflect.DeepEqual(plan.Allowed, []string{"overlay.txt", "stable.txt"}) {
		t.Fatalf("Allowed = %#v, want [overlay.txt stable.txt]", plan.Allowed)
	}
	// A masked upstream change blocks by default: an unattended --apply must not
	// be able to drop it with a zero exit.
	if err := plan.Applicable(); err == nil {
		t.Fatal("masked allow-listed upstream change must block apply by default")
	}
	if !plan.MaskedUpstreamBlocked() {
		t.Fatal("plan should report it is blocked solely on the masked upstream change")
	}
	// --keep-local-divergences is the explicit re-affirmation that unblocks it,
	// keeping the local version while the recorded base advances on apply.
	reaffirmed, err := PlanBaseSyncWithResolutions(source, target, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reaffirmed.AllowedUpstreamChanged, []string{"overlay.txt"}) {
		t.Fatalf("re-affirmed AllowedUpstreamChanged = %#v, want [overlay.txt]", reaffirmed.AllowedUpstreamChanged)
	}
	if err := reaffirmed.Applicable(); err != nil {
		t.Fatalf("--keep-local-divergences must let the intentional divergence apply: %v", err)
	}
	if reaffirmed.MaskedUpstreamBlocked() {
		t.Fatal("re-affirmed plan must not report itself blocked")
	}
}

// A masked upstream change means the tree cannot reach the target tag: it
// blocks (and is counted as withheld) by default, and even a re-affirmed apply
// that advances the recorded base keeps the local version, so ReachesTarget
// stays false. A clean sync with no masking reaches the tag.
func TestReachesTargetAndWithheldPathsTrackMaskedUpstream(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "overlay.txt"), "upstream v2")
	writeTestFile(t, filepath.Join(target, "overlay.txt"), "upstream v1")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"overlay.txt":       "product overlay",
		"requiredAdditions": map[string]string{},
	})
	writeManifest(t, source, "overlay.txt")
	writeManifest(t, target, "overlay.txt")

	blocked, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.WithheldPaths() != 1 {
		t.Fatalf("WithheldPaths = %d, want 1", blocked.WithheldPaths())
	}
	if blocked.ReachesTarget() {
		t.Fatal("a masked upstream change must not report the tree reaches the target")
	}

	reaffirmed, err := PlanBaseSyncWithResolutions(source, target, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	// Re-affirmation unblocks the apply (nothing withheld) but the local version
	// is still kept, so the tree does not reach the tag byte-for-byte.
	if reaffirmed.WithheldPaths() != 0 {
		t.Fatalf("re-affirmed WithheldPaths = %d, want 0", reaffirmed.WithheldPaths())
	}
	if reaffirmed.ReachesTarget() {
		t.Fatal("keeping a masked divergence local must still not reach the target tag")
	}

	writeTestFile(t, filepath.Join(target, "overlay.txt"), "upstream v2")
	writeManifest(t, target, "overlay.txt")
	clean, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !clean.ReachesTarget() {
		t.Fatal("a sync with no masking or blockers must reach the target tag")
	}
}

func TestBaseSyncDoesNotFlagAllowListedFirstPopulate(t *testing.T) {
	source, target := syncFixture(t)
	// overlay.txt is allow-listed and present in the source manifest, but the
	// target has no recorded base for it (wasBase=false). There is no recorded
	// base to have moved from, so this is an initial state, not a masked upstream
	// change — it must not be flagged, or every first populate would false-alarm.
	writeTestFile(t, filepath.Join(source, "overlay.txt"), "upstream v1")
	writeTestFile(t, filepath.Join(target, "overlay.txt"), "local divergence")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"overlay.txt":       "product overlay",
		"requiredAdditions": map[string]string{},
	})
	writeManifest(t, source, "overlay.txt")
	writeManifest(t, target) // empty recorded base: no entry for overlay.txt

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.AllowedUpstreamChanged) != 0 {
		t.Fatalf("AllowedUpstreamChanged = %#v, want empty (no recorded base to move from)", plan.AllowedUpstreamChanged)
	}
	if !reflect.DeepEqual(plan.Allowed, []string{"overlay.txt"}) {
		t.Fatalf("Allowed = %#v, want [overlay.txt]", plan.Allowed)
	}
	if err := plan.Applicable(); err != nil {
		t.Fatalf("first-populate allow-listed path must not block: %v", err)
	}
}

func TestBaseSyncFlagsAllowListedUpstreamRemoval(t *testing.T) {
	source, target := syncFixture(t)
	// retired.txt was part of the recorded base and is allow-listed; the pinned
	// source dropped it (upstream removal) while the local copy still exists.
	// Keeping it local silently would mask the removal, so it must be surfaced
	// and block by default, exactly like a masked upstream modification.
	writeTestFile(t, filepath.Join(target, "retired.txt"), "kept local after upstream removed it")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"retired.txt":       "product overlay",
		"requiredAdditions": map[string]string{},
	})
	writeManifest(t, source)                // source no longer ships retired.txt
	writeManifest(t, target, "retired.txt") // but it is in the recorded base

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.AllowedUpstreamRemoved, []string{"retired.txt"}) {
		t.Fatalf("AllowedUpstreamRemoved = %#v, want [retired.txt]", plan.AllowedUpstreamRemoved)
	}
	if err := plan.Applicable(); err == nil {
		t.Fatal("masked allow-listed upstream removal must block apply by default")
	}
	// Re-affirmation unblocks the removal case too, while the local file is kept.
	reaffirmed, err := PlanBaseSyncWithResolutions(source, target, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reaffirmed.AllowedUpstreamRemoved, []string{"retired.txt"}) {
		t.Fatalf("re-affirmed AllowedUpstreamRemoved = %#v, want [retired.txt]", reaffirmed.AllowedUpstreamRemoved)
	}
	if err := reaffirmed.Applicable(); err != nil {
		t.Fatalf("--keep-local-divergences must let a masked removal apply: %v", err)
	}
	// A local file already gone is nothing to keep and must not be flagged.
	if err := os.Remove(filepath.Join(target, "retired.txt")); err != nil {
		t.Fatal(err)
	}
	gone, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone.AllowedUpstreamRemoved) != 0 {
		t.Fatalf("AllowedUpstreamRemoved = %#v, want empty when local file is already gone", gone.AllowedUpstreamRemoved)
	}
}

func TestBaseSyncTreatsMissingTargetManifestAsFirstPopulate(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "services", "kept", "code", "main.go"), "package main\n")
	writeManifest(t, source, "services/kept/code/main.go")

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Create, []string{"services/kept/code/main.go"}) {
		t.Fatalf("create = %v", plan.Create)
	}
	if _, err := ApplyBaseSync(source, target, false); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, filepath.Join(target, "services", "kept", "code", "main.go"), "package main\n")
	if _, err := readBaseManifest(filepath.Join(target, baseManifestRelativePath)); err != nil {
		t.Fatalf("target manifest was not committed: %v", err)
	}
}

// A composed module whose base manifest was lost must not be reinterpreted as a
// fresh scaffold: without the manifest the plan cannot tell a user-modified base
// file from an overlay collision, so it must fail closed rather than let
// --accept-upstream overwrite the customized content.
func TestBaseSyncFailsClosedWhenTargetManifestMissingButBaseFilesDiffer(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "services", "kept", "code", "main.go"), "package main // upstream\n")
	writeManifest(t, source, "services/kept/code/main.go")
	writeTestFile(t, filepath.Join(target, "services", "kept", "code", "main.go"), "package main // heavily customized\n")

	if _, err := PlanBaseSync(source, target); err == nil {
		t.Fatal("plan against a missing manifest with conflicting base files was not refused")
	}
	if _, err := ApplyBaseSyncWithResolutions(source, target, []string{"services/kept/code/main.go"}, false); err == nil {
		t.Fatal("--accept-upstream overwrote a base file whose provenance was erased")
	}
	assertFileContents(t, filepath.Join(target, "services", "kept", "code", "main.go"), "package main // heavily customized\n")
}

func TestValidateInventoryOnlyScaffold(t *testing.T) {
	// Divergent base code with no manifest is a misbehaving agent's output.
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "services", "kept", "code", "main.go"), "package upstream\n")
	writeManifest(t, source, "services/kept/code/main.go")
	writeTestFile(t, filepath.Join(target, "services", "kept", "code", "main.go"), "package customized\n")
	if err := ValidateInventoryOnlyScaffold(source, target); err == nil {
		t.Fatal("divergent base code without a manifest was accepted")
	}

	// The normal inventory-only scaffold has no base code and is accepted.
	emptySource, emptyTarget := syncFixture(t)
	writeTestFile(t, filepath.Join(emptySource, "services", "kept", "code", "main.go"), "package upstream\n")
	writeManifest(t, emptySource, "services/kept/code/main.go")
	if err := ValidateInventoryOnlyScaffold(emptySource, emptyTarget); err != nil {
		t.Fatalf("empty inventory-only scaffold rejected: %v", err)
	}

	// Byte-identical base code is harmless (first sync adopts it) and accepted.
	sameSource, sameTarget := syncFixture(t)
	writeTestFile(t, filepath.Join(sameSource, "services", "kept", "code", "main.go"), "package upstream\n")
	writeManifest(t, sameSource, "services/kept/code/main.go")
	writeTestFile(t, filepath.Join(sameTarget, "services", "kept", "code", "main.go"), "package upstream\n")
	if err := ValidateInventoryOnlyScaffold(sameSource, sameTarget); err != nil {
		t.Fatalf("identical base code rejected: %v", err)
	}

	// A stale source manifest (its recorded digest does not match the actual
	// source file) is an upstream problem the sync surfaces as an invalid source,
	// not a scaffold inconsistency. Pin must re-hash the real source rather than
	// trust the stale digest, so it must not blame the agent here.
	staleSource, staleTarget := syncFixture(t)
	writeTestFile(t, filepath.Join(staleSource, "services", "kept", "code", "main.go"), "package upstream\n")
	writeTestJSON(t, filepath.Join(staleSource, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"services/kept/code/main.go": digestOf(t, "package stale\n")},
	})
	writeTestFile(t, filepath.Join(staleTarget, "services", "kept", "code", "main.go"), "package customized\n")
	if err := ValidateInventoryOnlyScaffold(staleSource, staleTarget); err != nil {
		t.Fatalf("stale source manifest misattributed to the scaffold: %v", err)
	}
}

func TestBaseSyncRefusesModifiedBaseAndOverlayCollisionWithoutMutation(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "owned.txt"), "new owned")
	writeTestFile(t, filepath.Join(source, "collision.txt"), "new base")
	writeTestFile(t, filepath.Join(target, "owned.txt"), "product edited base")
	writeTestFile(t, filepath.Join(target, "collision.txt"), "product side addition")
	writeManifest(t, source, "owned.txt", "collision.txt")
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"owned.txt": digestOf(t, "old owned")},
	})

	plan, err := ApplyBaseSync(source, target, false)
	if err == nil {
		t.Fatal("unsafe base sync unexpectedly applied")
	}
	if !reflect.DeepEqual(plan.Modified, []string{"owned.txt"}) || !reflect.DeepEqual(plan.Collisions, []string{"collision.txt"}) {
		t.Fatalf("unexpected refusal plan: %#v", plan)
	}
	assertFileContents(t, filepath.Join(target, "owned.txt"), "product edited base")
	assertFileContents(t, filepath.Join(target, "collision.txt"), "product side addition")
}

func TestBaseSyncAppliesOnlyExplicitUpstreamResolutions(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "owned.txt"), "new owned")
	writeTestFile(t, filepath.Join(source, "collision.txt"), "new base")
	writeTestFile(t, filepath.Join(target, "owned.txt"), "product edited base")
	writeTestFile(t, filepath.Join(target, "collision.txt"), "product side addition")
	writeManifest(t, source, "owned.txt", "collision.txt")
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"owned.txt": digestOf(t, "old owned")},
	})

	plan, err := PlanBaseSyncWithResolutions(source, target, []string{"owned.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Applicable(); err == nil {
		t.Fatal("unreviewed overlay collision did not remain a blocker")
	}
	if !reflect.DeepEqual(plan.ResolveUpstream, []string{"owned.txt"}) ||
		!reflect.DeepEqual(plan.Collisions, []string{"collision.txt"}) {
		t.Fatalf("unexpected partial reconciliation plan: %#v", plan)
	}
	assertFileContents(t, filepath.Join(target, "owned.txt"), "product edited base")

	plan, err = ApplyBaseSyncWithResolutions(source, target, []string{"owned.txt", "collision.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.ResolveUpstream, []string{"collision.txt", "owned.txt"}) {
		t.Fatalf("resolved paths = %v", plan.ResolveUpstream)
	}
	assertFileContents(t, filepath.Join(target, "owned.txt"), "new owned")
	assertFileContents(t, filepath.Join(target, "collision.txt"), "new base")
}

func TestBaseSyncUpstreamResolutionIsResumable(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "owned.txt"), "new owned")
	writeTestFile(t, filepath.Join(target, "owned.txt"), "product edited base")
	writeManifest(t, source, "owned.txt")
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"owned.txt": digestOf(t, "old owned")},
	})

	if _, err := ApplyBaseSyncWithResolutions(source, target, []string{"owned.txt"}, false); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanBaseSyncWithResolutions(source, target, []string{"owned.txt"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.ReconciledUpstream, []string{"owned.txt"}) {
		t.Fatalf("already reconciled paths = %v", plan.ReconciledUpstream)
	}
	if err := plan.Applicable(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyBaseSyncWithResolutions(source, target, []string{"owned.txt"}, false); err != nil {
		t.Fatal(err)
	}
}

func TestBaseSyncRejectsUnsafeUpstreamResolutionSelections(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "clean-update.txt"), "new")
	writeTestFile(t, filepath.Join(source, "protected.txt"), "upstream")
	writeTestFile(t, filepath.Join(target, "clean-update.txt"), "old")
	writeTestFile(t, filepath.Join(target, "protected.txt"), "product")
	writeManifest(t, source, "clean-update.txt", "protected.txt")
	writeManifest(t, target, "clean-update.txt")
	writeTestJSON(t, filepath.Join(target, "tools", "base-integrity-allow.json"), map[string]any{
		"requiredAdditions": map[string]string{"protected.txt": "product survival contract"},
	})

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "clean update", path: "clean-update.txt"},
		{name: "unknown path", path: "missing.txt"},
		{name: "path traversal", path: "../outside.txt"},
		{name: "required product addition", path: "protected.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PlanBaseSyncWithResolutions(source, target, []string{test.path}, false); err == nil {
				t.Fatalf("unsafe upstream selection %q was accepted", test.path)
			}
		})
	}
}

func TestBaseSyncRejectsDuplicateUpstreamResolution(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "owned.txt"), "new")
	writeTestFile(t, filepath.Join(target, "owned.txt"), "modified")
	writeManifest(t, source, "owned.txt")
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"owned.txt": digestOf(t, "old")},
	})

	if _, err := PlanBaseSyncWithResolutions(source, target, []string{"owned.txt", "owned.txt"}, false); err == nil {
		t.Fatal("duplicate upstream selection was accepted")
	}
}

func TestBaseSyncDoesNotInstallServicesOutsideConsumerComposition(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "services", "kept", "base.txt"), "new kept")
	writeTestFile(t, filepath.Join(source, "services", "omitted", "base.txt"), "do not install")
	writeTestFile(t, filepath.Join(target, "services", "kept", "base.txt"), "old kept")
	writeManifest(t, source, "services/kept/base.txt", "services/omitted/base.txt")
	writeManifest(t, target, "services/kept/base.txt")

	plan, err := ApplyBaseSync(source, target, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Omitted, []string{"services/omitted/base.txt"}) {
		t.Fatalf("omitted = %v", plan.Omitted)
	}
	assertFileContents(t, filepath.Join(target, "services", "kept", "base.txt"), "new kept")
	if _, err := os.Stat(filepath.Join(target, "services", "omitted", "base.txt")); !os.IsNotExist(err) {
		t.Fatalf("omitted service was installed: %v", err)
	}
}

func TestRestoreMissingServiceCodePreservesExistingFilesAndOverlays(t *testing.T) {
	source, target := syncFixture(t)
	missingCode := "services/kept/code/main.go"
	modifiedCode := "services/kept/code/custom.go"
	missingNonCode := "docs/base.md"
	writeTestFile(t, filepath.Join(source, missingCode), "package main\n")
	writeTestFile(t, filepath.Join(source, modifiedCode), "package main\n\nconst canonical = true\n")
	writeTestFile(t, filepath.Join(source, missingNonCode), "canonical docs\n")
	writeManifest(t, source, missingCode, modifiedCode, missingNonCode)
	writeTestFile(t, filepath.Join(target, modifiedCode), "package main\n\nconst consumerEdit = true\n")
	writeTestFile(t, filepath.Join(target, "services/kept/overlays/local.yaml"), "consumer: true\n")
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{Files: map[string]string{
		missingCode:    digestOf(t, "package main\n"),
		modifiedCode:   digestOf(t, "package main\n\nconst canonical = true\n"),
		missingNonCode: digestOf(t, "canonical docs\n"),
	}})

	restored, err := RestoreMissingServiceCode(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, []string{missingCode}) {
		t.Fatalf("restored = %v, want %s", restored, missingCode)
	}
	assertFileContents(t, filepath.Join(target, missingCode), "package main\n")
	assertFileContents(t, filepath.Join(target, modifiedCode), "package main\n\nconst consumerEdit = true\n")
	assertFileContents(t, filepath.Join(target, "services/kept/overlays/local.yaml"), "consumer: true\n")
	if _, err := os.Stat(filepath.Join(target, missingNonCode)); !os.IsNotExist(err) {
		t.Fatalf("non-code base file was restored: %v", err)
	}
}

func TestRestoreMissingServiceCodeRejectsSourceAheadOfTargetManifest(t *testing.T) {
	source, target := syncFixture(t)
	newPath := "services/kept/code/new.go"
	writeTestFile(t, filepath.Join(source, newPath), "package kept\n")
	writeManifest(t, source, newPath)
	writeManifest(t, target)

	if restored, err := RestoreMissingServiceCode(source, target); err == nil {
		t.Fatalf("source from a newer manifest was accepted; restored = %v", restored)
	}
	if _, err := os.Stat(filepath.Join(target, newPath)); !os.IsNotExist(err) {
		t.Fatalf("new-version path was injected into the target: %v", err)
	}
}

func TestRestoreMissingServiceCodeRejectsChangedTargetOwnedBytes(t *testing.T) {
	source, target := syncFixture(t)
	codePath := "services/kept/code/main.go"
	writeTestFile(t, filepath.Join(source, codePath), "package kept\n\nconst version = 2\n")
	writeManifest(t, source, codePath)
	writeTestJSON(t, filepath.Join(target, "tools", "base-manifest.json"), baseManifest{Files: map[string]string{
		codePath: digestOf(t, "package kept\n\nconst version = 1\n"),
	}})

	if restored, err := RestoreMissingServiceCode(source, target); err == nil {
		t.Fatalf("new-version bytes were accepted for target-owned code; restored = %v", restored)
	}
	if _, err := os.Stat(filepath.Join(target, codePath)); !os.IsNotExist(err) {
		t.Fatalf("mismatched source bytes were restored: %v", err)
	}
}

func TestAtomicCreateFileNeverReplacesAnExistingPath(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.go")
	target := filepath.Join(root, "target.go")
	writeTestFile(t, source, "canonical")
	writeTestFile(t, target, "created concurrently")

	if err := atomicCreateFile(source, target); err == nil {
		t.Fatal("create-only copy replaced an existing target")
	}
	assertFileContents(t, target, "created concurrently")
}

func TestBaseSyncRejectsManifestTraversalBeforeMutation(t *testing.T) {
	source, target := syncFixture(t)
	writeTestJSON(t, filepath.Join(source, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"../outside.txt": digestOf(t, "outside")},
	})
	writeManifest(t, target)

	plan, err := ApplyBaseSync(source, target, false)
	if err == nil {
		t.Fatal("manifest traversal unexpectedly applied")
	}
	if !reflect.DeepEqual(plan.SourceInvalid, []InvalidSource{{Path: "../outside.txt", Reason: SourceUnsafePath}}) {
		t.Fatalf("source invalid = %#v", plan.SourceInvalid)
	}
}

func TestBaseSyncClassifiesMissingSourceFileAsUnreadable(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(target, "gone.txt"), "consumer content")
	// The source manifest lists gone.txt, but the file is absent from the
	// checkout: a broken/partial upstream, not a path that escapes the module.
	writeTestJSON(t, filepath.Join(source, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"gone.txt": digestOf(t, "upstream content")},
	})
	writeManifest(t, target, "gone.txt")

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Applicable(); err == nil {
		t.Fatal("missing source file did not block the sync")
	}
	want := []InvalidSource{{Path: "gone.txt", Reason: SourceUnreadable}}
	if !reflect.DeepEqual(plan.SourceInvalid, want) {
		t.Fatalf("source invalid = %#v, want %#v", plan.SourceInvalid, want)
	}
}

func TestBaseSyncClassifiesStaleUpstreamManifestAsDigestMismatch(t *testing.T) {
	source, target := syncFixture(t)
	writeTestFile(t, filepath.Join(source, "owned.txt"), "actual upstream content")
	writeTestFile(t, filepath.Join(target, "owned.txt"), "consumer content")
	staleDigest := digestOf(t, "content the manifest still claims")
	writeTestJSON(t, filepath.Join(source, "tools", "base-manifest.json"), baseManifest{
		Files: map[string]string{"owned.txt": staleDigest},
	})
	writeManifest(t, target, "owned.txt")

	plan, err := PlanBaseSync(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Applicable(); err == nil {
		t.Fatal("stale upstream manifest did not block the sync")
	}
	want := []InvalidSource{{
		Path:           "owned.txt",
		Reason:         SourceDigestMismatch,
		ManifestDigest: staleDigest,
		ActualDigest:   digestOf(t, "actual upstream content"),
	}}
	if !reflect.DeepEqual(plan.SourceInvalid, want) {
		t.Fatalf("source invalid = %#v, want %#v", plan.SourceInvalid, want)
	}
}

const generatedManifestHeader = "# Code generated from deployment/topology.bindings.codefly.yaml. DO NOT EDIT.\n"

// A per-service generated service.codefly.yaml is a consumer overlay, not a
// base-owned file, so a base sync leaves its agent pin stale. RefreshServiceManifests
// re-aligns each composed service's manifest with the pinned source, and only
// for services the source actually composes.
func TestRefreshServiceManifestsRewritesComposedGeneratedManifests(t *testing.T) {
	source, target := syncFixture(t)
	relative := "services/kept/" + resources.ServiceConfigurationName
	upstream := generatedManifestHeader + "name: kept\nagent:\n  name: vault\n  version: 0.0.25\n"
	writeTestFile(t, filepath.Join(source, filepath.FromSlash(relative)), upstream)
	writeTestFile(t, filepath.Join(target, filepath.FromSlash(relative)), generatedManifestHeader+"name: kept\nagent:\n  name: vault\n  version: 0.0.15\n")
	// A service the pinned source defines but the module does not compose must
	// never be created on the consumer side.
	writeTestFile(t, filepath.Join(source, "services", "extra", resources.ServiceConfigurationName), generatedManifestHeader+"name: extra\n")

	pending, err := PlanServiceManifestRefresh(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pending, []string{relative}) {
		t.Fatalf("refresh plan = %#v, want %#v", pending, []string{relative})
	}
	refreshed, err := RefreshServiceManifests(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(refreshed, []string{relative}) {
		t.Fatalf("refreshed = %#v, want %#v", refreshed, []string{relative})
	}
	assertFileContents(t, filepath.Join(target, filepath.FromSlash(relative)), upstream)
	if _, err := os.Stat(filepath.Join(target, "services", "extra", resources.ServiceConfigurationName)); !os.IsNotExist(err) {
		t.Fatalf("uncomposed service manifest was created: %v", err)
	}

	// A second refresh is a no-op once the consumer matches the pinned source.
	again, err := RefreshServiceManifests(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("refresh was not idempotent: %#v", again)
	}
}

// A source manifest that no longer declares itself generated (e.g. an upstream
// generator regression that dropped the marker) must not be copied over the
// consumer's generated manifest: doing so would strip the consumer file's own
// marker and freeze that service out of every future refresh.
func TestRefreshServiceManifestsSkipsUngeneratedSource(t *testing.T) {
	source, target := syncFixture(t)
	relative := "services/kept/" + resources.ServiceConfigurationName
	writeTestFile(t, filepath.Join(source, filepath.FromSlash(relative)),
		"name: kept\nagent:\n  name: vault\n  version: 0.0.25\n")
	generated := generatedManifestHeader + "name: kept\nagent:\n  name: vault\n  version: 0.0.15\n"
	writeTestFile(t, filepath.Join(target, filepath.FromSlash(relative)), generated)

	pending, err := PlanServiceManifestRefresh(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("markerless source was planned for refresh: %#v", pending)
	}
	refreshed, err := RefreshServiceManifests(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 0 {
		t.Fatalf("markerless source overwrote the consumer manifest: %#v", refreshed)
	}
	assertFileContents(t, filepath.Join(target, filepath.FromSlash(relative)), generated)
}

// A service manifest the consumer has taken over as hand-authored product
// content — no "Code generated ... DO NOT EDIT" marker — is off-limits to the
// sync, the same ownership boundary `codefly update` honors. Overwriting it
// would silently destroy consumer-owned endpoints, spec, and dependencies.
func TestRefreshServiceManifestsPreservesHandAuthoredManifest(t *testing.T) {
	source, target := syncFixture(t)
	relative := "services/kept/" + resources.ServiceConfigurationName
	writeTestFile(t, filepath.Join(source, filepath.FromSlash(relative)),
		generatedManifestHeader+"name: kept\nagent:\n  name: vault\n  version: 0.0.25\n")
	handAuthored := "name: kept\nagent:\n  name: vault\n  version: 0.0.15\nendpoints:\n  - name: custom\n"
	writeTestFile(t, filepath.Join(target, filepath.FromSlash(relative)), handAuthored)

	pending, err := PlanServiceManifestRefresh(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("hand-authored manifest was planned for refresh: %#v", pending)
	}
	refreshed, err := RefreshServiceManifests(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(refreshed) != 0 {
		t.Fatalf("hand-authored manifest was overwritten: %#v", refreshed)
	}
	assertFileContents(t, filepath.Join(target, filepath.FromSlash(relative)), handAuthored)
}

const targetModuleYAML = `kind: module
name: app
services:
  - name: kept
`

func syncFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	writeTestFile(t, filepath.Join(source, "module.codefly.yaml"), targetModuleYAML)
	writeTestFile(t, filepath.Join(target, "module.codefly.yaml"), targetModuleYAML)
	return source, target
}

func writeManifest(t *testing.T, root string, paths ...string) {
	t.Helper()
	manifest := baseManifest{Files: map[string]string{}}
	for _, relative := range paths {
		digest, err := sha256File(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		manifest.Files[relative] = digest
	}
	writeTestJSON(t, filepath.Join(root, "tools", "base-manifest.json"), manifest)
}

func digestOf(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "value")
	writeTestFile(t, path, contents)
	digest, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != expected {
		t.Fatalf("%s = %q, want %q", path, payload, expected)
	}
}
