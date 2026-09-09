// Package conformance declares which CLI/core/agent combinations the
// repository claims to support and what evidence backs each claim.
//
// A row is "qualified" only when CI actually drives it end to end. The gate in
// this package is what makes that claim mechanical: a row a CI job declares
// required can no longer skip itself when Docker, Nix or a cluster is missing,
// it fails.
package conformance

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/blang/semver"
)

// MatrixRelativePath locates the matrix from the repository root.
const MatrixRelativePath = "pkg/conformance/matrix.json"

//go:embed matrix.json
var embeddedMatrix []byte

// Status is the support claim a row makes.
type Status string

const (
	// StatusQualified means CI drives the row end to end on every change.
	StatusQualified Status = "qualified"
	// StatusNotYetQualified means the row has a real gate and real tests, but
	// nothing proves it outside a developer machine. It is not supported.
	StatusNotYetQualified Status = "not-yet-qualified"
	// StatusUnsupported means the combination is not shipped and not tested.
	StatusUnsupported Status = "unsupported"
)

// Agent is an exact agent pin a row drives.
type Agent struct {
	Publisher string `json:"publisher"`
	Name      string `json:"name"`
	Version   string `json:"version"`
}

// Identifier renders the pin the way `codefly agent install` accepts it.
func (a Agent) Identifier() string {
	return fmt.Sprintf("%s/%s:%s", a.Publisher, a.Name, a.Version)
}

// Row is one supported, unsupported or not-yet-qualified combination.
type Row struct {
	ID            string   `json:"id"`
	Status        Status   `json:"status"`
	Summary       string   `json:"summary"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Backend       string   `json:"backend"`
	Kubernetes    string   `json:"kubernetes,omitempty"`
	Prerequisites []string `json:"prerequisites,omitempty"`
	GateEnv       string   `json:"gate_env,omitempty"`
	BuildTag      string   `json:"build_tag,omitempty"`
	Agents        []Agent  `json:"agents,omitempty"`
	CI            []string `json:"ci,omitempty"`
	Gates         []string `json:"gates,omitempty"`
	Blockers      []string `json:"blockers,omitempty"`
}

// Matrix is the whole published claim.
type Matrix struct {
	SchemaVersion int    `json:"schema_version"`
	CLI           string `json:"cli"`
	Core          string `json:"core"`
	Rows          []Row  `json:"rows"`
}

var defaultMatrix = mustParse(embeddedMatrix)

// Default returns the matrix compiled into this build.
func Default() Matrix { return defaultMatrix }

func mustParse(payload []byte) Matrix {
	matrix, err := Parse(payload)
	if err != nil {
		panic(err)
	}
	return matrix
}

// Parse decodes and validates a matrix document.
func Parse(payload []byte) (Matrix, error) {
	var matrix Matrix
	if err := json.Unmarshal(payload, &matrix); err != nil {
		return Matrix{}, fmt.Errorf("parse conformance matrix: %w", err)
	}
	if err := matrix.Validate(); err != nil {
		return Matrix{}, err
	}
	return matrix, nil
}

// Row returns the row with the given identifier.
func (m Matrix) Row(id string) (Row, bool) {
	for i := range m.Rows {
		if m.Rows[i].ID == id {
			return m.Rows[i], true
		}
	}
	return Row{}, false
}

// IDs returns every row identifier, in declaration order.
func (m Matrix) IDs() []string {
	ids := make([]string, 0, len(m.Rows))
	for i := range m.Rows {
		ids = append(ids, m.Rows[i].ID)
	}
	return ids
}

// Requirable reports whether a CI job may declare the row required. A row with
// no gate call site has nothing to enforce, so requiring it would be a claim
// no test can honour.
func (r *Row) Requirable() bool {
	return r.Status != StatusUnsupported && len(r.Gates) > 0
}

var (
	rowIDPattern   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	gateEnvPattern = regexp.MustCompile(`^CODEFLY_[A-Z0-9_]+$`)
	coreVersion    = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	ciEntryPattern = regexp.MustCompile(`^[a-z0-9.-]+\.ya?ml#[a-z0-9-]+$`)
)

// backendCluster is the only backend that reaches Kubernetes, so it is the
// only one for which a pinned cluster version is meaningful.
const backendCluster = "k3d"

var (
	knownOS       = map[string]bool{"linux": true, "darwin": true, "windows": true}
	knownArch     = map[string]bool{"amd64": true, "arm64": true}
	knownBackends = map[string]bool{"none": true, "native": true, "docker": true, "nix": true, backendCluster: true}
)

// Validate enforces the invariants that make the matrix a claim rather than a
// wish list.
func (m Matrix) Validate() error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("conformance matrix schema_version = %d, want 1", m.SchemaVersion)
	}
	if m.CLI != "source" {
		return fmt.Errorf("conformance matrix cli = %q, want \"source\": rows describe the checkout under test", m.CLI)
	}
	if !coreVersion.MatchString(m.Core) {
		return fmt.Errorf("conformance matrix core = %q, want a vX.Y.Z pin", m.Core)
	}
	if len(m.Rows) == 0 {
		return fmt.Errorf("conformance matrix has no rows")
	}
	ids := map[string]bool{}
	gateEnvs := map[string]string{}
	for i := range m.Rows {
		row := &m.Rows[i]
		if err := row.validate(); err != nil {
			return err
		}
		if ids[row.ID] {
			return fmt.Errorf("conformance matrix repeats row %q", row.ID)
		}
		ids[row.ID] = true
		if row.GateEnv == "" {
			continue
		}
		if owner, taken := gateEnvs[row.GateEnv]; taken {
			return fmt.Errorf("conformance rows %q and %q share gate_env %s", owner, row.ID, row.GateEnv)
		}
		gateEnvs[row.GateEnv] = row.ID
	}
	return nil
}

