package environments

import (
	"reflect"
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
	if !reflect.DeepEqual(*env.Host, want) {
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
		// A developer identity a local host admits is an https issuer and
		// the subject its certificate names, each required, neither
		// misspelled, none twice.
		"developer without subject":       {yaml: hostBlock + "      developers:\n        - issuer: https://github.com/login/oauth\n", want: "host developer subject"},
		"developer with http issuer":      {yaml: hostBlock + "      developers:\n        - issuer: http://github.com/login/oauth\n          subject: dev@example.test\n", want: "host developer issuer"},
		"developer with unknown key":      {yaml: hostBlock + "      developers:\n        - issuer: https://github.com/login/oauth\n          email: dev@example.test\n", want: "unknown host.developers field"},
		"developer subject not an e-mail": {yaml: hostBlock + "      developers:\n        - issuer: https://github.com/login/oauth\n          subject: https://github.com/dev\n", want: "exact e-mail"},
		"developer wildcard subject":      {yaml: hostBlock + "      developers:\n        - issuer: https://github.com/login/oauth\n          subject: \"*@example.test\"\n", want: "exact e-mail"},
		"developer wildcard issuer":       {yaml: hostBlock + "      developers:\n        - issuer: https://*.example.test\n          subject: dev@example.test\n", want: "wildcard"},
		"relative trust anchor":           {yaml: hostBlock + "      trust_anchor: etc/trust\n", want: "trust_anchor"},
		"unclean trust anchor":            {yaml: hostBlock + "      trust_anchor: /etc//trust\n", want: "trust_anchor"},
		"developer declared twice":        {yaml: hostBlock + "      developers:\n        - issuer: https://github.com/login/oauth\n          subject: dev@example.test\n        - issuer: https://github.com/login/oauth\n          subject: dev@example.test\n", want: "declared twice"},
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

// TestHostDevelopersAreALocalClustersAllowlist: a developer identity is what a
// local host's allowlist names, so it loads on a k3d environment and is refused
// on any other, where a deployed host would refuse it anyway and the
// declaration would only carry a person's identity into a hosted render.
func TestHostDevelopersAreALocalClustersAllowlist(t *testing.T) {
	developers := "      developers:\n        - issuer: https://github.com/login/oauth\n          subject: dev@example.test\n"
	local, err := resources.LoadFromBytes[resources.Workspace]([]byte(
		"name: example\nenvironments:\n  - name: local\n    namespace: example\n    cluster:\n      kind: k3d\n    host:\n" + hostBlock + developers))
	if err != nil {
		t.Fatal(err)
	}
	env, err := Select(local, "local")
	if err != nil {
		t.Fatalf("a k3d environment's developers were refused: %v", err)
	}
	if want := []HostDeveloper{{Issuer: "https://github.com/login/oauth", Subject: "dev@example.test"}}; !reflect.DeepEqual(env.Host.Developers, want) {
		t.Fatalf("developers %+v, want %+v", env.Host.Developers, want)
	}
	hosted, err := resources.LoadFromBytes[resources.Workspace]([]byte(
		"name: example\nenvironments:\n  - name: prod\n    namespace: example\n    cluster:\n      kind: gke\n    host:\n" + hostBlock + developers))
	if err == nil {
		_, err = Select(hosted, "prod")
	}
	if err == nil || !strings.Contains(err.Error(), "only a local cluster's host admits") {
		t.Fatalf("a hosted environment declaring developers was not refused: %v", err)
	}
	anchored, err := resources.LoadFromBytes[resources.Workspace]([]byte(
		"name: example\nenvironments:\n  - name: prod\n    namespace: example\n    cluster:\n      kind: gke\n    host:\n" + hostBlock + "      trust_anchor: /etc/host/delivery-trust\n"))
	if err == nil {
		_, err = Select(anchored, "prod")
	}
	if err == nil || !strings.Contains(err.Error(), "only a local cluster run mounts") {
		t.Fatalf("a hosted environment stating a trust anchor was not refused: %v", err)
	}
}
