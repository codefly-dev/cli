package solutionshape

import (
	"os"
	"path/filepath"
	"testing"
)

// rendered writes a module-manifest overlay into the module tree at tree the
// way `gitops render` emits one.
func rendered(t *testing.T, tree, module, environment, document string) {
	t.Helper()
	dir := filepath.Join(tree, module, "module-manifests", "overlays", environment)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "platform."+environment+"."+module+".yaml"), document)
}

func manifestDocument(alias string, generation int, bindings ...string) string {
	// A manifest declares a LIST of bindings, each with its own id — the shape
	// the host reads. The envelope keys the same things as a map.
	declared := ""
	for _, binding := range bindings {
		declared += "            - id: " + binding + "\n              audience: " + binding + "\n"
	}
	if declared == "" {
		declared = "            []\n"
	}
	return `apiVersion: v1
kind: ConfigMap
data:
    module-manifest.codefly.yaml: |
        alias: ` + alias + `
        generation: ` + itoa(generation) + `
        authority:
          bindings:
` + declared
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

func envelopeDocument(json string) string {
	return `apiVersion: v1
kind: ConfigMap
data:
    module-envelope.json: |
        ` + json + `
`
}

func reportFor(module string) *Report {
	return &Report{SchemaVersion: SchemaVersion, Module: module, IsSolution: true, Status: StatusOK}
}

// The class this check exists for: a solution whose shape is perfect, on a cell
// whose ceiling admits none of the bindings its modules declare. The host drops
// each one on its own stdout and nothing else says so.
func TestACeilingThatOmitsADeclaredBindingFails(t *testing.T) {
	root := t.TempDir()
	rendered(t, root, "evaluation", "blue", manifestDocument("evaluation", 5, "score", "tracing-read"))
	rendered(t, root, "wiki", "blue", manifestDocument("wiki", 3, "documents"))
	write(t, filepath.Join(root, "wiki", "module-manifests", "overlays", "blue", "module-envelope.yaml"),
		envelopeDocument(`{"principals":{"evaluation":{"resources":["evaluation.runs"]},"wiki":{"operation_audiences":{"documents":{}}}}}`))

	report := reportFor("wiki")
	report.InspectDelivered([]string{root}, "blue")

	check := checkFor(t, report, CodeEnvelopeOmitsBinding)
	if check.Status != StatusFail {
		t.Fatalf("a ceiling that admits no evaluation binding must fail, got %s", check.Status)
	}
	for _, named := range []string{"evaluation/score", "evaluation/tracing-read", "generation 5"} {
		if !contains(check.Message, named) {
			t.Errorf("the message must name %q so the fix is obvious; got %q", named, check.Message)
		}
	}
	if contains(check.Message, "wiki/documents") {
		t.Errorf("a binding the ceiling admits must not be reported: %q", check.Message)
	}
	if report.Conforms() {
		t.Error("a dropped binding makes the solution unusable, so the report must not conform")
	}
}

func TestACeilingThatAdmitsEveryBindingPasses(t *testing.T) {
	root := t.TempDir()
	rendered(t, root, "wiki", "blue", manifestDocument("wiki", 3, "documents", "runtime"))
	write(t, filepath.Join(root, "wiki", "module-manifests", "overlays", "blue", "module-envelope.yaml"),
		envelopeDocument(`{"principals":{"wiki":{"operation_audiences":{"documents":{},"runtime":{}}}}}`))

	report := reportFor("wiki")
	report.InspectDelivered([]string{root}, "blue")

	if checkFor(t, report, CodeEnvelopeAdmitsEveryBinding).Status != StatusOK {
		t.Error("every declared binding is admitted, so the check must pass")
	}
	if !report.Conforms() {
		t.Error("nothing is wrong, so the report must conform")
	}
}

// An environment nobody rendered must not read as an environment with nothing
// wrong: that is the vacuous pass this command refuses elsewhere too.
func TestAnUnrenderedEnvironmentFails(t *testing.T) {
	report := reportFor("wiki")
	report.InspectDelivered([]string{t.TempDir()}, "blue")
	if checkFor(t, report, CodeNotRenderedForEnvironment).Status != StatusFail {
		t.Error("an environment with no rendered manifest must fail, not pass silently")
	}
}

// A cell with manifests and no envelope admits nothing at all, which is worse
// than a narrow ceiling and must be said differently.
func TestManifestsWithNoEnvelopeFails(t *testing.T) {
	root := t.TempDir()
	rendered(t, root, "wiki", "blue", manifestDocument("wiki", 3, "documents"))
	report := reportFor("wiki")
	report.InspectDelivered([]string{root}, "blue")
	if checkFor(t, report, CodeEnvelopeNotRendered).Status != StatusFail {
		t.Error("no envelope means no binding is admitted; that must fail by its own name")
	}
}

// The trees are read in order and the later wins: the delivered tree, read
// after the working render, is what the host enforces. Only the trees given
// are read, so another environment's tree, never among them, cannot speak.
func TestTheLaterTreeWinsAndNoOtherIsRead(t *testing.T) {
	working, delivered, foreign := t.TempDir(), t.TempDir(), t.TempDir()
	rendered(t, working, "wiki", "blue", manifestDocument("wiki", 3, "documents"))
	write(t, filepath.Join(working, "wiki", "module-manifests", "overlays", "blue", "module-envelope.yaml"), envelopeDocument(`{"principals":{}}`))
	write(t, filepath.Join(delivered, "wiki", "module-manifests", "overlays", "blue", "module-envelope.yaml"),
		envelopeDocument(`{"principals":{"wiki":{"operation_audiences":{"documents":{}}}}}`))
	write(t, filepath.Join(foreign, "wiki", "module-manifests", "overlays", "blue", "module-envelope.yaml"), envelopeDocument(`{"principals":{}}`))

	report := reportFor("wiki")
	report.InspectDelivered([]string{working, delivered}, "blue")
	if checkFor(t, report, CodeEnvelopeAdmitsEveryBinding).Status != StatusOK {
		t.Error("the delivered envelope admits the binding and is read last, so it must win")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
