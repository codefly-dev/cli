package deploysecrets

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/secretgen"
)

// Payloads are generated in memory. Assertions never print them, even on failure.
func testPayload() string {
	value, err := secretgen.Generate(secretgen.FormatHex, 32)
	if err != nil {
		panic(err)
	}
	return value
}

func safetyRemote(name string, properties ...gitops.RenderedSecretProperty) gitops.RenderedServiceSecret {
	return gitops.RenderedServiceSecret{RemoteKey: name, Services: []string{"app/api"}, Properties: properties}
}

func safetyBuild(store *fakeStore, remotes []gitops.RenderedServiceSecret, generators []environments.EnvironmentSecretGenerator) (*Plan, error) {
	return Build(context.Background(), &Inputs{Store: store, ReadPayloads: true, Rendered: gitops.RenderedEnvironment{Secrets: remotes}, Generators: generators})
}

func aliasGenerators() []environments.EnvironmentSecretGenerator {
	return []environments.EnvironmentSecretGenerator{{Scope: environments.SecretGeneratorScopeWorkspace, Configuration: "auth", Keys: []string{"A", "B", "C"}}}
}
func aliasKey(key string) string { return "CODEFLY__WORKSPACE_SECRET_CONFIGURATION__AUTH__" + key }

func TestConfigurationAliasesShareOneGeneratedOrExistingValue(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("seeded=%t/reverse=%t", seeded, reverse), func(t *testing.T) {
				a, b, c := aliasKey("A"), aliasKey("B"), aliasKey("C")
				remotes := []gitops.RenderedServiceSecret{
					safetyRemote("one", mapped("a", a), mapped("ab", a, b)),
					safetyRemote("two", mapped("bc", b, c), mapped("c", c)),
				}
				if reverse {
					slices.Reverse(remotes)
					for i := range remotes {
						slices.Reverse(remotes[i].Properties)
						for j := range remotes[i].Properties {
							slices.Reverse(remotes[i].Properties[j].Keys)
						}
					}
				}
				docs := map[string]map[string]string{}
				existing := testPayload()
				if seeded {
					docs["two"] = map[string]string{"c": existing}
				}
				store := newFakeStore(docs)
				plan, err := safetyBuild(store, remotes, aliasGenerators())
				if err != nil {
					t.Fatal("consistent alias plan refused")
				}
				if _, err = plan.Apply(context.Background(), store, false); err != nil {
					t.Fatal("alias apply failed")
				}
				want := store.documents["one"]["a"]
				if want == "" || (seeded && want != existing) {
					t.Fatal("existing value was lost or generated value absent")
				}
				for _, r := range remotes {
					for _, p := range r.Properties {
						if store.documents[r.RemoteKey][p.Property] != want {
							t.Fatal("one configuration value diverged across aliases")
						}
					}
				}
				plan, err = safetyBuild(store, remotes, aliasGenerators())
				if err != nil || len(plan.Changes()) != 0 {
					t.Fatal("second alias plan is not idempotent")
				}
			})
		}
	}
}

func TestConfigurationAliasesRefuseEveryConflictingHolder(t *testing.T) {
	for _, split := range []bool{false, true} {
		t.Run(fmt.Sprint(split), func(t *testing.T) {
			a, b := aliasKey("A"), aliasKey("B")
			remotes := []gitops.RenderedServiceSecret{safetyRemote("one", mapped("first", a), mapped("second", a, b))}
			docs := map[string]map[string]string{"one": {"first": testPayload(), "second": testPayload()}}
			if split {
				remotes = append([]gitops.RenderedServiceSecret{safetyRemote("target", mapped("absent", b))}, remotes...)
			}
			store := newFakeStore(docs)
			_, err := safetyBuild(store, remotes, aliasGenerators())
			if err == nil {
				t.Fatal("conflicting holders were accepted")
			}
			if len(store.writes) != 0 {
				t.Fatal("conflicting plan wrote values")
			}
			for _, value := range docs["one"] {
				if strings.Contains(err.Error(), value) {
					t.Fatal("conflict error disclosed a value")
				}
			}
		})
	}
}

func TestEmptyStoredPropertiesFailBeforePlanning(t *testing.T) {
	for _, value := range []string{"", " \n\t"} {
		store := newFakeStore(map[string]map[string]string{"one": {"empty": value}})
		_, err := safetyBuild(store, []gitops.RenderedServiceSecret{safetyRemote("one", mapped("empty", aliasKey("A"))), safetyRemote("two", mapped("missing", aliasKey("A")))}, aliasGenerators())
		if err == nil || len(store.writes) != 0 {
			t.Fatal("empty value must refuse before any write")
		}
	}
}

func TestRequiredNoOpPlanCannotReportApplySuccess(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(fmt.Sprint(exists), func(t *testing.T) {
			docs := map[string]map[string]string{}
			if exists {
				docs["one"] = map[string]string{}
			}
			store := newFakeStore(docs)
			plan, err := safetyBuild(store, []gitops.RenderedServiceSecret{safetyRemote("one", mapped("absent", aliasKey("A")))}, nil)
			if err != nil || len(plan.Changes()) != 0 {
				t.Fatal("could not construct required no-op plan")
			}
			if plan.ValidateApply(false) == nil {
				t.Fatal("no-op validation accepted a missing value")
			}
			if _, err = plan.Apply(context.Background(), store, false); err == nil || len(store.writes) != 0 {
				t.Fatal("missing value apply did not refuse")
			}
			if err = plan.ValidateApply(true); err != nil {
				t.Fatal("explicit allow-missing should permit writing the remaining values")
			}
		})
	}
}
