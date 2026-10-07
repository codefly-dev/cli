package environments

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
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
		// The delivery API is named by composition identity, never by address:
		// absent, or a bare address, is a host block the publish cannot route
		// a document through.
		"no delivery":           {yaml: without("delivery"), want: "host delivery"},
		"bare delivery address": {yaml: strings.Replace(hostBlock, "delivery: platform/accounts/rest", "delivery: delivery.example.test:443", 1), want: "host delivery"},
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
