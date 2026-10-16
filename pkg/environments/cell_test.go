package environments

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeEnvironment(t *testing.T, body string) *Environment {
	t.Helper()
	var env Environment
	if err := yaml.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	return &env
}

// The cell and egress declarations are keyed by the module-qualified service,
// name what the composition owns, and refuse an entry that declares nothing.
func TestCellAndEgressDeclarationsAreValidatedAndReadBack(t *testing.T) {
	env := decodeEnvironment(t, `name: staging
egress:
  shop/api: {hosts: [identity.example.test, {name: api.github.com, port: 8443}]}
cell:
  shop/api: {bindings: [vault, identity], cloud-identity: true}
`)
	if err := env.validateEgress(); err != nil {
		t.Fatal(err)
	}
	if err := env.validateCell(); err != nil {
		t.Fatal(err)
	}
	hosts := env.EgressHosts("shop", "api")
	if len(hosts) != 2 || hosts[0].Name != "api.github.com" || hosts[0].Port != 8443 || hosts[1].Port != EgressDefaultPort {
		t.Fatalf("hosts = %+v", hosts)
	}
	grant, declared := env.CellWorkload("shop", "api")
	if !declared || !grant.CloudIdentity || strings.Join(grant.Bindings, ",") != "identity,vault" {
		t.Fatalf("grant = %+v declared=%v", grant, declared)
	}
	if _, declared := env.CellWorkload("shop", "web"); declared {
		t.Fatal("an undeclared service has no grant")
	}
	for _, bad := range []string{
		"name: s\ncell:\n  api: {bindings: [vault]}\n",
		"name: s\ncell:\n  shop/api: {}\n",
		"name: s\ncell:\n  shop/api: {bindings: [vault, vault]}\n",
		"name: s\negress:\n  shop/api: {hosts: [https://x.example.test]}\n",
		"name: s\negress:\n  shop/api: {hosts: []}\n",
	} {
		env := decodeEnvironment(t, bad)
		if env.validateCell() == nil && env.validateEgress() == nil {
			t.Fatalf("accepted: %s", bad)
		}
	}
}
