// Package posture holds the security posture a deployed render is held to: the
// rules a restricted (deployed) render refuses a workload for, the facts an
// environment asserts about the platform it deploys onto, and the explicit
// allowances an owner declares to make an exception of one service.
//
// The rules themselves are the owner's decision, recorded in
// obin-ai/handbook decisions/security-posture.md (handbook#215). This package is
// where they are enforced, because the render is the last door before a cell and
// the one every module, solution and agent passes through. Each refusal names
// three things — the service, the rule, and the field that triggered it — so the
// person reading it knows what to change without reading this code.
//
// Nothing here is product-specific: a rule is written against the shape of a
// rendered Kubernetes workload, never against the name of a service, module or
// product that happens to break it today.
package posture

import (
	"fmt"
	"sort"
	"strings"
)

// The rules a deployed render enforces. A rule identifier is stable: it appears
// in refusals, in an environment's allowances, and in docs/commands.md.
const (
	// RulePeerTransportMaterial refuses a service that carries its own TLS
	// material for its in-cell peers while the environment asserts that
	// transport between workloads is already protected. Transport between
	// workloads belongs to the mesh; edge TLS belongs to the ingress.
	RulePeerTransportMaterial = "peer-transport-material"

	// RuleNonScratchMount refuses a workload volume that is anything but
	// scratch space or a durable data claim. Configuration and credentials
	// reach a service as values and secrets through the environment variables
	// the render already projects, so a mounted ConfigMap, Secret, projected
	// volume or host path is a sign something reads a file the platform never
	// delivers.
	RuleNonScratchMount = "non-scratch-mount"

	// RuleInMemoryStateStore refuses a workload started in a development or
	// in-memory mode. A deployed product's keys and state live in a durable
	// store: an in-memory one loses them on the next node replacement.
	RuleInMemoryStateStore = "in-memory-state-store"
)

// Rules lists every rule identifier, in refusal-reporting order.
var Rules = []string{RulePeerTransportMaterial, RuleNonScratchMount, RuleInMemoryStateStore}

// AssertMeshProtectedTransport is the environment assertion that transport
// between workloads inside the cell is already protected by the platform. It is
// what makes RulePeerTransportMaterial applicable: without it, a service
// carrying its own TLS material is that environment's only transport protection
// and the render says nothing.
const AssertMeshProtectedTransport = "internal-transport/mesh-protected"

// Asserts lists every assertion an environment may declare.
var Asserts = []string{AssertMeshProtectedTransport}

// Declaration is an environment's security-posture declaration, read from the
// `posture` block of its workspace entry (or of the coordinate contract that
// imports it). A nil Declaration asserts nothing and allows nothing, which is
// the default every environment starts from.
type Declaration struct {
	// Asserts are the facts the environment states about the platform it
	// deploys onto, keyed by assertion identifier.
	Asserts map[string]bool `yaml:"asserts,omitempty"`
	// Allowances are the deliberate exceptions. Each names one rule and one
	// service and says why; the render prints every one of them on every run,
	// so an exception can never become a silent skip.
	Allowances []Allowance `yaml:"allowances,omitempty"`
}

// Allowance is one reviewed exception to one rule, for one service.
type Allowance struct {
	// Rule is the rule identifier this exception applies to.
	Rule string `yaml:"rule"`
	// Service is "<module>/<service>", or a bare "<service>" to name that
	// service in whichever module renders it.
	Service string `yaml:"service"`
	// Reason is why the exception exists. It is required: an allowance with no
	// stated reason is a silent skip wearing a declaration's clothes.
	Reason string `yaml:"reason"`
}

// Subject is the rendered unit a workload belongs to — the service a refusal
// names. Module may be empty when only the unit name is known.
type Subject struct {
	Module  string
	Service string
}

// String is how a subject is named in a refusal and matched against an
// allowance: "<module>/<service>", or the bare service when the module is
// unknown.
func (subject Subject) String() string {
	if subject.Module == "" {
		return subject.Service
	}
	if subject.Service == "" {
		return subject.Module
	}
	return subject.Module + "/" + subject.Service
}

