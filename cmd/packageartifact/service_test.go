package packageartifact

import "testing"

func TestParsePackageTargetsIsDeterministic(t *testing.T) {
	targets, err := parsePackageTargets([]string{"linux/amd64", "darwin/arm64", "linux/amd64"})
	if err != nil {
		t.Fatalf("parsePackageTargets: %v", err)
	}
	if len(targets) != 2 || targets[0].GetOs() != "darwin" || targets[1].GetOs() != "linux" {
		t.Fatalf("targets = %v, want sorted unique targets", targets)
	}
}

func TestParsePackageTargetsRejectsNativeCommandsOrMalformedValues(t *testing.T) {
	for _, value := range []string{"linux", "go build", "linux/amd64/extra"} {
		if _, err := parsePackageTargets([]string{value}); err == nil {
			t.Fatalf("parsePackageTargets(%q) succeeded", value)
		}
	}
}