func (r *Row) validate() error {
	if !rowIDPattern.MatchString(r.ID) {
		return fmt.Errorf("conformance row id %q must be lowercase dash-separated", r.ID)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return fmt.Errorf("conformance row %s has no summary", r.ID)
	}
	switch r.Status {
	case StatusQualified, StatusNotYetQualified, StatusUnsupported:
	default:
		return fmt.Errorf("conformance row %s has unknown status %q", r.ID, r.Status)
	}
	if !knownOS[r.OS] {
		return fmt.Errorf("conformance row %s has unknown os %q", r.ID, r.OS)
	}
	if !knownArch[r.Arch] {
		return fmt.Errorf("conformance row %s has unknown arch %q", r.ID, r.Arch)
	}
	if !knownBackends[r.Backend] {
		return fmt.Errorf("conformance row %s has unknown backend %q", r.ID, r.Backend)
	}
	if r.Kubernetes != "" && r.Backend != backendCluster {
		return fmt.Errorf("conformance row %s declares a kubernetes version without a cluster backend", r.ID)
	}
	if err := r.validatePrerequisites(); err != nil {
		return err
	}
	if err := r.validateGates(); err != nil {
		return err
	}
	if err := r.validateAgents(); err != nil {
		return err
	}
	if r.GateEnv != "" && !gateEnvPattern.MatchString(r.GateEnv) {
		return fmt.Errorf("conformance row %s has invalid gate_env %q", r.ID, r.GateEnv)
	}
	for _, entry := range r.CI {
		if !ciEntryPattern.MatchString(entry) {
			return fmt.Errorf("conformance row %s has invalid ci entry %q, want <workflow>.yml#<job or gate>", r.ID, entry)
		}
	}
	return r.validateStatusEvidence()
}

// validateStatusEvidence keeps each status honest about what backs it.
func (r *Row) validateStatusEvidence() error {
	switch r.Status {
	case StatusQualified:
		if len(r.CI) == 0 {
			return fmt.Errorf("conformance row %s is qualified but names no CI job", r.ID)
		}
		if len(r.Blockers) > 0 {
			return fmt.Errorf("conformance row %s is qualified but still lists blockers", r.ID)
		}
		if r.Backend == backendCluster && r.Kubernetes == "" {
			return fmt.Errorf("conformance row %s is a qualified cluster row and must pin a kubernetes version", r.ID)
		}
	case StatusNotYetQualified:
		if len(r.CI) > 0 {
			return fmt.Errorf("conformance row %s names a CI job but is not qualified; qualify it or drop the job", r.ID)
		}
		if len(r.Blockers) == 0 {
			return fmt.Errorf("conformance row %s is not qualified and must say what blocks it", r.ID)
		}
	case StatusUnsupported:
		if len(r.CI) > 0 || len(r.Gates) > 0 || len(r.Prerequisites) > 0 ||
			len(r.Agents) > 0 || len(r.Blockers) > 0 || r.GateEnv != "" || r.BuildTag != "" {
			return fmt.Errorf("conformance row %s is unsupported and must carry no evidence or gate", r.ID)
		}
	}
	return nil
}

func (r *Row) validatePrerequisites() error {
	seen := map[string]bool{}
	for _, prerequisite := range r.Prerequisites {
		if prerequisite == "" || prerequisite != strings.ToLower(prerequisite) || strings.ContainsAny(prerequisite, "/\\ ") {
			return fmt.Errorf("conformance row %s has invalid prerequisite %q, want a bare executable name", r.ID, prerequisite)
		}
		if seen[prerequisite] {
			return fmt.Errorf("conformance row %s repeats prerequisite %q", r.ID, prerequisite)
		}
		seen[prerequisite] = true
	}
	if len(r.Prerequisites) > 0 && len(r.Gates) == 0 {
		return fmt.Errorf("conformance row %s declares prerequisites but no gate call site enforces them", r.ID)
	}
	if r.GateEnv != "" && len(r.Gates) == 0 {
		return fmt.Errorf("conformance row %s declares gate_env %s but no gate call site reads it", r.ID, r.GateEnv)
	}
	return nil
}

func (r *Row) validateGates() error {
	seen := map[string]bool{}
	for _, gate := range r.Gates {
		if gate == "" || strings.HasPrefix(gate, "/") || strings.Contains(gate, "..") || strings.Contains(gate, "\\") {
			return fmt.Errorf("conformance row %s has invalid gate package %q", r.ID, gate)
		}
		if seen[gate] {
			return fmt.Errorf("conformance row %s repeats gate package %q", r.ID, gate)
		}
		seen[gate] = true
	}
	return nil
}

func (r *Row) validateAgents() error {
	seen := map[string]bool{}
	for _, agent := range r.Agents {
		if agent.Publisher == "" || agent.Name == "" {
			return fmt.Errorf("conformance row %s has an agent without publisher or name", r.ID)
		}
		if _, err := semver.Parse(agent.Version); err != nil {
			return fmt.Errorf("conformance row %s pins %s/%s at invalid exact version %q", r.ID, agent.Publisher, agent.Name, agent.Version)
		}
		identity := agent.Publisher + "/" + agent.Name
		if seen[identity] {
			return fmt.Errorf("conformance row %s repeats agent %s", r.ID, identity)
		}
		seen[identity] = true
	}
	return nil
}
