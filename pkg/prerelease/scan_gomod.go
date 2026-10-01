package prerelease

import (
	"strings"

	"golang.org/x/mod/modfile"
)

// scanGoMod finds first-party prerelease requires. A pseudo-version here names a
// commit of a repository the fleet publishes itself, which could have been
// released instead; a third-party one is somebody else's cadence.
//
// Reported rather than refused unless Options.GoModules — see the package
// comment for why, and the measurement behind it.
//
// A go.mod that does not parse is left to the Go toolchain to complain about,
// for the same reason a malformed YAML document is.
func scanGoMod(file string, content []byte, options Options) ([]Finding, error) {
	parsed, err := modfile.Parse(file, content, nil)
	if err != nil {
		return nil, nil
	}
	var findings []Finding
	for _, require := range parsed.Require {
		if !isFirstParty(require.Mod.Path, options.firstParty()) {
			continue
		}
		kind, prerelease := Classify(require.Mod.Version)
		if !prerelease {
			continue
		}
		finding := Finding{
			File:     file,
			Key:      "require " + require.Mod.Path,
			Version:  require.Mod.Version,
			Kind:     kind,
			Carrier:  CarrierGoModule,
			Blocking: options.GoModules,
		}
		if require.Syntax != nil {
			finding.Line = require.Syntax.Start.Line
		}
		if require.Indirect {
			finding.Key += " // indirect"
		}
		if finding.Blocking {
			finding.Why = "--go-modules: a first-party pseudo-version pins an unreleased commit of a repository this fleet publishes"
		} else {
			finding.Why = "reported only: first-party pseudo-versions are routine in this fleet and an indirect one cannot be moved from here — pass --go-modules to refuse them"
		}
		finding.Remedy = []string{
			"release the first-party module and `go get` the released version",
			"an `// indirect` entry moves only when the dependency that requires it releases — bump that dependency, do not hand-edit the require",
		}
		findings = append(findings, finding)
	}
	return findings, nil
}

func isFirstParty(modulePath string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(modulePath, prefix) {
			return true
		}
	}
	return false
}
