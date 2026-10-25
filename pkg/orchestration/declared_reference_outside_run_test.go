package orchestration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codefly-dev/core/configurations"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
	"github.com/stretchr/testify/require"
)

// declaredReferenceWorkspace is referenceValidityWorkspace with the group
// DECLARED: payments/worker names `upstreams` under
// workspace-configuration-dependencies, and the group carries one reference the
// test chooses beside a literal. A declared group is judged against the plan's
// graph first, which is the half a run of fewer services than the composition
// used to fail.
func declaredReferenceWorkspace(t *testing.T, reference, producerVisibility string) *resources.Workspace {
	t.Helper()
	return writeTempWorkspace(t, map[string]string{
		"workspace.codefly.yaml": "name: boundary\nlayout: modules\nmodules:\n    - name: platform\n    - name: payments\n",
		"modules/platform/module.codefly.yaml": "kind: module\nname: platform\n" +
			"services:\n    - name: authority\n",
		"modules/platform/services/authority/service.codefly.yaml": "kind: service\nname: authority\nversion: 0.0.0\nmodule: platform\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"endpoints:\n    - name: admin\n      api: rest\n      visibility: " + producerVisibility + "\n" + statedExposure(producerVisibility),
		"modules/payments/module.codefly.yaml": "kind: module\nname: payments\n" +
			"services:\n    - name: worker\n",
		"modules/payments/services/worker/service.codefly.yaml": "kind: service\nname: worker\nversion: 0.0.0\nmodule: payments\n" +
			"agent:\n    kind: runtime::service\n    name: go-grpc\n    version: 0.0.16\n    publisher: codefly.ai\n" +
			"workspace-configuration-dependencies:\n    - upstreams\n",
		"configurations/local/upstreams.env": "authority-endpoint=${endpoint:" + reference + "}\nregion=local\n",
	})
}

// declaredReferenceFlow is the real Flow `codefly run service payments/worker`
// builds, with any options the case adds (a module closure, say).
func declaredReferenceFlow(t *testing.T, workspace *resources.Workspace, options ...FlowOption) *Flow {
	t.Helper()
	ctx := context.Background()
	env, err := SelectEnvironment(workspace, LocalEnvironmentName)
	require.NoError(t, err)
	payments, err := workspace.LoadModuleFromName(ctx, "payments")
	require.NoError(t, err)
	worker, err := payments.LoadServiceFromName(ctx, "worker")
	require.NoError(t, err)
	flow, err := NewFlow(ctx, workspace, payments, worker, env, RunMode, options...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = flow.Stop() })
	return flow
}

// (a) A run of fewer services than the composition is not refused over a
// declared group's reference to a service the workspace composes but this run
// does not contain.
//
// A module-closure run's graph holds the seeds' modules and the modules their
// service dependencies reach; a configuration reference reaches none, so a
// producer only a value names is outside it. The plan gate judged declared
// groups against that graph alone and reported the producer as "not a service
// of this workspace" — a real service, with the wrong fault and no way out short
// of running more than was asked. Rolling declaredProducerLookup back to the
// graph alone fails this test.
func TestAModuleClosureRunDoesNotRefuseADeclaredReferenceToAComposedServiceOutsideIt(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	flow := declaredReferenceFlow(t,
		declaredReferenceWorkspace(t, "platform/authority/admin", "public"), WithRunModuleClosure("payments"))

	// The trap is set: without this the test would assert the absence of a
	// refusal nothing could have produced.
	_, err := flow.world.Dependencies.ServiceFromUnique("platform/authority")
	require.Error(t, err, "the closure must not contain the producer")

	err = flow.InitManagers(context.Background())
	var unresolved *configurations.UnresolvedReferencesError
	require.False(t, errors.As(err, &unresolved),
		"a composed producer outside this run must not refuse it: %v", err)
}

// (a), the other half: the resolution drops that value with a WARN naming the
// consumer, the value, which reference of it, and the producer — and delivers
// the rest of the group. The drop is what the run owes; the warning is what
// keeps it from being silent.
func TestAResolutionDropsADeclaredReferenceToAProducerOutsideTheRunWithAWarning(t *testing.T) {
	workspace := declaredReferenceWorkspace(t, "platform/authority/admin", "public")
	world, service := referenceValidityWorld(t, workspace, func(world *World) { world.Mode = RunMode })

	recorder := &warnRecorder{}
	ctx := wool.New(context.Background(), &wool.Resource{Kind: "test", Unique: "test"}).
		WithLogger(recorder).Inject(context.Background())
	confs, err := world.workspaceConfigurationsFor(ctx, service, nil, resources.NewContainerNetworkAccess())
	require.NoError(t, err, "a producer outside the run is a drop in a run, not a refusal")
	_, delivered := groupValue(confs, "upstreams", "authority-endpoint")
	require.False(t, delivered, "no local address exists for a producer outside the run")
	region, delivered := groupValue(confs, "upstreams", "region")
	require.True(t, delivered, "the rest of the group still arrives")
	require.Equal(t, "local", region)

	warning := recorder.find("its producer is not part of this run")
	require.NotNil(t, warning, "the drop must be warned about, got %v", recorder.messages())
	require.Equal(t, "upstreams/authority-endpoint", warning["value"])
	require.Equal(t, "platform/authority", warning["producer"])
	require.Equal(t, "payments/worker", warning["consumer"])
	require.Equal(t, "reference 1 of 1", warning["reference"])
}