// Validate refuses a posture declaration the render could not act on: an
// assertion or rule it does not know (a typo would otherwise assert nothing and
// allow nothing, silently), and an allowance missing the service or the reason
// that makes it reviewable.
func (declaration *Declaration) Validate() error {
	if declaration == nil {
		return nil
	}
	for _, name := range sortedKeys(declaration.Asserts) {
		if !known(Asserts, name) {
			return fmt.Errorf("posture asserts unknown fact %q (known: %s)", name, strings.Join(Asserts, ", "))
		}
	}
	for index := range declaration.Allowances {
		allowance := &declaration.Allowances[index]
		if !known(Rules, allowance.Rule) {
			return fmt.Errorf("posture allowance %d names unknown rule %q (known: %s)", index+1, allowance.Rule, strings.Join(Rules, ", "))
		}
		if strings.TrimSpace(allowance.Service) == "" {
			return fmt.Errorf("posture allowance for rule %s must name a service", allowance.Rule)
		}
		if strings.TrimSpace(allowance.Reason) == "" {
			return fmt.Errorf(
				"posture allowance for rule %s on service %s must state a reason: an exception the render prints with no reason is a silent skip",
				allowance.Rule, allowance.Service)
		}
	}
	return nil
}

// Asserted reports whether the environment states a fact.
func (declaration *Declaration) Asserted(name string) bool {
	if declaration == nil {
		return false
	}
	return declaration.Asserts[name]
}

// Allows reports the allowance that excuses a rule for a subject, if any. A
// bare service name in an allowance matches that service in any module: an
// allowance is a human decision about a service, and the module it is composed
// under is not always the one that wrote it.
func (declaration *Declaration) Allows(rule string, subject Subject) (Allowance, bool) {
	if declaration == nil {
		return Allowance{}, false
	}
	for _, allowance := range declaration.Allowances {
		if allowance.Rule != rule {
			continue
		}
		named := strings.TrimSpace(allowance.Service)
		if named == subject.String() || (!strings.Contains(named, "/") && named == subject.Service) {
			return allowance, true
		}
	}
	return Allowance{}, false
}

// Report is the line per declared allowance that a render prints on every run.
// It is never conditional on the allowance being exercised: the point is that an
// exception stays visible to whoever reads the render's output, including the
// run where the service that needed it no longer does.
func (declaration *Declaration) Report() []string {
	if declaration == nil || len(declaration.Allowances) == 0 {
		return nil
	}
	lines := make([]string, 0, len(declaration.Allowances))
	for _, allowance := range declaration.Allowances {
		lines = append(lines, fmt.Sprintf(
			"security posture: service %s is allowed to break rule %s — %s",
			allowance.Service, allowance.Rule, allowance.Reason))
	}
	return lines
}

// Violation is one refusal: the rule broken, the service that broke it, and the
// manifest field that triggered it.
type Violation struct {
	Rule    string
	Subject Subject
	// Location is the manifest the workload was rendered into.
	Location string
	// Workload identifies the workload inside that manifest, e.g.
	// "StatefulSet/store".
	Workload string
	// Field is the path of the triggering field inside the workload document.
	Field string
	// Detail says what the field holds and what to do instead.
	Detail string
}

// Error names the service, the rule and the field, then the allowance that
// would make the exception deliberate.
func (violation *Violation) Error() string {
	where := violation.Field
	if violation.Workload != "" {
		where = violation.Workload + " " + violation.Field
	}
	if violation.Location != "" {
		where = violation.Location + ": " + where
	}
	return fmt.Sprintf(
		"deployed render refuses service %s: %s — %s (%s). "+
			"Declare a deliberate exception as an environment-level posture allowance "+
			"(posture.allowances: rule %s, service %s, and the reason), which the render then prints on every run. "+
			"The rule is obin-ai/handbook decisions/security-posture.md",
		violation.Subject, violation.Rule, violation.Detail, where,
		violation.Rule, violation.Subject)
}

func known(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
