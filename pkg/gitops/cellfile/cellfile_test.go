package cellfile

import (
	"strings"
	"testing"
)

func sample() *File {
	return &File{
		Schema: Schema, Coordinate: "obin-staging", Component: "platform-obin", Environment: "staging",
		Source: &Source{Repo: "git@github.com:obin-ai/platform-obin.git", Revision: strings.Repeat("a", 40), Branch: "staging-obin"},
		Domain: "obin", TrustDomain: "cluster.local",
		Namespaces: []Namespace{{
			Name: "platform-obin-saas", Module: "saas",
			Workloads: []Workload{{
				Name: "accounts", Kind: "Deployment", Selector: map[string]string{"app": "accounts"},
				Service: "saas/accounts", ServiceAccount: "default",
				SPIFFEID: "spiffe://cluster.local/ns/platform-obin-saas/sa/default", Authenticating: "accounts",
				Containers: []Container{{Name: "accounts", Image: Image{Repository: "r/saas/accounts", Digest: "sha256:" + strings.Repeat("b", 64)}}},
				Endpoints:  []Endpoint{{Name: "rest", API: "rest", Port: 8080, ServicePort: 8080, Consumers: []string{"wiki/backend"}}},
				Verifier:   true, Bindings: []string{"audit", "identity", "vault"},
			}},
		}},
	}
}

// The writer is held to its reader: what Encode writes, Parse reads back as
// the same document, with every field the consumer keys on in place.
func TestTheCellFileRoundTripsThroughItsOwnReader(t *testing.T) {
	data, err := sample().Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"schema: codefly/cell/v1", "trust_domain: cluster.local", "spiffe_id: spiffe://", "service_port: 8080", "- wiki/backend", "verifier: true", "revision: " + strings.Repeat("a", 40)} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("encoded file lacks %q:\n%s", want, data)
		}
	}
	parsed, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Namespaces[0].Workloads[0].Endpoints[0].Consumers[0] != "wiki/backend" {
		t.Fatalf("parsed = %+v", parsed)
	}
}

// A key neither side agreed on is refused, as is another schema: the shape is
// a published interface.
func TestTheReaderRefusesUnknownKeysAndAnotherSchema(t *testing.T) {
	if _, err := Parse([]byte("schema: codefly/cell/v1\ncoordinate: c\ncomponent: p\nenvironment: e\ntrust_domain: t.l\nextra: 1\nnamespaces: [{name: n, module: m}]\n")); err == nil {
		t.Fatal("an unknown key must be refused")
	}
	if _, err := Parse([]byte("schema: codefly/cell/v2\ncoordinate: c\ncomponent: p\nenvironment: e\ntrust_domain: t.l\nnamespaces: [{name: n, module: m}]\n")); err == nil {
		t.Fatal("another schema must be refused")
	}
	wrong := sample()
	wrong.Schema = "codefly/cell/v2"
	if _, err := wrong.Encode(); err == nil {
		t.Fatal("the writer refuses a schema it does not write")
	}
}