// (b) A producer the workspace does not have is still refused, by name of the
// value and position, in the same module-closure run: relaxing membership must
// not turn a typo into a quiet drop.
func TestAModuleClosureRunStillRefusesADeclaredReferenceToAProducerTheWorkspaceLacks(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	flow := declaredReferenceFlow(t,
		declaredReferenceWorkspace(t, "platfrom/authority/admin", "public"), WithRunModuleClosure("payments"))

	err := flow.InitManagers(context.Background())
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "a typo'd producer must refuse the run, got %v", err)
	require.Len(t, unresolved.References, 1)
	require.Equal(t, "upstreams", unresolved.References[0].Group)
	require.Equal(t, "authority-endpoint", unresolved.References[0].Key)
	require.Contains(t, unresolved.References[0].Reason, "not a service of this workspace")
	require.NotContains(t, unresolved.References[0].Reason, "platfrom", "a diagnostic must not echo the value")
	require.Empty(t, flow.hub.managers, "no service of the run set was created")
}

// Only membership is relaxed: a producer outside the run is still held to its
// export boundary, so a private endpoint of another module is refused exactly
// as it is for a producer inside the run.
func TestAModuleClosureRunStillHoldsAnOutsideProducerToItsExportBoundary(t *testing.T) {
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	flow := declaredReferenceFlow(t,
		declaredReferenceWorkspace(t, "platform/authority/admin", "private"), WithRunModuleClosure("payments"))

	err := flow.InitManagers(context.Background())
	var unresolved *configurations.UnresolvedReferencesError
	require.True(t, errors.As(err, &unresolved), "a private endpoint must refuse the run, got %v", err)
	require.Contains(t, unresolved.References[0].Reason, "is private to module")
}

// The excluded-dependency case keeps refusing: excluding a producer
// (--exclude-dependency, a run profile) while a group this run receives still
// references it is the coupling the Codefly guide documents, and the refusal
// names the group, the excluded producer and both ways out. It is not dropped
// like a producer the run simply does not contain — the exclusion is the
// operator's own statement, and the group is theirs to exclude with it.
func TestAnExcludedDependencyStillRefusesADeclaredReferenceToIt(t *testing.T) {
	ctx := context.Background()
	t.Setenv(resources.CodeflyHomeEnv, filepath.Join(t.TempDir(), "home"))
	workspace := declaredReferenceWorkspace(t, "platform/authority/admin", "public")

	for name, options := range map[string][]FlowOption{
		"whole workspace": nil,
		"module closure":  {WithRunModuleClosure("payments")},
	} {
		t.Run(name, func(t *testing.T) {
			profile, err := workspace.ResolveRunProfile(ctx, "", resources.RunProfile{ExcludeDependencies: []string{"platform/authority"}})
			require.NoError(t, err)
			flow := declaredReferenceFlow(t, workspace, options...)
			require.NoError(t, flow.WithRunProfile(profile))

			err = flow.InitManagers(ctx)
			var unresolved *configurations.UnresolvedReferencesError
			require.True(t, errors.As(err, &unresolved), "want the plan-time refusal, got %v", err)
			require.Len(t, unresolved.References, 1)
			reason := unresolved.References[0].Reason
			require.Contains(t, reason, "excluded from this run")
			require.Contains(t, reason, "platform/authority")
			require.Contains(t, reason, `exclude the "upstreams" workspace configuration too`)
			require.Empty(t, flow.hub.managers, "no service of the run set was created")

			// Excluding the group as the refusal says clears the check.
			flow = declaredReferenceFlow(t, workspace, options...)
			require.NoError(t, flow.WithRunProfile(resources.RunProfile{
				ExcludeDependencies:            []string{"platform/authority"},
				ExcludeWorkspaceConfigurations: []string{"upstreams"},
			}))
			err = flow.InitManagers(ctx)
			require.False(t, errors.As(err, &unresolved), "excluding the group too clears the reference check, got %v", err)
		})
	}
}

// warnRecorder keeps every WARN line a resolution writes, with its fields.
type warnRecorder struct {
	mu   sync.Mutex
	logs []*wool.Log
}

func (r *warnRecorder) Process(msg *wool.Log) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if msg.Level == wool.WARN {
		r.logs = append(r.logs, msg)
	}
}

// find returns the fields of the first WARN whose message contains text.
func (r *warnRecorder) find(text string) map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, log := range r.logs {
		if !strings.Contains(log.Message, text) {
			continue
		}
		fields := map[string]string{}
		for _, field := range log.Fields {
			fields[field.Key] = fmt.Sprint(field.Value)
		}
		return fields
	}
	return nil
}

func (r *warnRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.logs))
	for _, log := range r.logs {
		out = append(out, log.Message)
	}
	return out
}
