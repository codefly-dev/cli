package orchestration

import (
	"context"
	"testing"

	basev0 "github.com/codefly-dev/core/generated/go/codefly/base/v0"
	"github.com/codefly-dev/core/resources"
	"github.com/stretchr/testify/require"
)

// droppedValueWorld resolves one composition-root group over the group-parity
// composition, with the root group's reference and the consumer's bound mappings
// chosen by the caller. The World is bound the way NewFlow binds one, including
// the loaded configurations, so the resolution can both validate a reference and
// notice a value that did not survive.
func droppedValueWorld(t *testing.T, mode Mode, reference string, mappings []*basev0.NetworkMapping) ([]*basev0.Configuration, error) {
	t.Helper()
	world, workspace := parityWorld(t, mode)
	loader := staticWorkspaceLoader{
		confs: []*basev0.Configuration{{
			Origin: resources.ConfigurationWorkspace,
			Infos: []*basev0.ConfigurationInformation{{
				Name: "work-context",
				ConfigurationValues: []*basev0.ConfigurationValue{
					// A literal beside the reference, so the group keeps its name
					// even when every reference of it is dropped: that is what
					// makes the loss invisible to an assertion over group names.
					{Key: "literal", Value: "present"},
					{Key: "authority-endpoint", Value: reference},
				},
			}},
		}},
		rootConfigs: []string{"work-context"},
	}
	world.ConfigurationManager = loadedWorkspaceManager(t, loader)
	world.compositionRootGroups = loader.CompositionRootWorkspaceConfigurationNames
	world.providedWorkspaceConfigurationInfos = func() []*basev0.ConfigurationInformation {
		return workspaceConfigurationInfos(loader.Configurations())
	}
	// The run set a real flow of this origin computes: payments/worker alone. A
	// root group's reference adds no edge to the closure, so the producer it
	// names is NOT in the run set — which is the topology every render has, and
	// the one a hand-set run set hid.
	world.setRunProducers([]string{"payments/worker"}, nil)

	service := parityService(t, workspace, "payments", "worker")
	instance := parityInstance(t, workspace, service)
	if mode == RunMode {
		runtimeContext, err := resources.NewRuntimeContext(resources.RuntimeContextNative)
		require.NoError(t, err)
		runner := &Runner{world: world, instance: instance, runtimeContext: resources.RuntimeContextNative}
		return runner.workspaceConfigurations(context.Background(), mappings, runtimeContext)
	}
	builder := &Builder{world: world, instance: instance}
	return builder.workspaceConfigurations(context.Background(), mappings)
}

// A render refuses a root value that did not survive resolution, whatever made
// it not survive.
//
// These are the shapes that defeated the earlier revision, which asked "do the
// bound mappings carry an endpoint with this identity?" and treated yes as
// resolution. All three answered yes while core dropped the value and returned
// no error, so the render emitted a manifest with the value simply absent —
// `delivered=false, err=nil`, the exact fault cli#882 is about:
//
//   - a mapping for the right endpoint with no instance for the consumer's
//     network access at all;
//   - an instance for that access whose address is empty;
//   - a reference missing its endpoint component, which identity matching never
//     even looked at.
//
// The fix does not predict core's interpolation; it reads the outcome. So a
// fourth shape nobody has thought of yet is covered by the same assertion.
func TestARenderRefusesARootValueThatDidNotSurviveResolution(t *testing.T) {
	endpoint := &basev0.Endpoint{Module: "platform", Service: "authority", Name: "rest", Api: "rest"}
	for _, test := range []struct {
		name, reference string
		mappings        []*basev0.NetworkMapping
	}{
		{
			"a mapping with no instance for the consumer's access",
			"${endpoint:platform/authority/rest}",
			[]*basev0.NetworkMapping{{Endpoint: endpoint}},
		},
		{
			"an instance whose address is empty",
			"${endpoint:platform/authority/rest}",
			[]*basev0.NetworkMapping{{Endpoint: endpoint, Instances: []*basev0.NetworkInstance{
				{Access: resources.NewContainerNetworkAccess(), Address: ""},
			}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			confs, err := droppedValueWorld(t, SnapshotMode, test.reference, test.mappings)
			require.Error(t, err, "a render must not emit a manifest with a root value silently absent")
			// Refused by CORE now, at the interpolation, rather than by this
			// package's outcome check afterwards: since core v0.11.0 an
			// endpoint with no instance for this access, and an instance whose
			// address is empty, are faults of the composition and not
			// omissions this consumer is entitled to. The value never reaches
			// the outcome check, so there is nothing to ask `confs` about.
			require.Nil(t, confs)
			require.Contains(t, err.Error(), "work-context/authority-endpoint")
			require.NotContains(t, err.Error(), "${endpoint:", "a diagnostic must not echo the value")
		})
	}
}

// A reference missing its endpoint component is refused earlier still, as
// malformed, by core's own check over the effective set — before any mapping is
// consulted. Identity matching could not see it at all.
func TestARootReferenceMissingItsEndpointComponentIsRefusedAsMalformed(t *testing.T) {
	_, err := droppedValueWorld(t, SnapshotMode, "${endpoint:platform/authority}", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "malformed reference")
}

// The same dropped value under `codefly run` is not refused: it is dropped, and
// warned about.
//
// This tolerance also held before the PR, by accident rather than by rule — the
// old code dropped the value silently — so the test survives a wholesale
// The render/run asymmetry this used to assert is GONE, by core's decision
// (core#703, recorded there and in its release notes): a faulty reference in a
// composition-root group fails every receiver, in a run as in a render. The
// case this package once had to tolerate — a run legitimately smaller or
// earlier than the composition — is now a fault core refuses, and an endpoint
// with no instance for this consumer's access is one of them. This test is kept,
// flipped, so the change is recorded rather than deleted.
func TestARunRefusesARootValueWhoseEndpointHasNoInstanceForItsAccess(t *testing.T) {
	_, err := droppedValueWorld(t, RunMode, "${endpoint:platform/authority/rest}",
		[]*basev0.NetworkMapping{{Endpoint: &basev0.Endpoint{Module: "platform", Service: "authority", Name: "rest", Api: "rest"}}})
	require.Error(t, err,
		"a composition fault in a root group fails every receiver, in a run as in a render")
	require.Contains(t, err.Error(), "work-context/authority-endpoint")
	require.Contains(t, err.Error(), "no instance for access")
}

// A World that resolves groups carrying references but cannot say what they
// were loaded from refuses, rather than validating nothing and noticing no
// drop. Both guards above are reads of that binding, so an unbound one would
// turn each of them into a silent no-op — which is how the first revision of
// this fix passed its own tests.
func TestAWorldThatCannotValidateItsReferencesRefusesToResolveThem(t *testing.T) {
	loader := staticWorkspaceLoader{
		confs:       []*basev0.Configuration{workspaceConfiguration("work-context", "authority-endpoint", "${endpoint:platform/authority/rest}")},
		rootConfigs: []string{"work-context"},
	}
	world := &World{
		Mode:                  SnapshotMode,
		ConfigurationManager:  loadedWorkspaceManager(t, loader),
		compositionRootGroups: loader.CompositionRootWorkspaceConfigurationNames,
	}
	world.setRunProducers([]string{"platform/authority"}, nil)

	_, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewContainerNetworkAccess())
	require.Error(t, err, "a world that cannot validate a reference must not deliver it unchecked")
	require.Contains(t, err.Error(), "providedWorkspaceConfigurationInfos")

	// A group with no reference at all needs no validation, so it still resolves.
	plain := staticWorkspaceLoader{
		confs:       []*basev0.Configuration{workspaceConfiguration("work-context", "authority-url", "https://authority.example")},
		rootConfigs: []string{"work-context"},
	}
	world.ConfigurationManager = loadedWorkspaceManager(t, plain)
	world.compositionRootGroups = plain.CompositionRootWorkspaceConfigurationNames
	confs, err := world.workspaceConfigurationsFor(context.Background(), &resources.Service{}, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err)
	require.Equal(t, []string{"work-context"}, groupSet(confs))
}

