package deploysecrets

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/solutionrun"
)

// Payloads are generated in memory. Assertions never print them, even on failure.
func testPayload() string { return solutionrun.MintCredential() }

func safetyRemote(name string, properties ...gitops.RenderedSecretProperty) gitops.RenderedServiceSecret {
	return gitops.RenderedServiceSecret{RemoteKey: name, Services: []string{"app/api"}, Properties: properties}
}

func safetyBuild(store *fakeStore, remotes []gitops.RenderedServiceSecret, generators []environments.EnvironmentSecretGenerator, federation solutionrun.DeployedSecrets) (*Plan, error) {
	return Build(context.Background(), &Inputs{Store: store, ReadPayloads: true, Rendered: gitops.RenderedEnvironment{Secrets: remotes}, Generators: generators, Federation: federation})
}

func safetyFederation(encoded, digest bool) solutionrun.DeployedSecrets {
	return solutionrun.DeployedSecrets{Services: map[string]map[string]solutionrun.SecretDerivation{
		"app/api": {registrations: {Credentials: []solutionrun.Credential{{Kind: solutionrun.ModuleRegistration, Identity: "module"}}, Encoded: encoded, Digest: digest}},
	}}
}

func TestStoredIdentityNeverReachesPublicPlan(t *testing.T) {
	marker := testPayload()
	store := newFakeStore(map[string]map[string]string{"one": {"mapped": marker + ":" + testPayload()}})
	plan, err := safetyBuild(store, []gitops.RenderedServiceSecret{safetyRemote("one", mapped("mapped", registrations))}, nil, safetyFederation(true, false))
	if err != nil {
		t.Fatal("could not build the refusal plan")
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal("could not encode public plan")
	}
	if strings.Contains(string(encoded), marker) {
		t.Fatal("stored identity reached public plan")
	}
	if _, err = plan.Apply(context.Background(), store, false); err == nil || strings.Contains(err.Error(), marker) {
		t.Fatal("apply must refuse without exposing stored content")
	}
	if len(store.writes) != 0 {
		t.Fatal("refusal wrote to the store")
	}
}

func TestInvalidExistingFederationValueNeverRotates(t *testing.T) {
	cases := []struct {
		name   string
		value  func() string
		digest bool
	}{
		{"unencoded", testPayload, false},
		{"empty entry", func() string { return "module:" }, false},
		{"missing identity", func() string { return ":" + testPayload() }, false},
		{"duplicate identity", func() string { return "module:" + testPayload() + ",module:" + testPayload() }, false},
		{"trailing separator", func() string { return "module:" + testPayload() + "," }, false},
		{"invalid digest", func() string { return "module:" + testPayload()[:20] }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := tc.value()
			store := newFakeStore(map[string]map[string]string{"one": {"mapped": value}})
			_, err := safetyBuild(store, []gitops.RenderedServiceSecret{safetyRemote("one", mapped("mapped", registrations))}, nil, safetyFederation(true, tc.digest))
			if err == nil {
				t.Fatal("invalid stored encoding was accepted")
			}
			if len(value) > 10 && strings.Contains(err.Error(), value) {
				t.Fatal("error disclosed stored content")
			}
			if store.documents["one"]["mapped"] != value || len(store.writes) != 0 {
				t.Fatal("invalid existing credential was changed")
			}
		})
	}
}

