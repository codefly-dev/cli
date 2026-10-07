package orchestration

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// answerWorkspace is one producer with the endpoints given — each a (name, api)
// pair, which is the whole input to core's matcher — and one consumer holding a
// reference to each name listed.
func answerWorkspace(t *testing.T, endpoints [][2]string, references []string) *resources.Workspace {
	t.Helper()
	declared := "endpoints:\n"
	for _, endpoint := range endpoints {
		declared += "    - name: " + endpoint[0] + "\n      api: " + endpoint[1] + "\n      visibility: public\n      exposure: none\n"
	}
	values := ""
	for index, reference := range references {
		values += fmt.Sprintf("v%d=${endpoint:platform/authority/%s}\n", index, reference)
	}
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/platform\nservices:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" + declared,
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\nproject: boundary\n" +
			"domain: github.com/codefly-ai/boundary/payments\nservices:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n",
		"configurations/local/work-context.env": values,
	})
}

// Dropping a producer's wrong answers for one reference must not leave a SECOND
// reference holding nothing but wrong answers of its own.
//
// The composition: `grpc` has api `probe`, `probe` has api `metrics`, and
// `metrics` — which a reference names — published no mapping at all.
//
//   - The reference naming `metrics` finds its endpoint unbound and a sibling,
//     `probe` (api `metrics`), that would answer it instead. No ordering helps,
//     so `probe` is unbound for this producer: the reference is answered by
//     nothing rather than by the wrong endpoint. That rule is right on its own.
//   - But `probe` is named EXACTLY by another reference. With `probe` gone, the
//     only mapping left matching `${…/probe}` is `grpc`, whose api is `probe` —
//     and core's first match wrote grpc's address into a value that named
//     `probe`. No error, no warning, and the outcome guard saw the key present.
//
// Each rule is correct in isolation and the pair is not, which is why the pass
// now verifies the list it is about to hand over instead of reasoning about it.
// A random search over small compositions found this one; no hand-written
// fixture had reached it. (Layer-4 round-twelve.)
func TestDroppingOneReferencesWrongAnswersCannotStrandAnother(t *testing.T) {
	ctx := context.Background()
	world, worker := referenceValidityWorld(t,
		answerWorkspace(t,
			[][2]string{{"grpc", "probe"}, {"metrics", "metrics"}, {"probe", "metrics"}},
			[]string{"grpc", "metrics", "probe"}),
		func(world *World) {
			world.Mode = RunMode
			recordMappings(t, world, "platform", "authority",
				endpointMapping("platform", "authority", "grpc", "probe", "public", nativeInstance("http://localhost:1111")),
				// `metrics` publishes nothing, which is what sets the drop off.
				endpointMapping("platform", "authority", "probe", "metrics", "public", nativeInstance("http://localhost:4444")),
			)
		})

	confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
	if err != nil {
		// Refusing is the honest outcome here: `grpc` would answer the
		// reference naming `probe`, and `grpc` cannot be unbound because a
		// reference names it exactly. No one list serves both.
		require.Contains(t, err.Error(), "${endpoint:platform/authority/probe}")
		require.Contains(t, err.Error(), `"grpc"`)
		require.NotContains(t, err.Error(), "localhost")
		return
	}
	for index, reference := range []string{"grpc", "metrics", "probe"} {
		value, delivered := groupValue(confs, "work-context", fmt.Sprintf("v%d", index))
		if !delivered || value == "" {
			continue
		}
		require.Equal(t, map[string]string{
			"grpc": "http://localhost:1111", "metrics": "", "probe": "http://localhost:4444",
		}[reference], value, "the value for ${endpoint:…/"+reference+"} addressed another endpoint")
	}
}

// No composition of one producer's endpoints delivers an address the reference
// did not name.
//
// This is the property every hand-written fixture in this package is one case
// of, and it is here because prediction kept failing: each review round found
// another arrangement where the pass reasoned correctly about its own rule and
// wrongly about the combination. The search is deterministic — one seed, the
// same compositions every run — so a failure is reproducible and names the
// endpoints, the APIs, the published instances and the references that produced
// it.
//
// `mode` is what the producer published for an endpoint: an address for this
// access, an EMPTY address for it (core stops there and errors, which is not
// the same as absence), an address for the other access only, or no mapping at
// all. Those four are the whole input space of the access rules.
func TestNoCompositionAnswersAReferenceWithAnEndpointItDidNotName(t *testing.T) {
	ctx := context.Background()
	const (
		modeAddress = iota
		modeEmptyAddress
		modeOtherAccess
		modeUnpublished
	)
	names := []string{"grpc", "admin", "metrics", "probe"}
	address := map[string]string{
		"grpc": "http://localhost:1111", "admin": "http://localhost:2222",
		"metrics": "http://localhost:3333", "probe": "http://localhost:4444",
	}
	random := rand.New(rand.NewSource(20261004))

	for iteration := 0; iteration < 1200; iteration++ {
		var endpoints [][2]string
		modes := map[string]int{}
		for _, index := range random.Perm(len(names))[:2+random.Intn(3)] {
			name := names[index]
			endpoints = append(endpoints, [2]string{name, names[random.Intn(len(names))]})
			modes[name] = random.Intn(4)
		}
		var references []string
		for _, endpoint := range endpoints {
			if random.Intn(2) == 0 {
				references = append(references, endpoint[0])
			}
		}
		if len(references) == 0 {
			continue
		}

		var recorded []*basev0.NetworkMapping
		for _, endpoint := range endpoints {
			name, api := endpoint[0], endpoint[1]
			switch modes[name] {
			case modeAddress:
				recorded = append(recorded, endpointMapping("platform", "authority", name, api, "public", nativeInstance(address[name])))
			case modeEmptyAddress:
				recorded = append(recorded, endpointMapping("platform", "authority", name, api, "public", nativeInstance("")))
			case modeOtherAccess:
				recorded = append(recorded, endpointMapping("platform", "authority", name, api, "public", containerInstance("http://"+name+":9090")))
			case modeUnpublished:
			}
		}

		world, worker := referenceValidityWorld(t, answerWorkspace(t, endpoints, references),
			func(world *World) {
				world.Mode = RunMode
				if len(recorded) > 0 {
					recordMappings(t, world, "platform", "authority", recorded...)
				}
			})
		confs, err := world.workspaceConfigurationsFor(ctx, worker, nil, resources.NewNativeNetworkAccess())
		if err != nil {
			// A refusal is an acceptable answer: it delivers nothing.
			continue
		}
		for index, reference := range references {
			value, delivered := groupValue(confs, "work-context", fmt.Sprintf("v%d", index))
			if !delivered || value == "" {
				continue
			}
			if value == address[reference] {
				continue
			}
			var described []string
			for _, endpoint := range endpoints {
				described = append(described, fmt.Sprintf("%s(api %s, mode %d)", endpoint[0], endpoint[1], modes[endpoint[0]]))
			}
			t.Fatalf("iteration %d: ${endpoint:platform/authority/%s} resolved to %q, which is not that endpoint's address %q\nendpoints: %s\nreferences: %v",
				iteration, reference, value, address[reference], strings.Join(described, ", "), references)
		}
	}
}
