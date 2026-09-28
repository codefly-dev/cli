package environments

import (
	"strings"
	"testing"

	"github.com/codefly-dev/core/resources"
)

// TestWorkspaceHostDeclarationSurvivesLoad is the round trip that matters:
// `host` is not a field core's resources.Environment models, so it arrives
// through the inline extensions and must still reach the CLI's Environment.
// Without this, a declared host is silently dropped and every render declares
// no binding while looking correctly configured.
func TestWorkspaceHostDeclarationSurvivesLoad(t *testing.T) {
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(`name: obin
environments:
  - name: prod
    namespace: obin
    host:
      coordinate: obin/prod/eu-west-1
      component: saas-host
      audience: https://saas-host.obin.example
`))
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
	if env.Host.Coordinate != "obin/prod/eu-west-1" || env.Host.Component != "saas-host" {
		t.Fatalf("host %+v", env.Host)
	}
	if env.Host.Audience != "https://saas-host.obin.example" {
		t.Fatalf("host audience %q", env.Host.Audience)
	}
}

func TestEnvironmentWithoutAHostIsValid(t *testing.T) {
	workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte("name: obin\nenvironments:\n  - name: prod\n    namespace: obin\n"))
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

func TestHostDeclarationRefusesAPartialOrMalformedIdentity(t *testing.T) {
	for name, table := range map[string]struct {
		yaml string
		want string
	}{
		"no component": {
			yaml: "      coordinate: obin/prod/eu-west-1\n      audience: https://saas-host.obin.example\n",
			want: "host component",
		},
		"no coordinate": {
			yaml: "      component: saas-host\n      audience: https://saas-host.obin.example\n",
			want: "host coordinate",
		},
		"no audience": {
			yaml: "      coordinate: obin/prod/eu-west-1\n      component: saas-host\n",
			want: "host audience",
		},
		"uppercase coordinate": {
			yaml: "      coordinate: Obin/Prod\n      component: saas-host\n      audience: a\n",
			want: "host coordinate",
		},
		"unknown key": {
			yaml: "      coordinate: obin/prod\n      component: saas-host\n      audience: a\n      cordinate: typo\n",
			want: "unknown host field",
		},
		"whitespace audience": {
			yaml: "      coordinate: obin/prod\n      component: saas-host\n      audience: \"a b\"\n",
			want: "host audience",
		},
	} {
		t.Run(name, func(t *testing.T) {
			workspace, err := resources.LoadFromBytes[resources.Workspace]([]byte(
				"name: obin\nenvironments:\n  - name: prod\n    namespace: obin\n    host:\n" + table.yaml))
			if err == nil {
				_, err = Select(workspace, "prod")
			}
			if err == nil || !strings.Contains(err.Error(), table.want) {
				t.Fatalf("declaration was accepted or refused wrongly: %v (want %q)", err, table.want)
			}
		})
	}
}
