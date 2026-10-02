package prerelease

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Report is the machine-readable form of a scan, for a CI job that wants to do
// something with the findings beyond failing.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	Scope         string    `json:"scope"`
	Status        string    `json:"status"`
	Dir           string    `json:"dir"`
	FilesScanned  int       `json:"files_scanned"`
	Tracked       bool      `json:"git_tracked_files"`
	FirstParty    []string  `json:"first_party_owners"`
	Blocking      []Finding `json:"blocking"`
	Allowed       []Finding `json:"allowed"`
}

// reportSchemaVersion is bumped when a consumer of the JSON would have to change.
const reportSchemaVersion = 1

// JSONReport renders the scan for a machine.
func (result *Result) JSONReport() Report {
	report := Report{
		SchemaVersion: reportSchemaVersion,
		Scope:         result.Scope(),
		Status:        "pass",
		Dir:           result.Dir,
		FilesScanned:  len(result.Files),
		Tracked:       result.Tracked,
		FirstParty:    result.FirstParty,
		Blocking:      emptyNotNull(result.Blocking()),
		Allowed:       emptyNotNull(result.Allowed()),
	}
	if len(report.Blocking) > 0 {
		report.Status = "fail"
	}
	return report
}

// emptyNotNull keeps an empty list rendering as [] rather than null. A CI job
// reading `.blocking | length` should not have to special-case the clean case,
// which is the one it sees most.
func emptyNotNull(findings []Finding) []Finding {
	if findings == nil {
		return []Finding{}
	}
	return findings
}

// JSON renders the scan as indented JSON.
func (result *Result) JSON() ([]byte, error) {
	payload, err := json.MarshalIndent(result.JSONReport(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal prerelease report: %w", err)
	}
	return append(payload, '\n'), nil
}

// Scope names what the scan was guarding.
func (result *Result) Scope() string {
	if result.Options.Release {
		return "release"
	}
	return "default-branch"
}

// Report is the human report, printed whether the scan passed or failed.
//
// Every finding names its file and line, the key that declares it and the
// version. That is the point rather than detail for its own sake: the person who
// wrote the pin that cost this release round had no reason to know a downstream
// composition's release document depended on it, so the gate has to carry the
// whole story to the place the pin was written.
//
// Findings are grouped by the remedy they share, and the remedy is stated once
// per group. A module repository carries dozens of first-party pseudo-versions
// and a composition can refuse five pins for one reason; repeating the same three
// lines beside each of them buries the locations, which are the part that differs.
func (result *Result) Report() string {
	var b strings.Builder
	blocking := result.Blocking()
	if len(blocking) > 0 {
		destination := "reach the default branch"
		if result.Options.Release {
			destination = "be in a release"
		}
		fmt.Fprintf(&b, "%s must not %s:\n", plural(len(blocking), "prerelease version", "prerelease versions"), destination)
		writeGroups(&b, blocking)
	} else {
		fmt.Fprintf(&b, "%s\n", result.Summary())
	}
	if allowed := result.Allowed(); len(allowed) > 0 {
		fmt.Fprint(&b, "\nFound and permitted here — still a prerelease somebody has to remove:\n")
		writeGroups(&b, allowed)
	}
	return strings.TrimRight(b.String(), "\n")
}

// writeGroups prints findings grouped by the reason and remedy they share, each
// finding on its own line under the group that explains it.
func writeGroups(b *strings.Builder, findings []Finding) {
	for _, group := range groupByRemedy(findings) {
		head := &group[0]
		fmt.Fprintf(b, "\n  %s (%d) — %s\n", head.Carrier, len(group), head.Why)
		for i := range group {
			finding := &group[i]
			fmt.Fprintf(b, "      %s\n          %s = %s   (%s)\n", finding.Location(), finding.Key, finding.Version, finding.Kind)
		}
		for _, line := range head.Remedy {
			fmt.Fprintf(b, "      -> %s\n", line)
		}
	}
}

// groupByRemedy buckets findings that a reader would act on the same way,
// preserving the order they were found in both between and within groups.
func groupByRemedy(findings []Finding) [][]Finding {
	var groups [][]Finding
	index := map[string]int{}
	for i := range findings {
		finding := &findings[i]
		key := string(finding.Carrier) + "\x00" + finding.Why + "\x00" + strings.Join(finding.Remedy, "\x00")
		position, seen := index[key]
		if !seen {
			index[key] = len(groups)
			groups = append(groups, []Finding{*finding})
			continue
		}
		groups[position] = append(groups[position], *finding)
	}
	return groups
}

// Err is the short error a failing scan returns. The detail is in Report, which
// the caller prints: a multi-line error reads badly through an error-chain
// renderer, and the report is output rather than a diagnostic.
func (result *Result) Err() error {
	blocking := result.Blocking()
	if len(blocking) == 0 {
		return nil
	}
	destination := "the default branch"
	if result.Options.Release {
		destination = "a release"
	}
	return fmt.Errorf("%s must not reach %s (see the findings above)",
		plural(len(blocking), "prerelease version", "prerelease versions"), destination)
}

// Failure is Report and Err together, for a caller that wants one error carrying
// everything — a test, or a gate embedded in another command's output.
func (result *Result) Failure() error {
	if result.OK() {
		return nil
	}
	return errors.New(result.Report())
}

// Summary is the one line a passing scan leads with.
func (result *Result) Summary() string {
	destination := "the default branch"
	if result.Options.Release {
		destination = "a release"
	}
	allowed := ""
	if count := len(result.Allowed()); count > 0 {
		allowed = fmt.Sprintf(", %d permitted prerelease(s) reported", count)
	}
	return fmt.Sprintf("no prerelease version reaches %s: %d file(s) scanned%s", destination, len(result.Files), allowed)
}

func plural(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}
