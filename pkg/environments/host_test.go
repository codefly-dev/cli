package environments

import (
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
	"gopkg.in/yaml.v3"
)

// hostBlock is a complete host declaration, every field present. The tests
// below remove one at a time, so a field that stops being required is a test
// that stops failing.
const hostBlock = `      coordinate: example/prod/region-a
      component: platform-host
      domain: example-platform
      audience: accounts
      trust_domain: cluster.example
      envelope_revision: 3
      delivery: platform/accounts/rest
`

// TestWorkspaceHostDeclarationSurvivesLoad is the round trip that matters:
// `host` is not a field core's resources.Environment models, so it arrives
// through the inline extensions and must still reach the CLI's Environment.
// Without this, a declared host is silently dropped and every render declares
// no binding while looking correctly configured.
func TestWorkspaceHostDeclarationSurvivesLoad(t *testing.T) {
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte("name: example\nenvironments:\n  - name: prod\n    namespace: example\n    host:\n" + hostBlock))
	if err != nil {
		t.Fatal(err)
	}
	env, err := Select(workspace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if env.Host == nil {
		t.Fatal("the declared host did not survive workspace load")
	}
	want := EnvironmentHost{
		Coordinate: "example/prod/region-a", Component: "platform-host", Domain: "example-platform",
		Audience: "accounts", TrustDomain: "cluster.example", EnvelopeRevision: 3, Delivery: "platform/accounts/rest",
	}
	if *env.Host != want {
		t.Fatalf("host %+v, want %+v", *env.Host, want)
	}
	if got := env.Host.SPIFFEID("crm", "crm-backend"); got != "spiffe://cluster.example/ns/crm/sa/crm-backend" {
		t.Fatalf("SPIFFE ID %q", got)
	}
	if module, service, endpoint := env.Host.DeliveryEndpoint(); module != "platform" || service != "accounts" || endpoint != "rest" {
		t.Fatalf("delivery endpoint %s/%s/%s", module, service, endpoint)
	}
}

func TestEnvironmentWithoutAHostIsValid(t *testing.T) {
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte("name: example\nenvironments:\n  - name: prod\n    namespace: example\n"))
	if err != nil {
		t.Fatal(err)
	}
	env, err := Select(workspace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if env.Host != nil {
		t.Fatalf("an undeclared host resolved to %+v", env.Host)
	}
	if err = env.Host.Validate(); err != nil {
		t.Fatalf("an undeclared host is not a valid absence: %v", err)
	}
}

// TestHostDeclarationRefusesAPartialOrMalformedIdentity pins that every field
// is required together: a host block is one declaration, and a document
// rendered from half of one is a document the host cannot use.
func TestHostDeclarationRefusesAPartialOrMalformedIdentity(t *testing.T) {
	without := func(field string) string {
		var kept []string
		for _, line := range strings.Split(strings.TrimSuffix(hostBlock, "\n"), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), field+":") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n") + "\n"
	}
	for name, table := range map[string]struct {
		yaml string
		want string
	}{
		"no component":             {yaml: without("component"), want: "host component"},
		"no coordinate":            {yaml: without("coordinate"), want: "host coordinate"},
		"no domain":                {yaml: without("domain"), want: "host domain"},
		"no audience":              {yaml: without("audience"), want: "host audience"},
		"no trust domain":          {yaml: without("trust_domain"), want: "host trust_domain"},
		"no envelope revision":     {yaml: without("envelope_revision"), want: "envelope_revision"},
		"envelope revision zero":   {yaml: without("envelope_revision") + "      envelope_revision: 0\n", want: "envelope_revision"},
		"uppercase coordinate":     {yaml: strings.Replace(hostBlock, "example/prod/region-a", "Example/Prod", 1), want: "host coordinate"},
		"uppercase domain":         {yaml: strings.Replace(hostBlock, "example-platform", "Example", 1), want: "host domain"},
		"trust domain with scheme": {yaml: strings.Replace(hostBlock, "cluster.example", "spiffe://cluster.example", 1), want: "host trust_domain"},
		"trust domain with path":   {yaml: strings.Replace(hostBlock, "cluster.example", "cluster.example/ns", 1), want: "host trust_domain"},
		"unknown key":              {yaml: hostBlock + "      cordinate: typo\n", want: "unknown host field"},
		"whitespace audience":      {yaml: strings.Replace(hostBlock, "audience: accounts", "audience: \"a b\"", 1), want: "host audience"},
	} {
		t.Run(name, func(t *testing.T) {
			workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(
				"name: example\nenvironments:\n  - name: prod\n    namespace: example\n    host:\n" + table.yaml))
			if err == nil {
				_, err = Select(workspace, "prod")
			}
			if err == nil || !strings.Contains(err.Error(), table.want) {
				t.Fatalf("declaration was accepted or refused wrongly: %v (want %q)", err, table.want)
			}
		})
	}
}

