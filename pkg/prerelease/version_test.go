package prerelease

import "testing"

// TestClassifyDetectsTheShapeNotTheString is the detection contract: a semver
// prerelease component and a Go pseudo-version are the same defect spelled
// differently, and nothing that merely contains "dev" or merely fails to be an
// exact version is one.
func TestClassifyDetectsTheShapeNotTheString(t *testing.T) {
	prereleases := map[string]Kind{
		// The three pins that reached released module tags.
		"0.1.48-dev.e87db5e08865":  KindPrereleaseTag,
		"0.0.159-dev.1ed5001fd8b4": KindPrereleaseTag,
		"0.0.139-dev.9785d12f3700": KindPrereleaseTag,
		// Spelled without "dev" at all.
		"0.2.0-rc.1":     KindPrereleaseTag,
		"1.0.0-alpha":    KindPrereleaseTag,
		"1.0.0-beta.2":   KindPrereleaseTag,
		"2.0.0-SNAPSHOT": KindPrereleaseTag,
		// Go pseudo-versions, both spellings.
		"v0.0.0-20260930123456-abcdef123456":    KindPseudoVersion,
		"v0.5.11-0.20260927230309-0c1b5db823d5": KindPseudoVersion,
		"0.0.0-20260930123456-abcdef123456":     KindPseudoVersion,
	}
	for version, want := range prereleases {
		kind, got := Classify(version)
		if !got {
			t.Errorf("Classify(%q) did not detect a prerelease", version)
			continue
		}
		if kind != want {
			t.Errorf("Classify(%q) = %q, want %q", version, kind, want)
		}
	}

	// Each of these is written in real committed config and names no unreleased
	// build. Flagging any of them would make the gate noise.
	for _, version := range []string{
		"", "0.0.0", "0.0.45", "v0.1.50", "0.1",
		"latest",                       // the agent sentinel
		"^0.0.1", "~1.2", ">=1.0", "*", // library dependency ranges
		"1.2.3+build.5", // build metadata is not a prerelease
		"main", "develop", "stable",
	} {
		if _, got := Classify(version); got {
			t.Errorf("Classify(%q) reported a prerelease", version)
		}
	}
}

// TestLabelledRequiresAnIssueReference pins what makes an agent-overrides comment
// a label. The condition for removal is prose no gate can check; the issue
// reference is the half that can be, and the half that tells the next reader
// where to look.
func TestLabelledRequiresAnIssueReference(t *testing.T) {
	labelled := []string{
		"# DEV PIN, labelled: codefly-dev/service-go#117 — the `go` agent ran `go mod download` inside the container.",
		"DEV PIN: see #146, removed before tagging",
		"stands in for obin-ai/module-runtime#188",
	}
	for _, comment := range labelled {
		if !Labelled(comment) {
			t.Errorf("Labelled(%q) = false, want true", comment)
		}
	}
	unlabelled := []string{
		"",
		"# dev pin",
		"# temporary, will fix later",
		"# published from service-go main bd71dd90cc10 as a prerelease",
	}
	for _, comment := range unlabelled {
		if Labelled(comment) {
			t.Errorf("Labelled(%q) = true, want false", comment)
		}
	}
}
