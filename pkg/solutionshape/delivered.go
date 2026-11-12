package solutionshape

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/codefly-dev/cli/pkg/workingrender"
	"gopkg.in/yaml.v3"
)

// Codes the delivered-composition checks add. A caller scripts against these,
// so a code is never renamed or reused for a different meaning.
const (
	CodeNotRenderedForEnvironment  = "not_rendered_for_environment"
	CodeEnvelopeNotRendered        = "module_envelope_not_rendered"
	CodeEnvelopeUnreadable         = "module_envelope_unreadable"
	CodeEnvelopeOmitsBinding       = "module_envelope_omits_binding"
	CodeEnvelopeAdmitsEveryBinding = "module_envelope_admits_every_binding"
)

// deliveredManifestKey is the document a rendered module-manifest ConfigMap
// carries. envelopeKey is the one a rendered envelope carries.
const (
	deliveredManifestKey = "module-manifest.codefly.yaml"
	envelopeKey          = "module-envelope.json"
)

// configMap is the little of a rendered ConfigMap these checks read.
type configMap struct {
	Data map[string]string `yaml:"data"`
}

// deliveredManifest is the little of a module manifest these checks read: its
// alias, the generation the host will record, and the operation bindings its
// authority declares. Those bindings are what the host registers — or drops.
//
// A manifest declares them as a LIST of bindings, each with its own id; the
// envelope keys the same things as a MAP of operation audiences. Reading one
// side's shape on both sides is how this check first passed vacuously against
// a cell whose host was dropping three of them.
type deliveredManifest struct {
	Alias      string `yaml:"alias"`
	Generation uint64 `yaml:"generation"`
	Authority  struct {
		Bindings []struct {
			ID       string `yaml:"id"`
			Audience string `yaml:"audience"`
		} `yaml:"bindings"`
	} `yaml:"authority"`
}

// Ceiling is what a cell's envelope admits: per module principal, the set of
// operation bindings. The host refuses to register any binding outside it, and
// says so one line at a time ("the envelope allows no such operation binding"),
// which is invisible unless somebody reads that service's stdout.
type Ceiling struct {
	Admitted map[string]map[string]bool
}

// ReadCeiling reads the envelope the render emitted for an environment.
func ReadCeiling(path string) (*Ceiling, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var holder configMap
	if err = yaml.Unmarshal(raw, &holder); err != nil {
		return nil, fmt.Errorf("the rendered envelope %s cannot be read: %w", filepath.Base(path), err)
	}
	document, named := holder.Data[envelopeKey]
	if !named {
		return nil, fmt.Errorf("%s carries no %s", filepath.Base(path), envelopeKey)
	}
	var envelope struct {
		Principals map[string]struct {
			OperationAudiences map[string]any `json:"operation_audiences"`
		} `json:"principals"`
	}
	if err = json.Unmarshal([]byte(document), &envelope); err != nil {
		return nil, fmt.Errorf("%s is not readable as an envelope: %w", envelopeKey, err)
	}
	ceiling := &Ceiling{Admitted: map[string]map[string]bool{}}
	for module, principal := range envelope.Principals {
		bindings := map[string]bool{}
		for binding := range principal.OperationAudiences {
			bindings[binding] = true
		}
		ceiling.Admitted[module] = bindings
	}
	return ceiling, nil
}

// Declares reads a rendered module manifest: the module it is for, the
// generation, and the operation bindings it asks the host to register.
func Declares(path string) (module string, generation uint64, bindings []string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", 0, nil, err
	}
	var holder configMap
	if err = yaml.Unmarshal(raw, &holder); err != nil {
		return "", 0, nil, err
	}
	document, named := holder.Data[deliveredManifestKey]
	if !named {
		// Not a module manifest: the same overlay carries the delivery Job and
		// its service account, which this check walks past rather than refuses.
		return "", 0, nil, nil
	}
	var manifest deliveredManifest
	if err = yaml.Unmarshal([]byte(document), &manifest); err != nil {
		return "", 0, nil, fmt.Errorf("%s in %s cannot be read: %w", deliveredManifestKey, filepath.Base(path), err)
	}
	for _, binding := range manifest.Authority.Bindings {
		if binding.ID != "" {
			bindings = append(bindings, binding.ID)
		}
	}
	sort.Strings(bindings)
	return manifest.Alias, manifest.Generation, bindings, nil
}