// TestEgressIsADeclarationCarriedNotDerived pins the per-service egress
// declaration: module-qualified keys, bare host names meaning port 443 or
// {name, port} for another port, read back sorted with the port explicit.
func TestEgressIsADeclarationCarriedNotDerived(t *testing.T) {
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(`name: example
environments:
  - name: prod
    namespace: example
    egress:
      platform/accounts: {hosts: [identity.example.test, api.github.com, {name: smtp.example.test, port: 587}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	env, err := Select(workspace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	want := []EnvironmentEgressHost{{Name: "api.github.com", Port: 443}, {Name: "identity.example.test", Port: 443}, {Name: "smtp.example.test", Port: 587}}
	if got := env.EgressHosts("platform", "accounts"); !slices.Equal(got, want) {
		t.Fatalf("egress hosts %+v, want %+v", got, want)
	}
	if env.EgressHosts("platform", "frontend") != nil {
		t.Fatal("a service declaring no egress reaches nothing")
	}
	// A bare name round-trips as a bare name, so a re-serialized workspace keeps
	// the terse declaration; a declared port keeps its mapping.
	encoded, err := yaml.Marshal(env.Egress["platform/accounts"])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); !strings.Contains(got, "- identity.example.test\n") || !strings.Contains(got, "port: 587") {
		t.Fatalf("egress re-serialized as:\n%s", got)
	}
	for name, table := range map[string]struct{ yaml, want string }{
		"bare key":              {yaml: "      accounts: {hosts: [api.github.com]}\n", want: "module-qualified"},
		"a URL":                 {yaml: "      platform/accounts: {hosts: [https://api.github.com/v3]}\n", want: "bare DNS host name"},
		"a port in the name":    {yaml: "      platform/accounts: {hosts: [api.github.com:443]}\n", want: "bare DNS host name"},
		"no host":               {yaml: "      platform/accounts: {hosts: []}\n", want: "declares no host"},
		"an uppercase":          {yaml: "      platform/accounts: {hosts: [API.github.com]}\n", want: "bare DNS host name"},
		"a port out of range":   {yaml: "      platform/accounts: {hosts: [{name: smtp.example.test, port: 70000}]}\n", want: "is not a port"},
		"a host twice":          {yaml: "      platform/accounts: {hosts: [api.github.com, {name: api.github.com, port: 443}]}\n", want: "declares api.github.com:443 twice"},
		"an unknown host field": {yaml: "      platform/accounts: {hosts: [{name: api.github.com, protocol: tls}]}\n", want: "unknown egress host field"},
	} {
		t.Run(name, func(t *testing.T) {
			workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(
				"name: example\nenvironments:\n  - name: prod\n    namespace: example\n    egress:\n" + table.yaml))
			if err == nil {
				_, err = Select(workspace, "prod")
			}
			if err == nil || !strings.Contains(err.Error(), table.want) {
				t.Fatalf("declaration was accepted or refused wrongly: %v (want %q)", err, table.want)
			}
		})
	}
}

// TestCellGrantsAreADeclaration pins the per-service cell declaration: which
// cell-provided resources a service binds and whether it mints a cloud
// credential, module-qualified, read back sorted, and an entry that declares
// nothing refused.
func TestCellGrantsAreADeclaration(t *testing.T) {
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(`name: example
environments:
  - name: prod
    namespace: example
    cell:
      platform/accounts: {bindings: [vault, audit]}
      platform/model: {bindings: [model_gateway], cloud-identity: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	env, err := Select(workspace, "prod")
	if err != nil {
		t.Fatal(err)
	}
	accounts, declared := env.CellWorkload("platform", "accounts")
	if !declared || !slices.Equal(accounts.Bindings, []string{"audit", "vault"}) || accounts.CloudIdentity {
		t.Fatalf("accounts %+v (declared %v)", accounts, declared)
	}
	model, declared := env.CellWorkload("platform", "model")
	if !declared || !slices.Equal(model.Bindings, []string{"model_gateway"}) || !model.CloudIdentity {
		t.Fatalf("model %+v (declared %v)", model, declared)
	}
	if _, declared := env.CellWorkload("platform", "frontend"); declared {
		t.Fatal("a service with no entry has no grant")
	}
	for name, table := range map[string]struct{ yaml, want string }{
		"bare key":         {yaml: "      accounts: {bindings: [vault]}\n", want: "module-qualified"},
		"nothing declared": {yaml: "      platform/accounts: {}\n", want: "declares nothing"},
		"a binding twice":  {yaml: "      platform/accounts: {bindings: [vault, vault]}\n", want: "declared twice"},
		"an uppercase":     {yaml: "      platform/accounts: {bindings: [Vault]}\n", want: "lowercase resource name"},
	} {
		t.Run(name, func(t *testing.T) {
			workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(
				"name: example\nenvironments:\n  - name: prod\n    namespace: example\n    cell:\n" + table.yaml))
			if err == nil {
				_, err = Select(workspace, "prod")
			}
			if err == nil || !strings.Contains(err.Error(), table.want) {
				t.Fatalf("declaration was accepted or refused wrongly: %v (want %q)", err, table.want)
			}
		})
	}
}