// The blocker this round found, pinned: a render refuses a lost root value even
// though the producer is NOT in the render's run set — because a real render
// never puts it there.
//
// A render flow covers one root service and its build closure
// (Flow.managerDependencies → Dependencies.Restrict), and a root group's
// reference adds no edge to that closure. So the producer a root group names is
// outside the run set of every render that does not happen to be rendering it.
// An earlier revision gated the render's refusal on run membership, which meant
// the refusal could not fire for the #882 case at all: another module's
// producer, named by a root group, value quietly absent from the manifest, two
// WARN lines. The tests of that revision passed only because they hand-set the
// producer into the run set.
//
// The render therefore judges by WORKSPACE membership: a deployed address is a
// pure function of identity and namespace, so it exists for any service the
// workspace has, whether or not this flow covers it.
func TestARenderRefusesALostRootValueWhoseProducerIsOutsideItsRunSet(t *testing.T) {
	ctx := context.Background()
	world, workspace := parityWorld(t, SnapshotMode, "payments/worker")
	require.Equal(t, []string{"payments/worker"},
		parityRunClosure(t, world.Dependencies, workspace, []string{"payments/worker"}),
		"the premise: a render of one service, whose closure does not contain platform/authority")
	require.False(t, world.producerInRun()("platform/authority"),
		"the producer a root group names is outside this render's run set, as it is in every real render")

	// Nothing can derive a deployed address here, so the root group's reference
	// cannot survive — the shape the render must refuse rather than ship.
	world.RemoteNetworkManager = nil

	service := parityService(t, workspace, "payments", "worker")
	builder := &Builder{world: world, instance: parityInstance(t, workspace, service)}
	_, err := builder.workspaceConfigurations(ctx, nil)
	require.Error(t, err, "a render must refuse a lost root value, run-set membership notwithstanding")
	require.Contains(t, err.Error(), "work-context/authority-endpoint")
	require.Contains(t, err.Error(), "reference 1 of 1", "located by position")
	require.Contains(t, err.Error(), "a service of this workspace")
	// The canary: a reference is text from a value and a value may be a
	// secret, so no diagnostic reconstructs one.
	require.NotContains(t, err.Error(), "${endpoint:")
}

// A producer the workspace does not have is refused outright, before the
// outcome is ever examined — so the render's "did it survive" rule never sees
// that case, and the rule itself stays "the workspace has it".
//
// It is refused by core's verdict rather than dropped, because a drop here would
// depend on the plan gate having run over the same values, and an
// invocation-scoped override reaches the resolution without the gate having
// seen it.
func TestARenderRefusesARootValueWhoseProducerTheWorkspaceLacks(t *testing.T) {
	_, err := droppedValueWorld(t, SnapshotMode, "${endpoint:absent/service/rest}", nil)
	require.Error(t, err, "a reference naming a service the workspace does not have must be refused")
	require.Contains(t, err.Error(), "not a service of this workspace")
}
