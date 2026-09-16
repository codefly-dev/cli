package ci

import (
	"github.com/codefly-dev/core/resources"
	"testing"
)

func TestDisposableTestEnvironmentCannotReuseFixtureState(t *testing.T) {
	declared := &resources.Environment{Name: "local", NamingScope: "retained-fixture", Fixture: "controlled"}
	workspace := &resources.Workspace{Environments: []*resources.Environment{declared}}
	first, err := testEnvironment(workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := testEnvironment(workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []*resources.Environment{first, second} {
		if env.NamingScope == "" || env.NamingScope == declared.NamingScope {
			t.Fatalf("disposable flow could reach retained fixture: %q", env.NamingScope)
		}
		if env.Fixture != declared.Fixture {
			t.Fatal("fixture selection changed")
		}
	}
	if first.NamingScope == second.NamingScope {
		t.Fatal("independent test flows share a resource scope")
	}
	stable, err := testEnvironment(workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if stable.NamingScope != "retained-fixture" || declared.NamingScope != "retained-fixture" {
		t.Fatal("disposable selection mutated ordinary CI or the declared environment")
	}
}