func TestValidMappedFederationMigrationIsIdempotent(t *testing.T) {
	for _, encoded := range []bool{false, true} {
		t.Run(fmt.Sprint(encoded), func(t *testing.T) {
			value := testPayload()
			if encoded {
				value = "module:" + value
			}
			store := newFakeStore(map[string]map[string]string{"one": {"mapped": value}})
			remotes := []gitops.RenderedServiceSecret{safetyRemote("one", mapped("mapped", registrations))}
			for range 2 {
				plan, err := safetyBuild(store, remotes, nil, safetyFederation(encoded, false))
				if err != nil {
					t.Fatal("valid existing federation value refused")
				}
				if _, err = plan.Apply(context.Background(), store, false); err != nil {
					t.Fatal("valid migration refused")
				}
				if len(plan.Changes()) != 0 || store.documents["one"]["mapped"] != value {
					t.Fatal("valid stored credential changed")
				}
			}
		})
	}
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
				plan, err := safetyBuild(store, remotes, aliasGenerators(), solutionrun.DeployedSecrets{})
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
				plan, err = safetyBuild(store, remotes, aliasGenerators(), solutionrun.DeployedSecrets{})
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
			_, err := safetyBuild(store, remotes, aliasGenerators(), solutionrun.DeployedSecrets{})
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

func TestSharedDocumentRetainsIndependentFederationBindings(t *testing.T) {
	key := solutionSecret
	properties := []gitops.RenderedSecretProperty{mapped("first", key), mapped("second", key)}
	properties[0].Readers = []gitops.RenderedSecretReader{{Service: "first/api", Key: key}}
	properties[1].Readers = []gitops.RenderedSecretReader{{Service: "second/api", Key: key}}
	remotes := []gitops.RenderedServiceSecret{{RemoteKey: "shared", Services: []string{"first/api", "second/api"}, Properties: properties}}
	federation := solutionrun.DeployedSecrets{Services: map[string]map[string]solutionrun.SecretDerivation{}}
	for _, name := range []string{"first", "second"} {
		federation.Services[name+"/api"] = map[string]solutionrun.SecretDerivation{key: {Credentials: []solutionrun.Credential{{Kind: solutionrun.SolutionRegistration, Identity: name}}}}
	}
	docs := map[string]map[string]string{"shared": {"first": testPayload(), "second": testPayload()}}
	before := maps.Clone(docs["shared"])
	store := newFakeStore(docs)
	plan, err := safetyBuild(store, remotes, nil, federation)
	if err != nil {
		t.Fatal("independent federation properties were conflated")
	}
	if _, err = plan.Apply(context.Background(), store, false); err != nil {
		t.Fatal("valid shared document refused")
	}
	if len(plan.Changes()) != 0 || !reflect.DeepEqual(before, store.documents["shared"]) {
		t.Fatal("existing independent credentials changed")
	}
}

func TestAliasCannotMixFederationAndConfiguredSources(t *testing.T) {
	for _, sameProperty := range []bool{true, false} {
		t.Run(fmt.Sprint(sameProperty), func(t *testing.T) {
			property := mapped("mapped", registrations, aliasKey("A"))
			remotes := []gitops.RenderedServiceSecret{safetyRemote("one", property)}
			if !sameProperty {
				key := solutionSecret
				remotes = []gitops.RenderedServiceSecret{safetyRemote("one", mapped("derived", key)), {RemoteKey: "two", Services: []string{"other/api"}, Properties: []gitops.RenderedSecretProperty{mapped("configured", key)}}}
			}
			federation := safetyFederation(false, false)
			if !sameProperty {
				federation.Services["app/api"][solutionSecret] = federation.Services["app/api"][registrations]
				delete(federation.Services["app/api"], registrations)
			}
			_, err := safetyBuild(newFakeStore(map[string]map[string]string{}), remotes, aliasGenerators(), federation)
			if err == nil {
				t.Fatal("mixed credential sources were accepted")
			}
		})
	}
}

func TestEmptyStoredPropertiesFailBeforePlanning(t *testing.T) {
	for _, value := range []string{"", " \n\t"} {
		store := newFakeStore(map[string]map[string]string{"one": {"empty": value}})
		_, err := safetyBuild(store, []gitops.RenderedServiceSecret{safetyRemote("one", mapped("empty", aliasKey("A"))), safetyRemote("two", mapped("missing", aliasKey("A")))}, aliasGenerators(), solutionrun.DeployedSecrets{})
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
			plan, err := safetyBuild(store, []gitops.RenderedServiceSecret{safetyRemote("one", mapped("absent", aliasKey("A")))}, nil, solutionrun.DeployedSecrets{})
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

func TestMissingReaderAssociationsAreRefusedWhenAmbiguous(t *testing.T) {
	for _, properties := range [][]gitops.RenderedSecretProperty{
		{mapped("first", solutionSecret), mapped("second", solutionSecret)},
		{mapped("shared", solutionSecret, registrations)},
	} {
		remote := gitops.RenderedServiceSecret{RemoteKey: "shared", Services: []string{"first/api", "second/api"}, Properties: properties}
		_, err := safetyBuild(newFakeStore(map[string]map[string]string{}), []gitops.RenderedServiceSecret{remote}, nil, solutionrun.DeployedSecrets{})
		if err == nil {
			t.Fatal("ambiguous service/key associations were inferred")
		}
	}
}
