package environments

import (
	"testing"
)

// The operator's half of an admission is read back as declared, by service,
// endpoint and caller: this is the ACCEPTANCE the render looks for, and
// nothing is implied — an environment that declares none admits none, so a
// service's own proposal grants nothing on its own.
func TestAdmittedSourcesAreReadBackByCaller(t *testing.T) {
	env := decodeEnvironment(t, `name: production
admitted-sources:
  - service: relay/ingest
    endpoint: ingest
    from: [other/webhook, other/replay]
`)
	if err := env.validateAdmittedSources(); err != nil {
		t.Fatal(err)
	}
	if !env.AdmitsSource("relay/ingest", "ingest", "other/webhook") {
		t.Fatal("a co-declared caller is not read back")
	}
	for _, missing := range [][3]string{
		{"relay/ingest", "ingest", "side/tool"},
		{"relay/ingest", "other", "other/webhook"},
		{"relay/other", "ingest", "other/webhook"},
	} {
		if env.AdmitsSource(missing[0], missing[1], missing[2]) {
			t.Fatalf("%v is admitted by nothing", missing)
		}
	}
	if callers := env.AdmittedSourceCallers("relay/ingest", "ingest"); len(callers) != 2 || callers[0] != "other/replay" {
		t.Fatalf("callers = %v, want both, sorted", callers)
	}
	var none *Environment
	if none.AdmitsSource("relay/ingest", "ingest", "other/webhook") {
		t.Fatal("no environment admits nothing")
	}
}

// Each entry is held to the shape an admission is written in, refused by name.
// Whether the composition carries the service, the endpoint or the caller is
// judged where the reach is derived, against the manifest that proposed it —
// this half knows only the shape.
func TestAdmittedSourcesAreRefusedByName(t *testing.T) {
	for _, bad := range []struct{ name, body string }{
		{"a service that is not module-qualified", "name: s\nadmitted-sources:\n  - service: ingest\n    endpoint: ingest\n    from: [other/webhook]\n"},
		{"a service with an empty module", "name: s\nadmitted-sources:\n  - service: /ingest\n    endpoint: ingest\n    from: [other/webhook]\n"},
		{"no endpoint", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    from: [other/webhook]\n"},
		{"the same endpoint twice", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: [other/webhook]\n  - service: relay/ingest\n    endpoint: ingest\n    from: [other/replay]\n"},
		{"nobody", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: []\n"},
		{"a caller that is not module-qualified", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: [webhook]\n"},
		{"a caller with three segments", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: [a/b/c]\n"},
		{"an unnamed caller", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: ['']\n"},
		{"the service that serves it", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: [relay/ingest]\n"},
		{"a caller twice", "name: s\nadmitted-sources:\n  - service: relay/ingest\n    endpoint: ingest\n    from: [other/webhook, other/webhook]\n"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if err := decodeEnvironment(t, bad.body).validateAdmittedSources(); err == nil {
				t.Fatalf("accepted: %s", bad.body)
			}
		})
	}
}

// Two entries may name two endpoints of one service, and two services one
// endpoint name: the key is the pair, not either half.
func TestAdmittedSourcesAllowEntriesThatDoNotCollide(t *testing.T) {
	env := decodeEnvironment(t, `name: production
admitted-sources:
  - service: relay/ingest
    endpoint: ingest
    from: [other/webhook]
  - service: relay/ingest
    endpoint: scrape
    from: [observe/agent]
  - service: store/db
    endpoint: ingest
    from: [relay/ingest]
`)
	if err := env.validateAdmittedSources(); err != nil {
		t.Fatal(err)
	}
}