// Gap is one binding a manifest declares and the cell's envelope does not
// admit. The host drops it, so every operation behind it is unreachable and no
// tool derives from it — which reads in a portal as "tool exposure is
// unavailable" and in an installation as "module X changed since review".
type Gap struct {
	Module     string `json:"module"`
	Binding    string `json:"binding"`
	Generation uint64 `json:"generation"`
}

// InspectDelivered judges what an environment will actually enforce, which the
// directory cannot answer: a solution's shape can be perfect while the cell's
// ceiling admits none of the bindings its modules declare.
//
// root is the workspace root; environment names the rendered overlay. Both the
// manifests and the envelope are render output, so this stays read-only and
// needs no cluster and no credential.
func (r *Report) InspectDelivered(root, environment string) {
	// Two trees hold this, and they answer different questions. The DELIVERED
	// tree (deployments/environments/<env>/modules/...) is what the cell reads,
	// and it is the only one the envelope appears in at all: `publish` writes
	// the envelope, `render` does not. The RENDERED tree answers the same
	// question before a publish: the environment's working render
	// (workingrender.Modules). Read both, and let the delivered one win where
	// they disagree, because that is what the host enforces. deployments/modules
	// is a delivered tree too, for an environment that delivers from the
	// default gitops path; a render never writes it.
	overlay := filepath.Join("*", "module-manifests", "overlays", environment, "*.yaml")
	rendered, _ := filepath.Glob(filepath.Join(workingrender.Modules(root, environment), overlay))
	committed, _ := filepath.Glob(filepath.Join(root, "deployments", "modules", overlay))
	delivered, _ := filepath.Glob(filepath.Join(root, "deployments", "environments", environment, "modules", overlay))
	overlays := make([]string, 0, len(rendered)+len(committed)+len(delivered))
	overlays = append(overlays, rendered...)
	overlays = append(overlays, committed...)
	overlays = append(overlays, delivered...)
	if len(overlays) == 0 {
		r.fail(CodeNotRenderedForEnvironment,
			fmt.Sprintf("no module manifest is rendered for environment %q, so nothing can be said about what that cell would enforce", environment),
			fmt.Sprintf("render the cell: codefly deploy gitops render --all --env %s", environment))
		return
	}

	var ceiling *Ceiling
	var ceilingPath string
	declared := map[string][]string{}
	generations := map[string]uint64{}
	for _, overlay := range overlays {
		if filepath.Base(overlay) == "module-envelope.yaml" {
			// Later wins: the delivered envelope is appended after the rendered one.
			ceilingPath = overlay
			continue
		}
		module, generation, bindings, readErr := Declares(overlay)
		if readErr != nil {
			r.fail(CodeEnvelopeUnreadable, readErr.Error(),
				"re-render the module; a manifest this check cannot read is one the host cannot read either")
			continue
		}
		if module == "" {
			continue
		}
		declared[module] = bindings
		generations[module] = generation
	}

	if ceilingPath == "" {
		r.fail(CodeEnvelopeNotRendered,
			fmt.Sprintf("environment %q carries no module envelope, so the host admits no operation binding at all", environment),
			"the envelope is written by `codefly deploy gitops publish`, not by render: publish the host module for this environment. Without it every module's bindings are dropped")
		return
	}
	ceiling, readErr := ReadCeiling(ceilingPath)
	if readErr != nil {
		r.fail(CodeEnvelopeUnreadable, readErr.Error(),
			"re-publish the host module; an envelope this check cannot read is one the host cannot read either")
		return
	}

	var gaps []Gap
	for _, module := range sortedKeys(declared) {
		admitted := ceiling.Admitted[module]
		for _, binding := range declared[module] {
			if !admitted[binding] {
				gaps = append(gaps, Gap{Module: module, Binding: binding, Generation: generations[module]})
			}
		}
	}
	if len(gaps) == 0 {
		r.ok(CodeEnvelopeAdmitsEveryBinding,
			fmt.Sprintf("environment %q admits every operation binding its %d rendered manifests declare", environment, len(declared)))
		return
	}
	named := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		named = append(named, fmt.Sprintf("%s/%s (manifest generation %d)", gap.Module, gap.Binding, gap.Generation))
	}
	r.fail(CodeEnvelopeOmitsBinding,
		fmt.Sprintf("environment %q admits none of these operation bindings its manifests declare: %s", environment, strings.Join(named, ", ")),
		"widen the ceiling for exactly these bindings (`obinctl module-envelope generate` derives what the applied manifests ask for; narrow it, then write it into the environment's module-envelope and re-render). Until then the host drops each one, no tool derives from it, and an installation refuses with \"module ... changed since review\"")
}

func sortedKeys(m map[string][]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
