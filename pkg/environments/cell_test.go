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

// A service-mesh entry labels a presenter's Service: it is read back as
// declared, and refused, naming the service, for a service that declares no
// presenter endpoint, a label the render owns, or an unknown key.
func TestServiceMeshLabelsOnlyAPresenter(t *testing.T) {
	env := decodeEnvironment(t, `name: staging
cell:
  saas/auth-gateway: {presenter: [workload]}
  saas/accounts: {bindings: [vault]}
service-mesh:
  services:
    saas/auth-gateway:
      labels: {istio.io/use-waypoint: ns-waypoint, istio.io/use-waypoint-namespace: istio-waypoint}
`)
	if err := env.validateServiceMesh(); err != nil {
		t.Fatal(err)
	}
	if labels := env.ServiceMeshLabels("saas", "auth-gateway"); labels["istio.io/use-waypoint"] != "ns-waypoint" || labels["istio.io/use-waypoint-namespace"] != "istio-waypoint" {
		t.Fatalf("labels = %v", labels)
	}
	if labels := env.ServiceMeshLabels("saas", "accounts"); labels != nil {
		t.Fatalf("a service without an entry is labelled %v", labels)
	}
	for bad, refusal := range map[string]string{
		"cell:\n  saas/accounts: {bindings: [vault]}\nservice-mesh:\n  services:\n    saas/accounts: {labels: {istio.io/use-waypoint: w}}\n": "service-mesh saas/accounts: the service declares no presenter endpoint",
		"service-mesh:\n  services:\n    saas/missing: {labels: {istio.io/use-waypoint: w}}\n":                                               "service-mesh saas/missing: the service declares no presenter endpoint",
		"cell:\n  saas/g: {presenter: [workload]}\nservice-mesh:\n  services:\n    saas/g: {labels: {codefly.dev/workload: w}}\n":            `service-mesh saas/g: label "codefly.dev/workload" is the render's own`,
		"service-mesh:\n  services:\n    g: {labels: {a: b}}\n":                                                                              "module-qualified",
	} {
		env := decodeEnvironment(t, "name: s\n"+bad)
		if err := env.validateServiceMesh(); err == nil || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("%s: err = %v, want %q", bad, err, refusal)
		}
	}
	var env2 Environment
	if err := yaml.Unmarshal([]byte("name: s\nservice-mesh:\n  services:\n    saas/g: {label: {a: b}}\n"), &env2); err == nil || !strings.Contains(err.Error(), `unknown service-mesh service field "label"`) {
		t.Fatalf("a mistyped key is refused: %v", err)
	}
}

// The egress group a service receives is its own declaration, every port
// explicit and sorted, the list the cell file carries for it; a service the
// environment declares nothing for receives the group with HOSTS empty.
func TestEgressGroupCarriesExactlyTheServicesDeclaredHosts(t *testing.T) {
	env := decodeEnvironment(t, `name: staging
egress:
  shop/api: {hosts: [identity.example.test, {name: api.github.com, port: 8443}]}
  shop/web: {hosts: [cdn.example.test]}
`)
	hostsOf := func(env *Environment, module, service string) string {
		t.Helper()
		group := env.EgressGroup(module, service)
		if len(group.Infos) != 1 || group.Infos[0].Name != EgressConfiguration || len(group.Infos[0].ConfigurationValues) != 1 {
			t.Fatalf("group = %+v", group)
		}
		value := group.Infos[0].ConfigurationValues[0]
		if value.Key != EgressHostsKey || value.Secret {
			t.Fatalf("value = %+v", value)
		}
		return value.Value
	}
	if got := hostsOf(env, "shop", "api"); got != "api.github.com:8443,identity.example.test:443" {
		t.Fatalf("shop/api HOSTS = %q", got)
	}
	if got := hostsOf(env, "shop", "web"); got != "cdn.example.test:443" {
		t.Fatalf("shop/web HOSTS = %q", got)
	}
	for _, other := range [][2]string{{"shop", "worker"}, {"edge", "api"}} {
		if got := hostsOf(env, other[0], other[1]); got != "" {
			t.Fatalf("%s/%s declares no egress, HOSTS = %q", other[0], other[1], got)
		}
	}
	var none *Environment
	if got := hostsOf(none, "shop", "api"); got != "" {
		t.Fatalf("no environment, HOSTS = %q", got)
	}
}
